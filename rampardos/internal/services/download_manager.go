package services

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// DownloadStatus represents the state of a download
type DownloadStatus string

const (
	DownloadStatusPending     DownloadStatus = "pending"
	DownloadStatusDownloading DownloadStatus = "downloading"
	DownloadStatusComplete    DownloadStatus = "complete"
	DownloadStatusError       DownloadStatus = "error"
)

// DownloadInfo holds information about a download
type DownloadInfo struct {
	Name            string             `json:"name"`
	URL             string             `json:"url"`
	Path            string             `json:"-"` // file path (not exposed in JSON)
	Status          DownloadStatus     `json:"status"`
	Progress        float64            `json:"progress"` // 0-100
	BytesTotal      int64              `json:"bytes_total"`
	BytesDownloaded int64              `json:"bytes_downloaded"`
	Error           string             `json:"error,omitempty"`
	StartedAt       time.Time          `json:"started_at"`
	CompletedAt     *time.Time         `json:"completed_at,omitempty"`
	cancel          context.CancelFunc // cancel function for stopping download
}

// DownloadManager manages background downloads
type DownloadManager struct {
	mu         sync.RWMutex
	downloads  map[string]*DownloadInfo
	onComplete func(name string, err error) // Callback when download completes
}

// ErrDownloadCancelled is returned when a download is cancelled
var ErrDownloadCancelled = fmt.Errorf("download cancelled")

// NewDownloadManager creates a new download manager
func NewDownloadManager(onComplete func(name string, err error)) *DownloadManager {
	return &DownloadManager{
		downloads:  make(map[string]*DownloadInfo),
		onComplete: onComplete,
	}
}

// StartDownload starts a background download
func (dm *DownloadManager) StartDownload(name, fromURL, toPath string) error {
	dm.mu.Lock()

	// Check if already downloading
	if existing, ok := dm.downloads[name]; ok {
		if existing.Status == DownloadStatusDownloading || existing.Status == DownloadStatusPending {
			dm.mu.Unlock()
			return fmt.Errorf("download already in progress for %s", name)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	info := &DownloadInfo{
		Name:      name,
		URL:       fromURL,
		Path:      toPath,
		Status:    DownloadStatusPending,
		Progress:  0,
		StartedAt: time.Now(),
		cancel:    cancel,
	}
	dm.downloads[name] = info
	dm.mu.Unlock()

	// Start download in background
	go dm.download(ctx, name, fromURL, toPath)

	return nil
}

// GetDownload returns info about a specific download
func (dm *DownloadManager) GetDownload(name string) *DownloadInfo {
	dm.mu.RLock()
	defer dm.mu.RUnlock()

	if info, ok := dm.downloads[name]; ok {
		// Return a copy
		copy := *info
		return &copy
	}
	return nil
}

// GetAllDownloads returns all download info
func (dm *DownloadManager) GetAllDownloads() map[string]*DownloadInfo {
	dm.mu.RLock()
	defer dm.mu.RUnlock()

	result := make(map[string]*DownloadInfo)
	for k, v := range dm.downloads {
		copy := *v
		result[k] = &copy
	}
	return result
}

// ClearDownload removes completed/errored downloads from tracking
func (dm *DownloadManager) ClearDownload(name string) {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	if info, ok := dm.downloads[name]; ok {
		if info.Status == DownloadStatusComplete || info.Status == DownloadStatusError {
			delete(dm.downloads, name)
		}
	}
}

// CancelDownload cancels an in-progress download and removes the partial file
func (dm *DownloadManager) CancelDownload(name string) error {
	dm.mu.Lock()
	info, ok := dm.downloads[name]
	if !ok {
		dm.mu.Unlock()
		return fmt.Errorf("download not found: %s", name)
	}

	if info.Status != DownloadStatusDownloading && info.Status != DownloadStatusPending {
		dm.mu.Unlock()
		return fmt.Errorf("download is not in progress: %s", name)
	}

	// Cancel the context to stop the download
	if info.cancel != nil {
		info.cancel()
	}
	path := info.Path
	dm.mu.Unlock()

	// Remove the partial file
	if path != "" {
		os.Remove(path)
	}

	// Remove from tracking
	dm.mu.Lock()
	delete(dm.downloads, name)
	dm.mu.Unlock()

	return nil
}

func (dm *DownloadManager) download(ctx context.Context, name, fromURL, toPath string) {
	dm.mu.Lock()
	info := dm.downloads[name]
	info.Status = DownloadStatusDownloading
	dm.mu.Unlock()

	err := dm.downloadWithProgress(ctx, name, fromURL, toPath)

	// Check if cancelled (download removed from map)
	dm.mu.Lock()
	info, exists := dm.downloads[name]
	if !exists {
		// Download was cancelled, don't call callback
		dm.mu.Unlock()
		return
	}

	now := time.Now()
	if err != nil {
		info.Status = DownloadStatusError
		info.Error = err.Error()
	} else {
		info.Status = DownloadStatusComplete
		info.Progress = 100
	}
	info.CompletedAt = &now
	dm.mu.Unlock()

	// Call completion callback (only if not cancelled)
	if dm.onComplete != nil && err == nil {
		dm.onComplete(name, err)
	}
}

func (dm *DownloadManager) downloadWithProgress(ctx context.Context, name, fromURL, toPath string) error {
	parsedURL, err := url.Parse(fromURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	host := parsedURL.Host

	if GlobalMetrics != nil {
		GlobalMetrics.RecordHTTPClientRequest(host)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fromURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("User-Agent", "Rampardos")

	client := &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			ResponseHeaderTimeout: 60 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		if GlobalMetrics != nil {
			GlobalMetrics.RecordHTTPClientError(host)
		}
		return fmt.Errorf("failed to download file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if GlobalMetrics != nil {
			GlobalMetrics.RecordHTTPClientError(host)
		}
		return fmt.Errorf("failed to load file. Got %d", resp.StatusCode)
	}

	// Get content length for progress
	contentLength := resp.ContentLength
	dm.mu.Lock()
	if info, ok := dm.downloads[name]; ok {
		info.BytesTotal = contentLength
	}
	dm.mu.Unlock()

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

	// Copy with progress tracking
	buf := make([]byte, 1024*1024) // 1MB buffer
	var written int64

	for {
		// Check for cancellation
		select {
		case <-ctx.Done():
			file.Close()
			os.Remove(toPath)
			return ErrDownloadCancelled
		default:
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			nw, writeErr := file.Write(buf[:n])
			if writeErr != nil {
				os.Remove(toPath)
				return fmt.Errorf("failed to write file: %w", writeErr)
			}
			written += int64(nw)

			// Update progress
			dm.mu.Lock()
			if info, ok := dm.downloads[name]; ok {
				info.BytesDownloaded = written
				if contentLength > 0 {
					info.Progress = float64(written) / float64(contentLength) * 100
				}
			}
			dm.mu.Unlock()
		}

		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			os.Remove(toPath)
			return fmt.Errorf("failed to read response: %w", readErr)
		}
	}

	if written == 0 {
		os.Remove(toPath)
		return fmt.Errorf("failed to load file. Got empty data")
	}

	return nil
}
