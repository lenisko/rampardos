package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestContentETag_Deterministic(t *testing.T) {
	a := contentETag("foo/bar/path.png")
	b := contentETag("foo/bar/path.png")
	if a != b {
		t.Fatalf("same path produced different ETags: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, `"`) || !strings.HasSuffix(a, `"`) {
		t.Errorf("ETag should be quoted per RFC 7232, got %q", a)
	}
}

func TestContentETag_PathInvalidates(t *testing.T) {
	a := contentETag("foo")
	b := contentETag("bar")
	if a == b {
		t.Errorf("different paths produced same ETag: %q", a)
	}
}

func TestBumpContentVersion_Invalidates(t *testing.T) {
	before := contentETag("same/path")
	BumpContentVersion()
	// BumpContentVersion stores time.Now().Unix(); same-second calls
	// would not move the epoch. The init-time epoch is serverStartTime,
	// which by the time tests run is at least a few seconds in the past,
	// so the bump is observable.
	after := contentETag("same/path")
	if before == after {
		t.Errorf("BumpContentVersion did not change ETag for same path: %q", before)
	}
}

func TestServedNotModified_Match(t *testing.T) {
	w := httptest.NewRecorder()

	// Simulate the handler: set headers, then a re-request with the
	// matching If-None-Match should be answered with 304 and an empty
	// body.
	setStaticMapCacheHeaders(w, "Cache/Static/abc.png")
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("setStaticMapCacheHeaders did not set ETag")
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "public") {
		t.Errorf("Cache-Control missing 'public': %q", cc)
	}
	if lm := w.Header().Get("Last-Modified"); lm == "" {
		t.Error("Last-Modified not set")
	}

	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest("GET", "/staticmap?style=bg", nil)
	r2.Header.Set("If-None-Match", etag)

	setStaticMapCacheHeaders(w2, "Cache/Static/abc.png")
	if !servedNotModified(w2, r2) {
		t.Fatal("servedNotModified returned false for matching ETag")
	}
	if w2.Code != http.StatusNotModified {
		t.Errorf("status = %d, want %d", w2.Code, http.StatusNotModified)
	}
	if w2.Body.Len() != 0 {
		t.Errorf("304 response should have empty body, got %d bytes", w2.Body.Len())
	}
	// 304 response must still carry the ETag (clients refresh their
	// cache metadata from the 304).
	if w2.Header().Get("ETag") != etag {
		t.Errorf("304 ETag = %q, want %q", w2.Header().Get("ETag"), etag)
	}
}

func TestServedNotModified_Miss(t *testing.T) {
	cases := []struct {
		name        string
		ifNoneMatch string
	}{
		{"empty header", ""},
		{"different etag", `"deadbeefcafef00d"`},
		{"unquoted match", "abcd"},     // ETag without quotes never matches
		{"weak comparison", `W/"abcd"`}, // We use strong comparison; weak doesn't match
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "/staticmap", nil)
			if tc.ifNoneMatch != "" {
				r.Header.Set("If-None-Match", tc.ifNoneMatch)
			}
			setStaticMapCacheHeaders(w, "Cache/Static/different.png")
			if servedNotModified(w, r) {
				t.Fatal("servedNotModified returned true for non-matching ETag")
			}
		})
	}
}
