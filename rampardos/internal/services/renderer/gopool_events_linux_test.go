package renderer

import (
	"strings"
	"testing"

	maplibre "github.com/maplibre/maplibre-native-ffi/bindings/go"
)

// TestHandleEventsForwardsFirstFailureOnly guards the runtime event
// handler. It runs on the runtime's native thread, so it must never
// block: it ignores everything but the two map failures, and a full
// mapErr keeps the failure already pending rather than waiting.
func TestHandleEventsForwardsFirstFailureOnly(t *testing.T) {
	w := &goWorker{mapErr: make(chan error, 1)}

	w.handleEvents(maplibre.EventBatchView{Events: []maplibre.RuntimeEvent{
		{Type: maplibre.RuntimeEventTypeMapStyleLoaded},
	}})
	select {
	case err := <-w.mapErr:
		t.Fatalf("non-failure event forwarded: %v", err)
	default:
	}

	// Two failures in one batch, then a third in a later batch: none may
	// block, and the first is what the await sees.
	w.handleEvents(maplibre.EventBatchView{Events: []maplibre.RuntimeEvent{
		{Type: maplibre.RuntimeEventTypeMapLoadingFailed, Message: "style 404"},
		{Type: maplibre.RuntimeEventTypeMapRenderError, Message: "second"},
	}})
	w.handleEvents(maplibre.EventBatchView{Events: []maplibre.RuntimeEvent{
		{Type: maplibre.RuntimeEventTypeMapRenderError, Message: "third"},
	}})

	select {
	case err := <-w.mapErr:
		if !strings.Contains(err.Error(), "map loading failed: style 404") {
			t.Fatalf("got %q, want the first failure", err)
		}
	default:
		t.Fatal("failure not forwarded")
	}
	select {
	case err := <-w.mapErr:
		t.Fatalf("later failure queued behind the first: %v", err)
	default:
	}
}
