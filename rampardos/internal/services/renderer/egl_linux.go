// Per-worker EGL context for the Go renderer's OpenGL backend.
//
// One EGL context per OS thread. EGL is thread-local — never share an
// eglContext across goroutines. The worker goroutine that calls
// (*eglContext).descriptor() must already be runtime.LockOSThread-pinned.
//
// Adapted from examples/go-readback/main.go in the upstream
// maplibre-native-ffi checkout (commit 2587cf28854ae0636f6d8512572c0f387b58e81a).
//
// Uses Desktop OpenGL 3.3 Compatibility Profile (not Core, not ES) to
// match Node's GLX path. The original upstream example used ES3 (~55%
// slower than Node across the full distribution); Desktop GL Core was
// also slower than ES3 in prod measurement; glxinfo showed Node sees
// "OpenGL 3.3 (Compatibility Profile)" so Compatibility is the
// specific code path mbgl/Mesa-llvmpipe is most optimized for.
//
// Uses a tiny pbuffer surface to satisfy eglMakeCurrent. The display
// backend is picked via EGL_PLATFORM=surfaceless in the runtime image's
// env so eglGetDisplay(EGL_DEFAULT_DISPLAY) returns Mesa's surfaceless
// platform.
package renderer

/*
#cgo linux pkg-config: egl

#include <EGL/egl.h>
#include <stddef.h>
#include <stdio.h>
#include <string.h>

typedef struct mln_go_egl_context {
    EGLDisplay display;
    EGLConfig  config;
    EGLContext share_context;
    EGLSurface surface;
} mln_go_egl_context;

static int mln_go_egl_init(mln_go_egl_context *out, char *err, size_t err_len) {
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
    if (eglBindAPI(EGL_OPENGL_API) == EGL_FALSE) {
        snprintf(err, err_len, "eglBindAPI(EGL_OPENGL_API) failed (0x%x)", eglGetError());
        return -3;
    }

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
        return -4;
    }

    EGLint context_attribs[] = {
        EGL_CONTEXT_MAJOR_VERSION, 3,
        EGL_CONTEXT_MINOR_VERSION, 3,
        EGL_CONTEXT_OPENGL_PROFILE_MASK, EGL_CONTEXT_OPENGL_COMPATIBILITY_PROFILE_BIT,
        EGL_NONE
    };
    out->share_context = eglCreateContext(out->display, out->config, EGL_NO_CONTEXT, context_attribs);
    if (out->share_context == EGL_NO_CONTEXT) {
        snprintf(err, err_len, "eglCreateContext failed (0x%x)", eglGetError());
        return -5;
    }

    EGLint surface_attribs[] = {
        EGL_WIDTH,  8,
        EGL_HEIGHT, 8,
        EGL_NONE
    };
    out->surface = eglCreatePbufferSurface(out->display, out->config, surface_attribs);
    if (out->surface == EGL_NO_SURFACE) {
        snprintf(err, err_len, "eglCreatePbufferSurface failed (0x%x)", eglGetError());
        return -6;
    }

    if (eglMakeCurrent(out->display, out->surface, out->surface, out->share_context) == EGL_FALSE) {
        snprintf(err, err_len, "eglMakeCurrent failed (0x%x)", eglGetError());
        return -7;
    }
    return 0;
}

static void mln_go_egl_destroy(mln_go_egl_context *ctx) {
    if (ctx == NULL || ctx->display == EGL_NO_DISPLAY) return;
    eglMakeCurrent(ctx->display, EGL_NO_SURFACE, EGL_NO_SURFACE, EGL_NO_CONTEXT);
    if (ctx->surface != EGL_NO_SURFACE) eglDestroySurface(ctx->display, ctx->surface);
    if (ctx->share_context != EGL_NO_CONTEXT) eglDestroyContext(ctx->display, ctx->share_context);
    eglTerminate(ctx->display);
    memset(ctx, 0, sizeof(*ctx));
}

static void *mln_go_egl_get_proc_address(void) {
    return (void *)eglGetProcAddress;
}
*/
import "C"

import (
	"fmt"
	"unsafe"

	maplibre "github.com/maplibre/maplibre-native-ffi/bindings/go"
)

// eglContext is the per-worker headless EGL context. Construct on the
// worker's OS thread (after runtime.LockOSThread), pass descriptor() to
// maplibre.Map.AttachOpenGLOwnedTexture, close() on worker exit on the
// same thread.
type eglContext struct {
	raw C.mln_go_egl_context
}

func newEGLContext() (*eglContext, error) {
	c := &eglContext{}
	var errBuf [256]C.char
	if rc := C.mln_go_egl_init(&c.raw, &errBuf[0], C.size_t(len(errBuf))); rc != 0 {
		return nil, fmt.Errorf("renderer: EGL init (rc=%d): %s", int(rc), C.GoString(&errBuf[0]))
	}
	return c, nil
}

// descriptor returns the maplibre binding's OpenGLContextDescriptor that
// borrows this context's display/config/share-context + get_proc_address.
// Caller passes the result into Map.AttachOpenGLOwnedTexture(...).
//
// The returned descriptor is valid only as long as this eglContext is
// alive. Caller must keep the *eglContext referenced for the lifetime
// of the RenderSession that uses it.
func (c *eglContext) descriptor() maplibre.OpenGLContextDescriptor {
	return maplibre.OpenGLContextDescriptor{
		EGL: &maplibre.EGLContextDescriptor{
			Display:        maplibre.NativePointer(uintptr(unsafe.Pointer(c.raw.display))),
			Config:         maplibre.NativePointer(uintptr(unsafe.Pointer(c.raw.config))),
			ShareContext:   maplibre.NativePointer(uintptr(unsafe.Pointer(c.raw.share_context))),
			GetProcAddress: maplibre.NativePointer(uintptr(C.mln_go_egl_get_proc_address())),
		},
	}
}

func (c *eglContext) close() {
	C.mln_go_egl_destroy(&c.raw)
}
