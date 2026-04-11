//go:build windows

package services

import (
	"os"
	"syscall"
	"time"
)

// getFileCreationTime returns the file creation time on Windows
func getFileCreationTime(info os.FileInfo) time.Time {
	if sys := info.Sys(); sys != nil {
		if stat, ok := sys.(*syscall.Win32FileAttributeData); ok {
			return time.Unix(0, stat.CreationTime.Nanoseconds())
		}
	}
	return time.Time{}
}
