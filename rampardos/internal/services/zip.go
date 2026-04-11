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

// extractZip extracts a ZIP file from bytes to a destination directory
func extractZip(zipData []byte, destDir string) error {
	reader, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return fmt.Errorf("failed to read ZIP: %w", err)
	}

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

		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(destPath, 0755); err != nil {
				return fmt.Errorf("failed to create directory %s: %w", name, err)
			}
			continue
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
