package renderer

import (
	"strings"
	"testing"

	maplibre "github.com/maplibre/maplibre-native-ffi/bindings/go"
)

// TestHandleEventsForwardsFirstRenderErrorOnly guards the runtime event
// handler's render-error path. It runs on the runtime's native thread,
// so it must never block: a full mapErr keeps the error already pending
// rather than waiting, and unrelated events are ignored.
func TestHandleEventsForwardsFirstRenderErrorOnly(t *testing.T) {
	w := &goWorker{mapErr: make(chan error, 1), wake: make(chan struct{}, 1)}

	w.handleEvents(maplibre.EventBatchView{Events: []maplibre.RuntimeEvent{
		{Type: maplibre.RuntimeEventTypeMapStyleLoaded},
	}})
	select {
	case err := <-w.mapErr:
		t.Fatalf("non-failure event forwarded: %v", err)
	default:
	}

	// Two errors in one batch, then a third in a later batch: none may
	// block, and the first is what the await sees.
	w.handleEvents(maplibre.EventBatchView{Events: []maplibre.RuntimeEvent{
		{Type: maplibre.RuntimeEventTypeMapRenderError, Message: "first"},
		{Type: maplibre.RuntimeEventTypeMapRenderError, Message: "second"},
	}})
	w.handleEvents(maplibre.EventBatchView{Events: []maplibre.RuntimeEvent{
		{Type: maplibre.RuntimeEventTypeMapRenderError, Message: "third"},
	}})

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

// TestStyleFailureIsStateScopedToTheStyle guards the style-failure
// path: a failure is state that every render refuses on (mbgl fails
// only the still pending when a style fails, and a later still never
// completes), it wakes a parked await, and a style command that commits
// after it clears it.
func TestStyleFailureIsStateScopedToTheStyle(t *testing.T) {
	w := &goWorker{mapErr: make(chan error, 1), wake: make(chan struct{}, 1), styleGen: 5}

	// A failure the previous style reported before this style committed.
	w.handleEvents(maplibre.EventBatchView{Events: []maplibre.RuntimeEvent{
		{Type: maplibre.RuntimeEventTypeMapLoadingFailed, Generation: 4, Message: "old style"},
	}})
	if err := w.styleErr(); err != nil {
		t.Fatalf("failure of an earlier style applied to the current one: %v", err)
	}
	<-w.wake

	w.handleEvents(maplibre.EventBatchView{Events: []maplibre.RuntimeEvent{
		{Type: maplibre.RuntimeEventTypeMapLoadingFailed, Generation: 7, Message: "style 404"},
	}})
	select {
	case <-w.wake:
	default:
		t.Fatal("style failure did not wake the await")
	}
	select {
	case err := <-w.mapErr:
		t.Fatalf("style failure sent as a one-shot error: %v", err)
	default:
	}
	for range 2 { // state, not a one-shot signal
		if err := w.styleErr(); err == nil || !strings.Contains(err.Error(), "style 404") {
			t.Fatalf("styleErr = %v, want the style failure", err)
		}
	}

	w.styleGen = 8 // a new style committed
	if err := w.styleErr(); err != nil {
		t.Fatalf("failure survived a new style: %v", err)
	}
}
