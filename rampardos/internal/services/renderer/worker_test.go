package renderer

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestWorkerHandshakeAndRender(t *testing.T) {
	w, err := spawnWorker(workerArgs{
		binary:           "bash",
		script:           "testdata/fake-worker-ok.sh",
		styleID:          "fake",
		handshakeTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.kill()

	if w.handshake.Style != "fake" {
		t.Errorf("handshake.Style: got %q, want %q", w.handshake.Style, "fake")
	}
	if w.handshake.PID == 0 {
		t.Errorf("handshake.PID: want non-zero")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	payload, err := w.dispatch(ctx, []byte(`{"zoom":14}`))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, []byte("fake")) {
		t.Errorf("payload: got %q, want %q", payload, "fake")
	}
}

func TestWorkerErrorResponse(t *testing.T) {
	w, err := spawnWorker(workerArgs{
		binary:           "bash",
		script:           "testdata/fake-worker-error.sh",
		styleID:          "fake",
		handshakeTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.kill()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = w.dispatch(ctx, []byte(`{}`))
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var rerr *RenderError
	if !errors.As(err, &rerr) {
		t.Fatalf("expected *RenderError, got %T: %v", err, err)
	}
	if rerr.Message != "simulated-failure" {
		t.Errorf("message: got %q, want %q", rerr.Message, "simulated-failure")
	}
}

func TestWorkerHangIsKilledOnContextDeadline(t *testing.T) {
	w, err := spawnWorker(workerArgs{
		binary:           "bash",
		script:           "testdata/fake-worker-hang.sh",
		styleID:          "fake",
		handshakeTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer w.kill()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = w.dispatch(ctx, []byte(`{}`))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected error from hung worker, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want context.DeadlineExceeded, got %v", err)
	}
	if elapsed > 1*time.Second {
		t.Errorf("dispatch took too long (%v); worker should have been killed quickly", elapsed)
	}

	// Synchronise on the reaper goroutine kill() launched. After
	// w.exited is closed, the reaper has finished cmd.Wait() and
	// ProcessState is safe to read.
	select {
	case <-w.exited:
	case <-time.After(2 * time.Second):
		t.Fatalf("reaper goroutine did not finish within 2s")
	}
	// ProcessState.Exited() is false for signal-terminated processes
	// (SIGKILL doesn't count as a clean exit per Go's docs), so check
	// only that ProcessState was set — which Wait() guarantees once
	// the process has terminated, regardless of cause.
	if w.cmd.ProcessState == nil {
		t.Errorf("worker process still running after kill")
	}
}
