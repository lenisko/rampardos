// Process-wide EGL display + config for the Go renderer's OpenGL backend.
//
// Since the native-executor binding's core-worker driver (upstream
// maplibre-native-ffi#631, "Run private OpenGL textures on core
// workers"), the session creates and owns its EGL context on a native
// worker thread — the host contributes only a borrowed EGLDisplay and
// EGLConfig and must keep the display initialized until detach
// completes. The context-creation, pbuffer, and make-current machinery
// this file used to carry is gone with the caller-graphics-thread
// driver that needed it.
//
// ClientAPI selects Desktop OpenGL (not ES) for the session's context:
// the previous share-anchor context bound EGL_OPENGL_API for the same
// reason — Mesa-llvmpipe's desktop-GL path measured faster than ES3
// against the Node renderer (see git history of this file).
//
// The display backend is picked via EGL_PLATFORM=surfaceless in the
// runtime image's env so eglGetDisplay(EGL_DEFAULT_DISPLAY) returns
// Mesa's surfaceless platform.
//
// eglGetDisplay(EGL_DEFAULT_DISPLAY) returns the same display to every
// caller in the process, so every worker borrows one display (and one
// llvmpipe rasterizer). It is initialized once and never terminated:
// eglTerminate would invalidate the contexts of every other worker
// still rendering on it, and the display costs nothing to keep.
package renderer

/*
#cgo linux pkg-config: egl

#include <EGL/egl.h>
#include <stddef.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>

typedef struct mln_go_egl_display {
    EGLDisplay display;
    EGLConfig  config;
} mln_go_egl_display;

static int mln_go_egl_init(mln_go_egl_display *out, char *err, size_t err_len) {
    memset(out, 0, sizeof(*out));

    out->display = eglGetDisplay(EGL_DEFAULT_DISPLAY);
    if (out->display == EGL_NO_DISPLAY) {
        snprintf(err, err_len, "eglGetDisplay returned EGL_NO_DISPLAY (0x%x)", eglGetError());
        return -1;
    }
    EGLint major = 0, minor = 0;
    if (eglInitialize(out->display, &major, &minor) == EGL_FALSE) {
        snprintf(err, err_len, "eglInitialize failed (0x%x)", eglGetError());
        return -2;
    }

    // The session creates its context and pbuffer from this config on
    // its core worker; EGL_PBUFFER_BIT is a documented requirement for
    // OpenGL texture sessions.
    EGLint config_attribs[] = {
        EGL_SURFACE_TYPE,    EGL_PBUFFER_BIT,
        EGL_RENDERABLE_TYPE, EGL_OPENGL_BIT,
        EGL_RED_SIZE,        8,
        EGL_GREEN_SIZE,      8,
        EGL_BLUE_SIZE,       8,
        EGL_ALPHA_SIZE,      8,
        EGL_DEPTH_SIZE,      24,
        EGL_STENCIL_SIZE,    8,
        EGL_NONE
    };
    EGLint config_count = 0;
    if (eglChooseConfig(out->display, config_attribs, &out->config, 1, &config_count) == EGL_FALSE ||
        config_count == 0 || out->config == NULL) {
        snprintf(err, err_len, "eglChooseConfig found no config (0x%x)", eglGetError());
        return -3;
    }
    return 0;
}

// eglQueryString fails with EGL_NOT_INITIALIZED on a terminated display.
static int mln_go_egl_initialized(const mln_go_egl_display *d) {
    return eglQueryString(d->display, EGL_VERSION) != NULL;
}

// The binding takes these handles as uintptr (maplibre.NativePointer) and
// converts them back to pointers in C. Doing our half of that in C too
// keeps the EGL handles out of Go's pointer rules entirely: they are
// Mesa-owned, never Go-managed, so uintptr(unsafe.Pointer(...)) on them is
// safe in practice but is exactly the pattern `go vet` flags as possible
// misuse — and vet has no way to know the memory is not Go's.
static uintptr_t mln_go_egl_display_handle(const mln_go_egl_display *d) {
    return (uintptr_t)d->display;
}

static uintptr_t mln_go_egl_config_handle(const mln_go_egl_display *d) {
    return (uintptr_t)d->config;
}

static uintptr_t mln_go_egl_proc_address_handle(void) {
    return (uintptr_t)eglGetProcAddress;
}
*/
import "C"

import (
	"fmt"
	"sync"

	maplibre "github.com/maplibre/maplibre-native-ffi/bindings/go"
)

// eglDisplay is the process-wide headless EGL display + config. Any
// goroutine may use it; nothing here is thread-affine. The binding
// requires the display to stay initialized through session teardown;
// never terminating it satisfies that for every worker at once.
type eglDisplay struct {
	raw C.mln_go_egl_display
}

var (
	sharedEGLMu sync.Mutex
	sharedEGL   *eglDisplay
)

// newEGLDisplay returns the shared display, initializing it on first
// use. A failed initialization is not cached, so a later worker retries.
func newEGLDisplay() (*eglDisplay, error) {
	sharedEGLMu.Lock()
	defer sharedEGLMu.Unlock()
	if sharedEGL != nil {
		return sharedEGL, nil
	}
	d := &eglDisplay{}
	var errBuf [256]C.char
	if rc := C.mln_go_egl_init(&d.raw, &errBuf[0], C.size_t(len(errBuf))); rc != 0 {
		return nil, fmt.Errorf("renderer: EGL init (rc=%d): %s", int(rc), C.GoString(&errBuf[0]))
	}
	sharedEGL = d
	return d, nil
}

// descriptor returns the dedicated-ownership OpenGL context descriptor
// for a core-worker session: the session creates a private context from
// this display+config with the named client API and joins no share
// group.
func (d *eglDisplay) descriptor() maplibre.OpenglContextDescriptor {
	return maplibre.OpenglContextDescriptor{
		Ownership: maplibre.OpenglContextOwnershipDedicated,
		Data: maplibre.OpenglContextDescriptorDataEglVariant{
			Value: maplibre.EglContextDescriptor{
				Display:        uintptr(C.mln_go_egl_display_handle(&d.raw)),
				Config:         uintptr(C.mln_go_egl_config_handle(&d.raw)),
				ClientApi:      maplibre.OpenglClientApiGl,
				GetProcAddress: uintptr(C.mln_go_egl_proc_address_handle()),
			},
		},
	}
}

func (d *eglDisplay) initialized() bool {
	return C.mln_go_egl_initialized(&d.raw) != 0
}
