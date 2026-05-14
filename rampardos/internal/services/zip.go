package services

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxZipEntries = 10000
	maxZipBytes   = 10 * 1024 * 1024 * 1024 // 10 GB
)

// extractZip extracts a ZIP file from bytes to a destination directory
func extractZip(zipData []byte, destDir string) error {
	reader, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return fmt.Errorf("failed to read ZIP: %w", err)
	}

	if len(reader.File) > maxZipEntries {
		return fmt.Errorf("zip contains too many entries (%d > %d)", len(reader.File), maxZipEntries)
	}

	var totalBytes int64

	for _, file := range reader.File {
		// Sanitize path to prevent zip slip vulnerability
		name := filepath.Clean(file.Name)
		if strings.HasPrefix(name, "..") || strings.HasPrefix(name, "/") {
			continue
		}

		destPath := filepath.Join(destDir, name)

		// Ensure the file is within destDir
		if !strings.HasPrefix(destPath, filepath.Clean(destDir)+string(os.PathSeparator)) {
			continue
		}

		fi := file.FileInfo()
		mode := fi.Mode()

		// Skip symlinks and irregular files (devices, pipes, sockets)
		if mode&os.ModeSymlink != 0 {
			continue
		}
		if mode&(os.ModeDevice|os.ModeNamedPipe|os.ModeSocket|os.ModeCharDevice) != 0 {
			continue
		}

		if fi.IsDir() {
			if err := os.MkdirAll(destPath, 0755); err != nil {
				return fmt.Errorf("failed to create directory %s: %w", name, err)
			}
			continue
		}

		// Check total bytes cap (use uncompressed size from zip header)
		totalBytes += int64(file.UncompressedSize64)
		if totalBytes > maxZipBytes {
			return fmt.Errorf("zip extraction exceeds maximum allowed size (%d bytes)", maxZipBytes)
		}

		// Create parent directories
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return fmt.Errorf("failed to create parent directory: %w", err)
		}

		// Extract file
		if err := extractZipFile(file, destPath); err != nil {
			return err
		}
	}

	return nil
}

func extractZipFile(file *zip.File, destPath string) error {
	// Reject if destPath is already a symlink to prevent symlink-following writes
	if fi, err := os.Lstat(destPath); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to overwrite symlink at %s", destPath)
	}

	rc, err := file.Open()
	if err != nil {
		return fmt.Errorf("failed to open ZIP entry %s: %w", file.Name, err)
	}
	defer rc.Close()

	outFile, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, file.Mode())
	if err != nil {
		return fmt.Errorf("failed to create file %s: %w", destPath, err)
	}
	defer outFile.Close()

	// Limit extraction size to prevent zip bombs (100MB per file)
	limited := io.LimitReader(rc, 100*1024*1024)
	if _, err := io.Copy(outFile, limited); err != nil {
		return fmt.Errorf("failed to extract file %s: %w", file.Name, err)
	}

	return nil
}
