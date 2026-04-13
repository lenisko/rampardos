package services

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/lenisko/rampardos/internal/config"
)

// HTTPService handles all HTTP client operations
type HTTPService struct {
	generalClient  *http.Client
	downloadClient *http.Client // For large file downloads (no overall timeout)
}

var globalHTTPService *HTTPService

// InitHTTPService initializes the global HTTP service with config
func InitHTTPService(cfg *config.Config) {
	globalHTTPService = &HTTPService{
		generalClient: &http.Client{
			Timeout: cfg.HTTPTimeout, // 0 = unlimited
			Transport: &http.Transport{
				MaxIdleConns:        cfg.HTTPMaxConns,
				MaxIdleConnsPerHost: cfg.HTTPMaxConns,
				MaxConnsPerHost:     cfg.HTTPMaxConns,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		downloadClient: &http.Client{
			Timeout: 0, // No overall timeout for large files (100GB+)
			Transport: &http.Transport{
				ResponseHeaderTimeout: 30 * time.Second,
				MaxIdleConns:          cfg.HTTPMaxConns,
				MaxIdleConnsPerHost:   cfg.HTTPMaxConns,
				MaxConnsPerHost:       cfg.HTTPMaxConns,
				IdleConnTimeout:       90 * time.Second,
			},
		},
	}
}

// getClient returns the appropriate client for the given host.
func (s *HTTPService) getClient(_ string) *http.Client {
	return s.generalClient
}

// DownloadFile downloads a file from a URL and saves it to the specified path.
// timeout: timeout for initial response headers (0 = 30s default). Body read has no timeout for large files.
func DownloadFile(ctx context.Context, fromURL, toPath, expectedType string, timeout time.Duration) error {
	if globalHTTPService == nil {
		return fmt.Errorf("HTTP service not initialized")
	}

	parsedURL, err := url.Parse(fromURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	host := parsedURL.Host

	if timeout == 0 {
		timeout = 30 * time.Second
	}

	startTime := time.Now()
	GlobalMetrics.RecordHTTPClientRequest(host)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fromURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", "TileserverCache")

	// Use the shared download client (connection pooling, no overall timeout for large files)
	resp, err := globalHTTPService.downloadClient.Do(req)
	if err != nil {
		GlobalMetrics.RecordHTTPClientError(host)
		return fmt.Errorf("failed to download file: %w", err)
	}
	defer resp.Body.Close()

	duration := time.Since(startTime).Seconds()
	GlobalMetrics.RecordHTTPClientDuration(host, duration)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		GlobalMetrics.RecordHTTPClientError(host)
		GlobalMetrics.RecordError("http_client", fmt.Sprintf("status_%d", resp.StatusCode))
		return fmt.Errorf("failed to load file. Got %d", resp.StatusCode)
	}

	if expectedType != "" {
		contentType := resp.Header.Get("Content-Type")
		if contentType != "" && !hasPrefix(contentType, expectedType) {
			return fmt.Errorf("failed to load file. Got invalid type: %s", contentType)
		}
	}

	// Ensure directory exists
	dir := filepath.Dir(toPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	file, err := os.Create(toPath)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer file.Close()

	written, err := io.Copy(file, resp.Body)
	if err != nil {
		os.Remove(toPath)
		return fmt.Errorf("failed to write file: %w", err)
	}

	if written == 0 {
		os.Remove(toPath)
		return fmt.Errorf("failed to load file. Got empty data")
	}

	return nil
}

// HTTPGet performs a GET request and returns the response.
// Caller is responsible for closing the response body.
func HTTPGet(ctx context.Context, fromURL string, timeout time.Duration) (*http.Response, error) {
	if globalHTTPService == nil {
		return nil, fmt.Errorf("HTTP service not initialized")
	}

	parsedURL, err := url.Parse(fromURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	host := parsedURL.Host

	// Apply timeout if specified
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		_ = cancel // caller handles context
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fromURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "TileserverCache")

	client := globalHTTPService.getClient(host)
	return client.Do(req)
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
