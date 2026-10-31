package renderer

import (
	"testing"
	"time"
)

// Every worker's eglGetDisplay(EGL_DEFAULT_DISPLAY) returns the same
// process-wide display, so a retiring worker must not terminate it out
// from under the workers still rendering on it.
func TestEGLDisplaySurvivesWorkerRetirement(t *testing.T) {
	retiring, err := newEGLDisplay()
	if err != nil {
		t.Skipf("no EGL in this environment: %v", err)
	}
	live, err := newEGLDisplay()
	if err != nil {
		t.Fatal(err)
	}

	w := &goWorker{
		pool: &goStylePool{cfg: goStylePoolConfig{settleTimeout: time.Second}},
		egl:  retiring,
	}
	w.cleanup()

	if !live.initialized() {
		t.Fatal("a retiring worker's cleanup terminated the display other workers still use")
	}
}
