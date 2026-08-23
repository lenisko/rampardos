// Per-worker EGL display + config for the Go renderer's OpenGL backend.
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

static void mln_go_egl_destroy(mln_go_egl_display *d) {
    if (d == NULL || d->display == EGL_NO_DISPLAY) return;
    eglTerminate(d->display);
    memset(d, 0, sizeof(*d));
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

	maplibre "github.com/maplibre/maplibre-native-ffi/bindings/go"
)

// eglDisplay is the per-worker headless EGL display + config. Any
// goroutine may construct it; nothing here is thread-affine. close()
// only after the session that borrowed it has completed detach — the
// contract keeps the EGLDisplay initialized through session teardown.
type eglDisplay struct {
	raw C.mln_go_egl_display
}

func newEGLDisplay() (*eglDisplay, error) {
	d := &eglDisplay{}
	var errBuf [256]C.char
	if rc := C.mln_go_egl_init(&d.raw, &errBuf[0], C.size_t(len(errBuf))); rc != 0 {
		return nil, fmt.Errorf("renderer: EGL init (rc=%d): %s", int(rc), C.GoString(&errBuf[0]))
	}
	return d, nil
}

// descriptor returns the dedicated-ownership OpenGL context descriptor
// for a core-worker session: the session creates a private context from
// this display+config with the named client API and joins no share
// group.
//
// The returned descriptor is valid only as long as this eglDisplay is
// alive. Caller must keep the *eglDisplay referenced for the lifetime
// of the RenderSession that uses it.
func (d *eglDisplay) descriptor() maplibre.OpenGLContextDescriptor {
	return maplibre.OpenGLContextDescriptor{
		Ownership: maplibre.OpenGLContextOwnershipDedicated,
		EGL: &maplibre.EGLContextDescriptor{
			Display:        maplibre.NativePointer(C.mln_go_egl_display_handle(&d.raw)),
			Config:         maplibre.NativePointer(C.mln_go_egl_config_handle(&d.raw)),
			ClientAPI:      maplibre.OpenGLClientAPIGL,
			GetProcAddress: maplibre.NativePointer(C.mln_go_egl_proc_address_handle()),
		},
	}
}

func (d *eglDisplay) close() {
	C.mln_go_egl_destroy(&d.raw)
}
