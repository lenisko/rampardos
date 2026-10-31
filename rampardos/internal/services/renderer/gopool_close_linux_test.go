package renderer

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestCloseRefusesWaitingDispatches guards pool shutdown against
// concurrent renders. A reload closes the pools it replaced while
// requests are still dispatching to them, so close must refuse a
// dispatch no worker has taken. Closing the command channel instead
// panicked every sender with "send on closed channel".
func TestCloseRefusesWaitingDispatches(t *testing.T) {
	p := newTestPool(1, 1) // inert workers: nothing ever takes a render

	const waiting = 4
	errs := make(chan error, waiting)
	for range waiting {
		go func() {
			_, err := p.dispatch(context.Background(), ViewportRequest{Width: 1, Height: 1}, 1, nil)
			errs <- err
		}()
	}
	time.Sleep(50 * time.Millisecond) // let them block on the queue

	p.close()
	for range waiting {
		select {
		case err := <-errs:
			if !errors.Is(err, errPoolClosed) {
				t.Errorf("dispatch on a closed pool: got %v, want errPoolClosed", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("dispatch still waiting after close")
		}
	}

	if _, err := p.dispatch(context.Background(), ViewportRequest{Width: 1, Height: 1}, 1, nil); !errors.Is(err, errPoolClosed) {
		t.Errorf("dispatch after close: got %v, want errPoolClosed", err)
	}
}
