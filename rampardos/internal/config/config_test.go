package config

import (
	"os"
	"testing"
	"time"
)

func TestLoadRendererDefaults(t *testing.T) {
	clearRendererEnv(t)
	cfg := Load()
	if cfg.RendererBackend != "node-pool" {
		t.Errorf("RendererBackend: got %q, want %q", cfg.RendererBackend, "node-pool")
	}
	if cfg.RendererNodeBinary != "node" {
		t.Errorf("RendererNodeBinary: got %q, want %q", cfg.RendererNodeBinary, "node")
	}
	if cfg.RendererWorkerScript == "" {
		t.Errorf("RendererWorkerScript default must be non-empty")
	}
	if cfg.RendererRenderTimeout != 15*time.Second {
		t.Errorf("RendererRenderTimeout: got %v, want %v", cfg.RendererRenderTimeout, 15*time.Second)
	}
	if cfg.RendererWorkerLifetime != 500 {
		t.Errorf("RendererWorkerLifetime: got %d, want 500", cfg.RendererWorkerLifetime)
	}
}

func TestLoadRendererOverrides(t *testing.T) {
	clearRendererEnv(t)
	t.Setenv("RENDERER_BACKEND", "mbgl-binary")
	t.Setenv("RENDERER_NODE_BINARY", "/usr/local/bin/node")
	t.Setenv("RENDERER_POOL_SIZE", "8")
	t.Setenv("RENDERER_TIMEOUT_SECONDS", "30")
	t.Setenv("RENDERER_WORKER_LIFETIME", "1000")
	cfg := Load()
	if cfg.RendererBackend != "mbgl-binary" {
		t.Errorf("RendererBackend: got %q", cfg.RendererBackend)
	}
	if cfg.RendererNodeBinary != "/usr/local/bin/node" {
		t.Errorf("RendererNodeBinary: got %q", cfg.RendererNodeBinary)
	}
	if cfg.RendererPoolSize != 8 {
		t.Errorf("RendererPoolSize: got %d", cfg.RendererPoolSize)
	}
	if cfg.RendererRenderTimeout != 30*time.Second {
		t.Errorf("RendererRenderTimeout: got %v", cfg.RendererRenderTimeout)
	}
	if cfg.RendererWorkerLifetime != 1000 {
		t.Errorf("RendererWorkerLifetime: got %d", cfg.RendererWorkerLifetime)
	}
}

func clearRendererEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"RENDERER_BACKEND", "RENDERER_NODE_BINARY", "RENDERER_WORKER_SCRIPT",
		"RENDERER_POOL_SIZE", "RENDERER_TIMEOUT_SECONDS", "RENDERER_WORKER_LIFETIME",
		"RENDERER_STARTUP_TIMEOUT_SECONDS",
	} {
		_ = os.Unsetenv(k)
	}
}
