package config

import (
	"os"
	"testing"
	"time"
)

func TestLoadRendererDefaults(t *testing.T) {
	clearRendererEnv(t)
	cfg := Load()
	if cfg.RendererBackend != "go-pool" {
		t.Errorf("RendererBackend: got %q, want %q", cfg.RendererBackend, "go-pool")
	}
	if cfg.RendererRenderTimeout != 15*time.Second {
		t.Errorf("RendererRenderTimeout: got %v, want %v", cfg.RendererRenderTimeout, 15*time.Second)
	}
}

func TestLoadRendererOverrides(t *testing.T) {
	clearRendererEnv(t)
	t.Setenv("RENDERER_BACKEND", "mbgl-binary")
	t.Setenv("RENDERER_POOL_SIZE", "8")
	t.Setenv("RENDERER_TIMEOUT_SECONDS", "30")
	cfg := Load()
	if cfg.RendererBackend != "mbgl-binary" {
		t.Errorf("RendererBackend: got %q", cfg.RendererBackend)
	}
	if cfg.RendererPoolSize != 8 {
		t.Errorf("RendererPoolSize: got %d", cfg.RendererPoolSize)
	}
	if cfg.RendererRenderTimeout != 30*time.Second {
		t.Errorf("RendererRenderTimeout: got %v", cfg.RendererRenderTimeout)
	}
}

func clearRendererEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"RENDERER_BACKEND",
		"RENDERER_POOL_SIZE", "RENDERER_TIMEOUT_SECONDS",
		"RENDERER_STARTUP_TIMEOUT_SECONDS",
	} {
		_ = os.Unsetenv(k)
	}
}
