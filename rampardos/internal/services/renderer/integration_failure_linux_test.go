//go:build renderer_integration

package renderer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStyleFailureFailsStillFast guards the decision to take load
// failures from the still's own Future rather than from runtime events:
// a style that fails to load must fail every render promptly (not by
// the render timeout). A reload builds new pools, so a pool's first
// style is the only one it ever loads. A failed still leaves the map
// with nothing pending, so it must not retire the worker either.
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
		img, err := p.dispatch(ctx, vp, 1, nil)
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
	for _, tc := range []struct{ name, file string }{
		{"broken", bad},
		{"missing", missing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPool(tc.file)
			w := p.workers[0]
			render(p, true, tc.name+" style")
			render(p, true, tc.name+" style, second render")
			sameWorker(p, w)
		})
	}

	t.Run("good", func(t *testing.T) {
		p := newPool(good)
		render(p, false, "good style")
		render(p, false, "good style, second render")
	})
}

// TestMissingResourcesReport pins how glyph and sprite load failures
// behave. Each fails the still with a bare native error, so every render
// error must name the resource, and each failing resource is recorded
// once per pool for the warning log. Repeat renders must fail promptly
// too: a failed glyph range is never re-requested, so a map that saw one
// would leave the next still pending until the render timeout.
func TestMissingResourcesReport(t *testing.T) {
	cases := []struct {
		kind, extra, layout string
	}{
		{"glyphs", `"glyphs":"file://DIR/fonts/{fontstack}/{range}.pbf",`, `"text-field":"London","text-font":["Missing Sans"]`},
		{"sprite", `"sprite":"file://DIR/sprite/missing",`, `"icon-image":"nope"`},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			dir := t.TempDir()
			style := filepath.Join(dir, "style.json")
			body := `{"version":8,` + strings.ReplaceAll(tc.extra, "DIR", dir) + `
			  "sources":{"pt":{"type":"geojson","data":{"type":"Point","coordinates":[-0.12,51.5]}}},
			  "layers":[
			    {"id":"bg","type":"background","paint":{"background-color":"#ff8800"}},
			    {"id":"sym","type":"symbol","source":"pt","layout":{` + tc.layout + `}}
			  ]}`
			if err := os.WriteFile(style, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			p, err := newGoStylePool(goStylePoolConfig{
				ambientCacheBytes: -1,
				styleID:           "missing-" + tc.kind,
				scaleLabel:        "1",
				poolSize:          1,
				minPoolSize:       1,
				ratio:             1,
				styleURL:          "file://" + style,
				startupTimeout:    30 * time.Second,
				renderTimeout:     10 * time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(p.close)

			vp := ViewportRequest{StyleID: "missing-" + tc.kind, Latitude: 51.5, Longitude: -0.12, Zoom: 10, Width: 128, Height: 128, Scale: 1}
			for i := 0; i < 3; i++ {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				start := time.Now()
				_, err := p.dispatch(ctx, vp, 1, nil)
				cancel()
				took := time.Since(start)
				if err == nil {
					t.Fatalf("render %d succeeded, want a failure", i)
				}
				if !strings.Contains(err.Error(), tc.kind+" ") {
					t.Fatalf("render %d: error %q does not name the %s failure", i, err, tc.kind)
				}
				if took > 2500*time.Millisecond {
					t.Fatalf("render %d took %s, want a prompt result (err=%v)", i, took, err)
				}
				t.Logf("render %d: %s err=%v", i, took, err)
			}

			// The events arrive on the runtime's thread; give them a moment.
			found := func() (n int) {
				p.resourceErrs.Range(func(k, _ any) bool {
					if strings.HasPrefix(k.(string), tc.kind+" ") {
						n++
					}
					return true
				})
				return n
			}
			deadline := time.Now().Add(2 * time.Second)
			for found() == 0 && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			p.resourceErrs.Range(func(k, _ any) bool { t.Logf("reported: %s", k); return true })
			if found() == 0 {
				t.Fatalf("no %s failure reported", tc.kind)
			}
		})
	}
}

// TestGlyphRetirementDoesNotStrandWaitingRenders guards requests queued
// on a pool whose worker retires. A glyph failure retires the worker
// after its render; with one worker, renders already waiting had no one
// left to take them, and on a context without a deadline (as static
// maps use) they waited forever. The pool must replace the worker so
// every waiting render runs and fails promptly.
func TestGlyphRetirementDoesNotStrandWaitingRenders(t *testing.T) {
	dir := t.TempDir()
	style := filepath.Join(dir, "style.json")
	body := `{"version":8,"glyphs":"file://` + dir + `/fonts/{fontstack}/{range}.pbf",
	  "sources":{"pt":{"type":"geojson","data":{"type":"Point","coordinates":[-0.12,51.5]}}},
	  "layers":[
	    {"id":"bg","type":"background","paint":{"background-color":"#ff8800"}},
	    {"id":"sym","type":"symbol","source":"pt","layout":{"text-field":"London","text-font":["Missing Sans"]}}
	  ]}`
	if err := os.WriteFile(style, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	const renderTimeout = 10 * time.Second
	p, err := newGoStylePool(goStylePoolConfig{
		ambientCacheBytes: -1,
		styleID:           "strand",
		scaleLabel:        "1",
		poolSize:          1,
		minPoolSize:       1,
		ratio:             1,
		styleURL:          "file://" + style,
		startupTimeout:    30 * time.Second,
		renderTimeout:     renderTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.close)

	vp := ViewportRequest{StyleID: "strand", Latitude: 51.5, Longitude: -0.12, Zoom: 10, Width: 128, Height: 128, Scale: 1}
	const renders = 3
	errs := make(chan error, renders)
	start := time.Now()
	for range renders {
		go func() {
			_, err := p.dispatch(context.Background(), vp, 1, nil)
			errs <- err
		}()
	}
	for i := range renders {
		select {
		case err := <-errs:
			if err == nil {
				t.Fatalf("render %d succeeded, want a glyph failure", i)
			}
			t.Logf("render %d after %s: %v", i, time.Since(start), err)
		case <-time.After(2 * renderTimeout):
			t.Fatalf("render %d still waiting after %s: stranded by the retired worker", i, 2*renderTimeout)
		}
	}
	// Each render gets a fresh worker in turn; none should have waited
	// out the queue bound.
	if took := time.Since(start); took > renderTimeout/2 {
		t.Fatalf("renders took %s, want each served by a replacement worker", took)
	}
}
