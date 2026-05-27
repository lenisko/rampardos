//go:build !mln_ffi

// Stub for the in-process Go renderer when the binding is not built in.
// The real implementation in gopool.go imports
// github.com/maplibre/maplibre-native-ffi/bindings/go, which pulls in
// CGO and requires libmaplibre-native-c.so at build/run time. To keep
// the standard rampardos build unchanged (CGO_ENABLED=0, no FFI
// dependency), the import is gated behind the `mln_ffi` build tag and
// selecting RENDERER_BACKEND=go-pool without that tag produces a clear
// error rather than a silent fallback.
package renderer

import (
	"errors"
)

var errGoBackendNotBuilt = errors.New("renderer: go-pool backend requires building rampardos with -tags mln_ffi (CGO + libmaplibre-native-c.so via the upstream binding); see Dockerfile mln-ffi-build stage for the build setup")

// NewGoPoolRenderer returns an error in untagged builds. main.go selects
// the backend by config string, so the call site stays unconditional.
func NewGoPoolRenderer(cfg Config) (Renderer, error) {
	return nil, errGoBackendNotBuilt
}
