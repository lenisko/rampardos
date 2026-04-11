//go:build !windows

package services

import (
	"os"
	"time"
)

// getFileCreationTime returns the file creation time on Unix systems
// On most Unix systems, true creation time is not available, so we fall back to ModTime
// This means dropAfter will effectively use modification time on Unix
func getFileCreationTime(info os.FileInfo) time.Time {
	// Unix systems typically don't track creation time reliably
	// Fall back to modification time
	return info.ModTime()
}
