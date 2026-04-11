package services

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// TileserverMonitor monitors tileserver health and restarts it if unresponsive
type TileserverMonitor struct {
	healthURL        string
	checkInterval    time.Duration
	timeout          time.Duration
	failureThreshold int
	client           *http.Client
	failures         int
}

// NewTileserverMonitor creates a new monitor
func NewTileserverMonitor(tileServerURL string, checkInterval, timeout time.Duration, failureThreshold int) *TileserverMonitor {
	return &TileserverMonitor{
		healthURL:        tileServerURL + "/health",
		checkInterval:    checkInterval,
		timeout:          timeout,
		failureThreshold: failureThreshold,
		client: &http.Client{
			Timeout: timeout,
		},
	}
}

// Start begins monitoring in a background goroutine
func (m *TileserverMonitor) Start() {
	go m.run()
	slog.Info("Tileserver monitor started",
		"healthURL", m.healthURL,
		"interval", m.checkInterval,
		"timeout", m.timeout,
		"failureThreshold", m.failureThreshold,
	)
}

func (m *TileserverMonitor) run() {
	// Wait for tileserver to start
	time.Sleep(30 * time.Second)

	ticker := time.NewTicker(m.checkInterval)
	defer ticker.Stop()

	for range ticker.C {
		if err := m.checkHealth(); err != nil {
			m.failures++
			slog.Warn("Tileserver health check failed",
				"error", err,
				"failures", m.failures,
				"threshold", m.failureThreshold,
			)

			if m.failures >= m.failureThreshold {
				slog.Error("Tileserver unresponsive, attempting restart",
					"failures", m.failures,
				)
				if err := m.restartTileserver(); err != nil {
					slog.Error("Failed to restart tileserver", "error", err)
				} else {
					GlobalMetrics.IncTileserverRestarts()
					slog.Info("Tileserver restart signal sent, waiting for recovery")
					m.failures = 0
					// Wait for tileserver to restart
					time.Sleep(30 * time.Second)
				}
			}
		} else {
			if m.failures > 0 {
				slog.Info("Tileserver recovered", "previousFailures", m.failures)
			}
			m.failures = 0
		}
	}
}

func (m *TileserverMonitor) checkHealth() error {
	ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", m.healthURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy status: %d", resp.StatusCode)
	}

	return nil
}

func (m *TileserverMonitor) restartTileserver() error {
	// Find the tileserver process (node process running tileserver-gl)
	pid, err := findTileserverPID()
	if err != nil {
		return fmt.Errorf("failed to find tileserver PID: %w", err)
	}

	slog.Info("Found tileserver process", "pid", pid)

	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("failed to find process: %w", err)
	}

	// Use SIGKILL - the process is hung/deadlocked and won't respond to SIGTERM
	// Docker will restart the container due to restart policy
	if err := process.Signal(syscall.SIGKILL); err != nil {
		return fmt.Errorf("failed to send SIGKILL: %w", err)
	}

	return nil
}

// findTileserverPID finds the main node process running tileserver-gl
func findTileserverPID() (int, error) {
	// Read /proc to find the node process
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, fmt.Errorf("failed to read /proc: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}

		// Read cmdline
		cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			continue
		}

		cmdStr := string(cmdline)
		// Look for the main tileserver node process: "node /usr/src/app/ -p 8080"
		if strings.Contains(cmdStr, "node") && strings.Contains(cmdStr, "/usr/src/app") {
			return pid, nil
		}
	}

	return 0, fmt.Errorf("tileserver process not found")
}
