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

// TestBadStyleFailsFast pins where a bad style fails: its style command
// fails once the style document is loaded and parsed, so the worker never
// starts and the pool build reports it at once. The map used to accept
// the style and fail only the still pending at the time, which took a
// state machine of style failures and generations to keep every later
// render from hanging to the timeout.
func TestBadStyleFailsFast(t *testing.T) {
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
	// A truncated file fails at parse; a path with no file fails at load.
	write(bad, `{"version":8,"sources":{},"layers":[`)
	missing := filepath.Join(dir, "missing.json")

	const startupTimeout = 30 * time.Second
	newPool := func(styleFile string) (*goStylePool, error) {
		t.Helper()
		p, err := newGoStylePool(goStylePoolConfig{
			device:            newTestDevice(t),
			ambientCacheBytes: -1,
			styleID:           "fail",
			scaleLabel:        "1",
			poolSize:          1,
			minPoolSize:       1,
			ratio:             1,
			styleURL:          "file://" + styleFile,
			startupTimeout:    startupTimeout,
			renderTimeout:     10 * time.Second,
		})
		if err == nil {
			t.Cleanup(p.close)
		}
		return p, err
	}

	for _, tc := range []struct{ name, file string }{
		{"broken", bad},
		{"missing", missing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			p, err := newPool(tc.file)
			took := time.Since(start)
			if err == nil {
				p.close()
				t.Fatal("pool built on a bad style")
			}
			if !strings.Contains(err.Error(), `"fail"`) {
				t.Errorf("error does not name the style: %v", err)
			}
			if took > 3*time.Second {
				t.Errorf("bad style took %s to fail, want well under the startup timeout", took)
			}
			t.Logf("%s: %s err=%v", tc.name, took, err)
		})
	}

	t.Run("good", func(t *testing.T) {
		p, err := newPool(good)
		if err != nil {
			t.Fatal(err)
		}
		vp := ViewportRequest{StyleID: "fail", Latitude: 51.5, Longitude: -0.12, Zoom: 3, Width: 64, Height: 64, Scale: 1}
		for i := range 2 {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			img, err := p.dispatch(ctx, vp, 1, nil)
			cancel()
			if err != nil || img == nil {
				t.Fatalf("render %d: img=%v err=%v", i, img != nil, err)
			}
		}
	})
}

// TestReloadWithBrokenStyleKeepsServing: a style edited into a broken
// state now fails the rebuild's worker start, and a failed rebuild must
// leave the previous pool installed and serving.
func TestReloadWithBrokenStyleKeepsServing(t *testing.T) {
	cfg := setupIntegrationFixtures(t)
	ri, err := NewGoPoolRenderer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ri.Close()
	r := ri.(*GoPoolRenderer)

	runRenderAssertions(t, r)
	before, err := r.getOrCreatePool("bg", 1)
	if err != nil {
		t.Fatal(err)
	}

	stylePath := filepath.Join(cfg.StylesDir, "bg", "style.json")
	if err := os.WriteFile(stylePath, []byte(`{"version":8,"sources":{},"layers":[`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.ReloadStyles(context.Background()); err != nil {
		t.Fatal(err)
	}

	after, err := r.getOrCreatePool("bg", 1)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("a failed rebuild replaced the serving pool")
	}
	runRenderAssertions(t, r)
}

// TestMissingResourcesReport pins how glyph and sprite load failures
// behave. Each fails the still with a bare native error, so every render
// error must name the resource, and each failing resource is recorded
// once per pool for the warning log. Repeat renders must fail promptly
// and name the resource again: each still re-requests the failed range.
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
				device:            newTestDevice(t),
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

// TestGlyphFailureRecoversWithoutRetirement: a still re-requests glyph
// ranges that failed earlier, so a glyph failure no longer leaves the map
// unable to finish a later still. The worker that saw it keeps serving,
// and once the glyphs exist it renders them, with no stale failure
// refusing or mislabelling the renders after it.
func TestGlyphFailureRecoversWithoutRetirement(t *testing.T) {
	dir := t.TempDir()
	style := filepath.Join(dir, "style.json")
	body := `{"version":8,"glyphs":"file://` + dir + `/fonts/{fontstack}/{range}.pbf",
	  "sources":{"pt":{"type":"geojson","data":{"type":"Point","coordinates":[-0.12,51.5]}}},
	  "layers":[
	    {"id":"bg","type":"background","paint":{"background-color":"#ff8800"}},
	    {"id":"sym","type":"symbol","source":"pt","layout":{"text-field":"London","text-font":["Late Sans"]}}
	  ]}`
	if err := os.WriteFile(style, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := newGoStylePool(goStylePoolConfig{
		device:            newTestDevice(t),
		ambientCacheBytes: -1,
		styleID:           "late-glyphs",
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
	p.mu.Lock()
	worker := p.workers[0]
	p.mu.Unlock()

	vp := ViewportRequest{StyleID: "late-glyphs", Latitude: 51.5, Longitude: -0.12, Zoom: 10, Width: 128, Height: 128, Scale: 1}
	render := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := p.dispatch(ctx, vp, 1, nil)
		return err
	}

	if err := render(); err == nil || !strings.Contains(err.Error(), "glyphs Late Sans") {
		t.Fatalf("render with the glyphs missing: err=%v, want a failure naming glyphs Late Sans", err)
	}

	// An empty file is a valid glyph range with no glyphs in it.
	fontDir := filepath.Join(dir, "fonts", "Late Sans")
	if err := os.MkdirAll(fontDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fontDir, "0-255.pbf"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if err := render(); err != nil {
			t.Fatalf("render %d after the glyphs appeared: %v", i, err)
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.workers) != 1 || p.workers[0] != worker {
		t.Fatalf("the glyph failure retired the worker (workers=%d)", len(p.workers))
	}
}
