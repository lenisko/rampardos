package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestDetachRequestTimeoutStripsDeadline pins the pprof wiring
// invariant: the detachRequestTimeout middleware must hide the
// upstream REQUEST_TIMEOUT deadline from wrapped handlers so
// pprof.Profile / pprof.Trace can honour `?seconds=N` past the
// global timeout.
func TestDetachRequestTimeoutStripsDeadline(t *testing.T) {
	var observedDeadline bool
	handler := detachRequestTimeout(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, observedDeadline = r.Context().Deadline()
	}))

	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/profile?seconds=60", nil)
	ctx, cancel := context.WithTimeout(req.Context(), 10*time.Millisecond)
	defer cancel()
	req = req.WithContext(ctx)

	handler.ServeHTTP(httptest.NewRecorder(), req)

	if observedDeadline {
		t.Fatal("handler saw a deadline on ctx — detachRequestTimeout must strip the parent deadline")
	}
}
