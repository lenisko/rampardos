package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/lenisko/rampardos/internal/version"
)

// serverStartTime is the Last-Modified value stamped on every cacheable
// response. Stable across the process lifetime so http.ServeContent's
// If-Modified-Since path also short-circuits to 304 (belt-and-braces
// for clients that omit If-None-Match), and all responses share a
// single modtime regardless of when they were generated.
var serverStartTime = time.Now().UTC().Truncate(time.Second)

// contentVersionEpoch participates in the ETag namespace alongside the
// build SHA. Bumped on any operation that changes the bytes a
// previously-cached URL would produce — currently from the SIGHUP
// handler after ReloadStyles. Clients holding a cached response from
// a prior epoch see an ETag mismatch on revalidation and re-download;
// new requests get the new namespace immediately.
var contentVersionEpoch atomic.Int64

func init() {
	contentVersionEpoch.Store(time.Now().UnixNano())
}

// BumpContentVersion advances the content-version epoch. Call after
// any operation that may change the bytes a previously-cached URL
// would produce (style reload, dataset reload). Cheap; safe to call
// from any goroutine.
//
// Uses UnixNano (not Unix) so two reloads in the same second still
// produce distinct epochs — clients with cached state from between
// the two reloads will revalidate cleanly.
func BumpContentVersion() {
	contentVersionEpoch.Store(time.Now().UnixNano())
}

// contentETag returns an HTTP-compatible (quoted) ETag for a content-
// addressable path. The contract: same path + same version namespace
// ⇒ same ETag. This is what makes If-None-Match serve 304 without
// rendering or even checking the disk cache; the URL deterministically
// describes content per CLAUDE.md ("staticMap.Path() stable across
// process lifetime"), so an ETag match means the client already has
// the bytes the server would produce.
//
// Folding version.GitCommit + contentVersionEpoch into the digest
// guarantees that binary upgrades and live reloads invalidate the
// caller's cache: same URL, different epoch ⇒ different ETag ⇒ full
// re-download.
func contentETag(path string) string {
	h := sha256.New()
	h.Write([]byte(version.GitCommit))
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatInt(contentVersionEpoch.Load(), 10)))
	h.Write([]byte{0})
	h.Write([]byte(path))
	// 12 hex bytes = 96 bits of namespace; collisions are not a
	// security boundary here, so the short form keeps headers compact.
	return `"` + hex.EncodeToString(h.Sum(nil)[:12]) + `"`
}

// setStaticMapCacheHeaders sets ETag, Last-Modified and Cache-Control
// for a content-addressable response. Call before the conditional-GET
// check so the response carries the caching directives even when we
// short-circuit to 304 (clients store/refresh their cache from 304
// responses too).
func setStaticMapCacheHeaders(w http.ResponseWriter, path string) {
	w.Header().Set("ETag", contentETag(path))
	w.Header().Set("Last-Modified", serverStartTime.Format(http.TimeFormat))
	// public — explicit so shared caches don't fall back to "private" defaults.
	// max-age=31536000 (1 year) — bytes are content-addressed (URL → bytes
	//   is stable until BumpContentVersion rotates the epoch on a style/
	//   dataset reload). 1y is the conventional "forever" value for
	//   content-addressable URLs; CDNs won't ask us for a year, then
	//   revalidate to a 304 and reset the window. The previous 7-day
	//   value had CDNs revalidating 52× as often for no semantic reason.
	// stale-while-revalidate=86400 — proxies serve a stale image while
	//   refreshing in background. Reduces tail latency under cache-miss
	//   stampedes.
	// stale-if-error=604800 — keep serving stale for a week if origin
	//   errors. Bot-friendly resilience.
	// Note: deliberately not using `immutable` even though it would let
	//   CDNs skip revalidation entirely — `immutable` would lie when
	//   BumpContentVersion rotates the epoch (clients keep serving old
	//   bytes forever without ever asking us). Long max-age + ETag
	//   revalidation gives us "ask once a year, get 304, extend" instead.
	w.Header().Set("Cache-Control", "public, max-age=31536000, stale-while-revalidate=86400, stale-if-error=604800")
}

// servedNotModified short-circuits to 304 Not Modified when the
// client-supplied If-None-Match matches the ETag we just set. Returns
// true if 304 was written; caller should return immediately.
//
// Must be called AFTER setStaticMapCacheHeaders so the ETag header is
// present (we read it back from w.Header() rather than recomputing).
//
// Why this is the whole game: a client with a matching ETag already
// holds bytes equivalent to what we would render. Returning 304
// without rendering, encoding, or even consulting the disk cache is
// the win — revalidation cost collapses from "full render path" to
// "string compare."
func servedNotModified(w http.ResponseWriter, r *http.Request) bool {
	etag := w.Header().Get("ETag")
	if etag == "" {
		return false
	}
	if r.Header.Get("If-None-Match") != etag {
		return false
	}
	w.WriteHeader(http.StatusNotModified)
	return true
}
