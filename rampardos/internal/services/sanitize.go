package services

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SanitizeName validates and sanitizes a user-provided name for use in file paths.
// It prevents path traversal attacks by rejecting names containing "..", "/", or path separators.
func SanitizeName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("invalid name: empty")
	}

	// Clean the path
	cleaned := filepath.Clean(name)

	// Reject path traversal attempts
	if strings.Contains(cleaned, "..") {
		return "", fmt.Errorf("invalid name: path traversal detected")
	}

	// Reject absolute paths or paths with separators
	if strings.HasPrefix(cleaned, "/") || strings.Contains(cleaned, string(os.PathSeparator)) {
		return "", fmt.Errorf("invalid name: path separators not allowed")
	}

	// Reject if cleaning resulted in empty or dot
	if cleaned == "" || cleaned == "." {
		return "", fmt.Errorf("invalid name: invalid characters")
	}

	return cleaned, nil
}
