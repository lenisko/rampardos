package renderer

import (
	"context"
	"errors"
	"sync"
)

// Fake is a test double for Renderer. Callers outside this package
// use it to unit-test code that takes a Renderer without spawning
// real Node processes.
//
// Default behaviour: Render and RenderViewport return canned bytes
// (one byte set per StyleID or a global default). Override with
// RenderFn / RenderViewportFn for per-test behaviour.
type Fake struct {
	mu sync.Mutex

	RenderFn         func(ctx context.Context, req Request) ([]byte, error)
	RenderViewportFn func(ctx context.Context, req ViewportRequest) ([]byte, error)
	ReloadFn         func(ctx context.Context) error
	CloseFn          func() error

	// Calls records every Render and RenderViewport call in order.
	//
	// Direct reads of this slice are only safe when no render is in
	// flight (e.g. after the action under test has returned and all
	// spawned goroutines have joined). For concurrent scenarios, use
	// Snapshot() to obtain a lock-protected copy.
	Calls []FakeCall

	// Canned is the byte slice returned by default when no *Fn is set.
	// If nil, default is a single 0x00 byte.
	Canned []byte

	closed bool
}

// compile-time check that Fake satisfies the Renderer interface.
var _ Renderer = (*Fake)(nil)

// FakeCall is one recorded call against the fake.
type FakeCall struct {
	Kind     string // "Render" or "RenderViewport"
	Request  Request
	Viewport ViewportRequest
}

func (f *Fake) Render(ctx context.Context, req Request) ([]byte, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, FakeCall{Kind: "Render", Request: req})
	f.mu.Unlock()
	if f.RenderFn != nil {
		return f.RenderFn(ctx, req)
	}
	if f.Canned != nil {
		return append([]byte(nil), f.Canned...), nil
	}
	return []byte{0x00}, nil
}

func (f *Fake) RenderViewport(ctx context.Context, req ViewportRequest) ([]byte, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, FakeCall{Kind: "RenderViewport", Viewport: req})
	f.mu.Unlock()
	if f.RenderViewportFn != nil {
		return f.RenderViewportFn(ctx, req)
	}
	if f.Canned != nil {
		return append([]byte(nil), f.Canned...), nil
	}
	return []byte{0x00}, nil
}

func (f *Fake) ReloadStyles(ctx context.Context) error {
	if f.ReloadFn != nil {
		return f.ReloadFn(ctx)
	}
	return nil
}

func (f *Fake) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	f.mu.Unlock()

	if f.CloseFn != nil {
		return f.CloseFn()
	}
	return nil
}

// Snapshot returns a copy of Calls under the mutex. Safe to call
// concurrently with in-flight Render / RenderViewport invocations.
func (f *Fake) Snapshot() []FakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FakeCall, len(f.Calls))
	copy(out, f.Calls)
	return out
}

// ErrNotConfigured is a sentinel that tests can return from a Fake's
// override hooks (RenderFn, RenderViewportFn, etc.) to signal
// intentional failure. The fake itself never returns it; tests are
// free to return any error they like, but this constant is provided
// as a convenience for the common "simulate a misconfigured
// renderer" case.
var ErrNotConfigured = errors.New("renderer: fake not configured")
