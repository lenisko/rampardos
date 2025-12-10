package services

import (
	"log/slog"
	"os"
	"sync/atomic"
)

// RuntimeSettings holds runtime-configurable settings
type RuntimeSettings struct {
	debugEnabled atomic.Bool
}

// GlobalRuntimeSettings is the global runtime settings instance
var GlobalRuntimeSettings *RuntimeSettings

// InitGlobalRuntimeSettings initializes the global runtime settings
func InitGlobalRuntimeSettings(initialDebug bool) {
	GlobalRuntimeSettings = &RuntimeSettings{}
	GlobalRuntimeSettings.debugEnabled.Store(initialDebug)
	updateLogLevel(initialDebug)
}

// IsDebugEnabled returns whether debug logging is enabled
func (rs *RuntimeSettings) IsDebugEnabled() bool {
	return rs.debugEnabled.Load()
}

// SetDebugEnabled sets the debug logging state
func (rs *RuntimeSettings) SetDebugEnabled(enabled bool) {
	rs.debugEnabled.Store(enabled)
	updateLogLevel(enabled)
	slog.Info("Debug mode changed", "enabled", enabled)
}

// ToggleDebug toggles the debug logging state and returns the new state
func (rs *RuntimeSettings) ToggleDebug() bool {
	// Toggle atomically
	for {
		current := rs.debugEnabled.Load()
		newState := !current
		if rs.debugEnabled.CompareAndSwap(current, newState) {
			updateLogLevel(newState)
			slog.Info("Debug mode toggled", "enabled", newState)
			return newState
		}
	}
}

// updateLogLevel updates the slog level based on debug state
func updateLogLevel(debug bool) {
	var level slog.Level
	if debug {
		level = slog.LevelDebug
	} else {
		level = slog.LevelInfo
	}

	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	})
	slog.SetDefault(slog.New(handler))
}
