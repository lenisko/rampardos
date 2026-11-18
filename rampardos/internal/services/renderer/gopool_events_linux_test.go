package renderer

import (
	"strings"
	"testing"

	"maplibre.org/maplibre-native-ffi/bindings/go/maplibre"
)

// TestHandleEventsForwardsFirstRenderErrorOnly guards the runtime event
// handler's render-error path. It runs on the runtime's native thread,
// so it must never block: a full mapErr keeps the error already pending
// rather than waiting, and unrelated events are ignored.
func TestHandleEventsForwardsFirstRenderErrorOnly(t *testing.T) {
	w := &goWorker{mapErr: make(chan error, 1), wake: make(chan struct{}, 1)}

	w.handleEvents([]maplibre.RuntimeEvent{
		{Type: maplibre.RuntimeEventTypeMapStyleLoaded},
	})
	select {
	case err := <-w.mapErr:
		t.Fatalf("non-failure event forwarded: %v", err)
	default:
	}

	// Two errors in one batch, then a third in a later batch: none may
	// block, and the first is what the await sees.
	w.handleEvents([]maplibre.RuntimeEvent{
		{Type: maplibre.RuntimeEventTypeMapRenderError, Message: "first"},
		{Type: maplibre.RuntimeEventTypeMapRenderError, Message: "second"},
	})
	w.handleEvents([]maplibre.RuntimeEvent{
		{Type: maplibre.RuntimeEventTypeMapRenderError, Message: "third"},
	})

	select {
	case err := <-w.mapErr:
		if !strings.Contains(err.Error(), "map render error: first") {
			t.Fatalf("got %q, want the first render error", err)
		}
	default:
		t.Fatal("render error not forwarded")
	}
	select {
	case err := <-w.mapErr:
		t.Fatalf("later render error queued behind the first: %v", err)
	default:
	}
}
