package services

import (
	"context"
	"fmt"
	"io"
	"log/slog"
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

	// Write to a temp file then rename atomically, so concurrent
	// readers never see a partially-downloaded file.
	dir := filepath.Dir(toPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	file, err := os.CreateTemp(filepath.Dir(toPath), filepath.Base(toPath)+".tmp.*")
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}

	tmpName := file.Name()
	written, err := io.Copy(file, resp.Body)
	file.Close()
	if err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("failed to write file: %w", err)
	}

	if written == 0 {
		os.Remove(tmpName)
		return fmt.Errorf("failed to load file. Got empty data")
	}

	if err := os.Rename(tmpName, toPath); err != nil {
		os.Remove(tmpName)
		return err
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

// HTTPServiceInitialized reports whether the global HTTP service has been
// initialised. Useful in tests to avoid double-init.
func HTTPServiceInitialized() bool { return globalHTTPService != nil }

// InitHTTPServiceForTest initialises the global HTTP service with simple
// in-memory clients suitable for unit tests.
func InitHTTPServiceForTest() {
	globalHTTPService = &HTTPService{
		generalClient:  &http.Client{Timeout: 30 * time.Second},
		downloadClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// DownloadBytes fetches a URL and returns the body as bytes. When
// cachePath is non-empty, bytes are also persisted atomically to
// that path (useful for marker caching: the bytes returned to the
// caller are race-free; the disk copy is best-effort).
// expectedType, if set, is matched against the response's Content-Type.
// A cachePath write failure is non-fatal: DownloadBytes returns the
// bytes and logs a warning — the in-memory bytes are the authoritative
// result for the caller.
func DownloadBytes(ctx context.Context, fromURL, cachePath, expectedType string, timeout time.Duration) ([]byte, error) {
	if globalHTTPService == nil {
		return nil, fmt.Errorf("HTTP service not initialized")
	}
	parsedURL, err := url.Parse(fromURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	host := parsedURL.Host
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	startTime := time.Now()
	GlobalMetrics.RecordHTTPClientRequest(host)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fromURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", "TileserverCache")

	resp, err := globalHTTPService.downloadClient.Do(req)
	if err != nil {
		GlobalMetrics.RecordHTTPClientError(host)
		return nil, fmt.Errorf("failed to download: %w", err)
	}
	defer resp.Body.Close()

	GlobalMetrics.RecordHTTPClientDuration(host, time.Since(startTime).Seconds())

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		GlobalMetrics.RecordHTTPClientError(host)
		GlobalMetrics.RecordError("http_client", fmt.Sprintf("status_%d", resp.StatusCode))
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}

	if expectedType != "" {
		if ct := resp.Header.Get("Content-Type"); ct != "" && !hasPrefix(ct, expectedType) {
			return nil, fmt.Errorf("invalid content type %q", ct)
		}
	}

	// Cap body size to prevent a malicious or misconfigured upstream
	// from OOMing the server. Markers and other assets fetched through
	// DownloadBytes are image files in the low-KB range; 32 MiB is
	// comfortably above any legitimate payload.
	const maxDownloadBytes = 32 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(data)) > maxDownloadBytes {
		return nil, fmt.Errorf("response body exceeds %d bytes", maxDownloadBytes)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("empty response body")
	}

	if cachePath != "" {
		if err := SaveBytesAtomic(cachePath, data); err != nil {
			slog.Warn("DownloadBytes: cache write failed", "path", cachePath, "error", err)
		}
	}
	return data, nil
}

// SaveBytesAtomic writes data to path via temp file + rename. Callers
// across packages use this as the canonical atomic-write primitive
// (utils.SaveImageBytes and handlers.atomicWriteFile both delegate).
func SaveBytesAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
