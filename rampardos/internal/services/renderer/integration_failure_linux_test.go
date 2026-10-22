//go:build renderer_integration

package renderer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestStyleFailureFailsStillFast guards the decision to take load
// failures from the still's own Future rather than from runtime events:
// a style that fails to load, at startup or on reload, must fail the
// render promptly (not by the render timeout), and once a good style is
// loaded again no stale failure may fail the next render. A failed
// still leaves the map with nothing pending, so it must not retire the
// worker either.
func TestStyleFailureFailsStillFast(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	bad := filepath.Join(dir, "bad.json")
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(good, `{"version":8,"sources":{},"layers":[{"id":"bg","type":"background","paint":{"background-color":"#ff8800"}}]}`)
	// A truncated file: mbgl fails it at parse. A path with no file
	// fails it at load.
	write(bad, `{"version":8,"sources":{},"layers":[`)
	missing := filepath.Join(dir, "missing.json")

	const renderTimeout = 10 * time.Second
	newPool := func(styleFile string) *goStylePool {
		t.Helper()
		p, err := newGoStylePool(goStylePoolConfig{
			ambientCacheBytes: -1,
			styleID:           "fail",
			scaleLabel:        "1",
			poolSize:          1,
			minPoolSize:       1,
			ratio:             1,
			styleURL:          "file://" + styleFile,
			startupTimeout:    30 * time.Second,
			renderTimeout:     renderTimeout,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(p.close)
		return p
	}
	vp := ViewportRequest{StyleID: "fail", Latitude: 51.5, Longitude: -0.12, Zoom: 3, Width: 64, Height: 64, Scale: 1}
	render := func(p *goStylePool, wantErr bool, what string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*renderTimeout)
		defer cancel()
		start := time.Now()
		img, err := p.dispatch(ctx, vp, 1)
		took := time.Since(start)
		switch {
		case wantErr && err == nil:
			t.Fatalf("%s: render succeeded, want a failure", what)
		case !wantErr && err != nil:
			t.Fatalf("%s: render failed: %v", what, err)
		case !wantErr && img == nil:
			t.Fatalf("%s: no image", what)
		}
		// Well under the render timeout: the failure came from the
		// still, not from the await giving up.
		if took > renderTimeout/4 {
			t.Fatalf("%s: took %s, want a prompt result (err=%v)", what, took, err)
		}
		t.Logf("%s: %s err=%v", what, took, err)
	}
	sameWorker := func(p *goStylePool, want *goWorker) {
		t.Helper()
		p.mu.Lock()
		defer p.mu.Unlock()
		if len(p.workers) != 1 || p.workers[0] != want {
			t.Fatalf("worker retired by a style failure (workers=%d)", len(p.workers))
		}
	}
	reload := func(p *goStylePool, styleFile string) {
		t.Helper()
		if err := p.setStyleAll(context.Background(), "file://"+styleFile); err != nil {
			t.Fatalf("reload %s: %v", filepath.Base(styleFile), err)
		}
	}

	t.Run("startup", func(t *testing.T) {
		p := newPool(bad)
		w := p.workers[0]
		render(p, true, "broken style at startup")
		render(p, true, "broken style, second render")
		sameWorker(p, w)
		reload(p, good)
		render(p, false, "after reload to a good style")
	})

	t.Run("reload", func(t *testing.T) {
		p := newPool(good)
		w := p.workers[0]
		render(p, false, "good style")
		reload(p, bad)
		render(p, true, "after reload to a broken style")
		render(p, true, "broken style after reload, second render")
		sameWorker(p, w)
		reload(p, good)
		render(p, false, "after reload back to a good style")
		reload(p, missing)
		render(p, true, "after reload to a missing style")
		render(p, true, "missing style, second render")
		sameWorker(p, w)
		reload(p, good)
		render(p, false, "after reload from a missing style")
		render(p, false, "good style, second render")
	})
}
