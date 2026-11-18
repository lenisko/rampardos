//go:build !linux

// Stub for non-Linux builds. The renderer needs EGL and links
// libmaplibre-native-c.so, both of which are Linux-only here — the
// deployment target is a Linux container and macOS development runs in
// Docker. Keeping this stub means the rest of the package (and the rest of
// the repo) still compiles and tests on a developer Mac; only the renderer
// itself is absent.
//
// This replaced an `mln_ffi` build tag. A GOOS constraint cannot be
// forgotten: a Linux build always gets the real renderer, whereas the tag
// silently produced a binary with no renderer if omitted — and hid a
// compile break in gopool.go from `go build ./...` entirely.
package renderer

import (
	"errors"
)

var errGoBackendNotBuilt = errors.New("renderer: the in-process renderer requires a Linux build with CGO and libmaplibre-native-c.so; build in Docker (see the Dockerfile mln-ffi-build stage) or use `make build-go-renderer` on Linux")

// NewGoPoolRenderer returns an error on non-Linux builds. main.go calls it
// unconditionally, so the signature has to exist on every platform.
func NewGoPoolRenderer(cfg Config) (Renderer, error) {
	return nil, errGoBackendNotBuilt
}
