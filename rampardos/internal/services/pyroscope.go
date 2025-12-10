package services

import (
	"log/slog"
	"os"
	"runtime"

	"github.com/grafana/pyroscope-go"
	"github.com/lenisko/rampardos/internal/config"
)

// InitPyroscope initializes Pyroscope profiling if configured
func InitPyroscope(cfg *config.Config) {
	if cfg.PyroscopeServerAddress == "" {
		return
	}

	slog.Info("Pyroscope starting", "server", cfg.PyroscopeServerAddress)

	runtime.SetMutexProfileFraction(cfg.PyroscopeMutexProfileFraction)
	runtime.SetBlockProfileRate(cfg.PyroscopeBlockProfileRate)

	pyroscopeConfig := pyroscope.Config{
		ApplicationName: cfg.PyroscopeApplicationName,
		ServerAddress:   cfg.PyroscopeServerAddress,
		Tags:            map[string]string{"hostname": os.Getenv("HOSTNAME")},
		ProfileTypes: []pyroscope.ProfileType{
			pyroscope.ProfileCPU,
			pyroscope.ProfileAllocObjects,
			pyroscope.ProfileAllocSpace,
			pyroscope.ProfileInuseObjects,
			pyroscope.ProfileInuseSpace,
			pyroscope.ProfileGoroutines,
			pyroscope.ProfileMutexCount,
			pyroscope.ProfileMutexDuration,
			pyroscope.ProfileBlockCount,
			pyroscope.ProfileBlockDuration,
		},
	}

	if cfg.PyroscopeLogger {
		pyroscopeConfig.Logger = pyroscope.StandardLogger
	}

	if cfg.PyroscopeApiKey != "" {
		pyroscopeConfig.HTTPHeaders = map[string]string{
			"Authorization": "Bearer " + cfg.PyroscopeApiKey,
		}
	} else if cfg.PyroscopeBasicAuthUser != "" {
		pyroscopeConfig.BasicAuthUser = cfg.PyroscopeBasicAuthUser
		pyroscopeConfig.BasicAuthPassword = cfg.PyroscopeBasicAuthPassword
	}

	_, err := pyroscope.Start(pyroscopeConfig)
	if err != nil {
		slog.Error("Pyroscope init failed", "error", err)
	} else {
		slog.Info("Pyroscope started successfully", "app", cfg.PyroscopeApplicationName)
	}
}
