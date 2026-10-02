//go:build renderer_integration

package renderer

import (
	"testing"
)

// TestIntegrationGo exercises GoPoolRenderer against the in-process
// maplibre-native binding, using the shared fixtures and assertions in
// integration_test.go.
//
// Prerequisites:
//   - libmaplibre-native-c.so available via pkg-config
//     (see scripts/build-mln-ffi.sh or `make build-ffi`)
//   - Linux, and -tags renderer_integration
//
// Run:
//
//	PKG_CONFIG_PATH=$MLN_FFI_DIR_HOST/build/pkgconfig \
//	CGO_LDFLAGS="-Wl,-rpath,$MLN_FFI_DIR_HOST/build" \
//	CGO_ENABLED=1 \
//	go test -tags renderer_integration \
//	  ./internal/services/renderer/ -v -run TestIntegrationGo
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
