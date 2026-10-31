//go:build renderer_integration

package renderer

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestPoolCreationDoesNotStallOtherPools guards getOrCreatePool's lock
// scope. Creating a pool takes up to the startup timeout, and it used to
// run under the renderer's write lock, so every render of every warm pool
// waited behind the first request for a new style or scale. Here the new
// style's style.json is a named pipe, so its creation blocks reading it
// until the test writes the style. With a single global slot it also
// guards against the build holding that slot.
func TestPoolCreationDoesNotStallOtherPools(t *testing.T) {
	cfg := setupIntegrationFixtures(t)
	slowDir := filepath.Join(cfg.StylesDir, "slow")
	if err := os.MkdirAll(slowDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(slowDir, "style.json")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	// One global slot: a cold build holding it would starve the warm pool.
	cfg.PoolSize = 1
	cfg.DiscoverStyles = func() ([]string, error) { return []string{"bg", "slow"}, nil }

	r, err := NewGoPoolRenderer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	render := func(style string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := r.RenderViewportImage(ctx, ViewportRequest{StyleID: style, Zoom: 2, Width: 64, Height: 64, Scale: 1})
		return err
	}
	if err := render("bg"); err != nil { // warm the bg pool
		t.Fatal(err)
	}

	slow := make(chan error, 1)
	go func() { slow <- render("slow") }()
	time.Sleep(200 * time.Millisecond) // the slow pool is now blocked reading its style

	done := make(chan error, 1)
	go func() { done <- render("bg") }()
	stalled := false
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		stalled = true
		t.Error("render on a warm pool waited for another pool's creation")
	}

	// Unblock the slow pool so it can finish.
	w, err := os.OpenFile(fifo, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.WriteString(`{"version":8,"sources":{},"layers":[{"id":"bg","type":"background","paint":{"background-color":"#00ff00"}}]}`)
	_ = w.Close()
	if err := <-slow; err != nil {
		t.Fatalf("slow pool: %v", err)
	}
	if stalled {
		<-done
	}
}
