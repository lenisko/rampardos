//go:build renderer_integration && mln_ffi

package renderer

import (
	"testing"
)

// TestIntegrationGo exercises GoPoolRenderer against the in-process
// maplibre-native binding. Mirrors TestIntegrationNode's setup and
// assertions verbatim via setupIntegrationFixtures + runRenderAssertions
// so the two backends are observably equivalent at the Renderer
// interface boundary; a regression that's only visible through one
// backend would still trip the shared asserts.
//
// Prerequisites:
//   - libmaplibre-native-c.so available via pkg-config
//     (see scripts/build-mln-ffi.sh or `make build-ffi`)
//   - Build/test invocation includes -tags 'renderer_integration mln_ffi'
//
// Run:
//   PKG_CONFIG_PATH=$MLN_FFI_DIR_HOST/build/pkgconfig \
//   CGO_LDFLAGS="-Wl,-rpath,$MLN_FFI_DIR_HOST/build" \
//   CGO_ENABLED=1 \
//   go test -tags 'renderer_integration mln_ffi' \
//     ./internal/services/renderer/ -v -run TestIntegrationGo
func TestIntegrationGo(t *testing.T) {
	cfg := setupIntegrationFixtures(t)
	cfg.Backend = "go-pool"

	r, err := NewGoPoolRenderer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	runRenderAssertions(t, r)
}
