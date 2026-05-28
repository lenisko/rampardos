# FFI changes for upstream proposal

Captures the maplibre-native-ffi changes discovered during the
in-process Go renderer spike (see `2026-05-27-go-renderer-opengl-spike-design.md`).
Each entry records what changed, its measured benefit in rampardos
prod, and the case for proposing it to upstream.

The spike's downstream context is rampardos serving real Pokemon
GO-style traffic (klokantech-basic style, 512×512 and 1024×1024
staticmaps, prod load on a single Linux/amd64 Intel VPS, Mesa
software OpenGL via llvmpipe). All measurements are taken via
rampardos's per-render-breakdown Prometheus histograms (see commit
`1cbedca`) at N≥500 renders per configuration.

## Summary table

| # | Change | Status | Measured benefit (Go p50 scale=1) | Upstream priority |
|---|---|---|---|---|
| 1 | Drop `context.finish()` in `OpenGLTextureRenderableResource::swap()` | ✅ landed in jfberry@8ea3345 | combined w/ #2: −2 ms p50, large p99 win | **HIGH** |
| 2 | `ContextMode::Shared` → `Unique` in `OpenGLTextureBackend` | ✅ landed in jfberry@8ea3345 | combined w/ #1 | **HIGH** |
| 3 | `AttachOpenGLOffscreen` — renderbuffer-backed FBO session variant | ✅ landed in jfberry@856d5ae | ~1-2 ms p50 scale=2; mostly API cleanliness | **MEDIUM** |
| 4 | `mln_runtime_run_blocking` — `uv_run(UV_RUN_ONCE)` exposed | ⏳ requested, not built | expected ~6-10 ms p50 (closes remaining gap) | **HIGH (key)** |
| 5 | File-source bypass / custom `ResourceLoader` provider | ❌ cancelled — instrumentation showed 0.09 ms/render | ~0 in our workload | **LOW** for us |

Cumulative spike progress: Go p50 scale=1 from **34 ms** (baseline)
to **28 ms** (after #1+#2+#3). Node baseline is 20 ms. Expected
after #4: **~16-18 ms**, closing the gap and likely beating Node.

## #1 — Drop `context.finish()` in texture-session `swap()`

**Status:** Landed in `jfberry/maplibre-native-ffi@8ea3345`.

**Change:** In `src/render/opengl/opengl_texture_session.cpp`, the
`OpenGLTextureRenderableResource::swap()` override was:

```cpp
void swap() override { context.finish(); }
```

Patched to:

```cpp
void swap() override {}
```

**Rationale:** mbgl's `HeadlessRenderableResource` (in
`platform/default/src/mbgl/gl/headless_backend.cpp`) has `swap()` as
a no-op. The subsequent `glReadPixels` in the readback path
provides an implicit fence, so the explicit `glFinish()` is a
redundant extra CPU/GPU sync.

**Measured impact:** Combined with #2 (they shipped together in one
commit so can't be separated), p50 scale=2 dropped from 38 ms → 35.6
ms (−6%). p99 scale=2 dropped from 189 ms → 100 ms (−47%). The
p99 win is mostly attributable to this change — `glFinish()` adds
jitter when the GPU work the fence is waiting on stretches into the
high tail.

**Upstream pitch:** This is a strict alignment with mbgl's
`HeadlessBackend` defaults. The FFI's texture session was doing
defensive extra work that mbgl's own offscreen path doesn't do. No
behaviour change for correctness; just removes a perf
pessimisation. One-line patch.

## #2 — `ContextMode::Shared` → `Unique` in `OpenGLTextureBackend`

**Status:** Landed in `jfberry/maplibre-native-ffi@8ea3345` (same
commit as #1).

**Change:** In `src/render/opengl/opengl_texture_session.cpp`, both
`OpenGLTextureBackend` constructors initialised
`mbgl::gl::RendererBackend` with `mbgl::gfx::ContextMode::Shared`.
Patched to `Unique`.

**Rationale:** `Shared` mode tells mbgl's `gl::Context` to assume
external code mutates GL state between draws, so it cannot rely on
its internal "this state is already current" cache. The Context
re-issues `glBindBuffer`, `glUseProgram`, `glActiveTexture`, etc.
on every state change rather than skipping no-ops. For a render
with many draw calls (anything label-heavy on software GL) this
multiplies state-change traffic — often the dominant CPU cost.

mbgl's `HeadlessBackend` default is `Unique`. The FFI's texture
session owns its GL context exclusively (the caller's borrowed
`share_context` is never mutated externally between renders), so
`Unique` is semantically correct.

**Measured impact:** Combined with #1, see above. Bench numbers
from the upstream FFI's go-readback example with these two changes
together: +75% FPS (432 → 755 FPS), p50 1.99 ms → 1.18 ms.

**Upstream pitch:** Same as #1 — alignment with mbgl
`HeadlessBackend` defaults. Should be the default for any
session-type backend that owns its GL context exclusively (which is
all of them currently). One-arg change. If there's a concern about
borrowed-context backends, leave Shared as the default for those
and switch only owned-texture/owned-offscreen.

## #3 — `AttachOpenGLOffscreen` renderbuffer-backed session variant

**Status:** Landed in `jfberry/maplibre-native-ffi@856d5ae`.

**Change:** New session type alongside `AttachOpenGLOwnedTexture`
and `AttachOpenGLBorrowedTexture`. Uses a framebuffer with
**renderbuffer** color attachment (not texture) plus depth/stencil
renderbuffer — matching `mbgl::HeadlessBackend`'s layout exactly.
No texture handle is exposed; `AcquireOpenGLTextureFrame` is not
applicable.

C ABI: `mln_opengl_offscreen_descriptor { Extent, Context }` +
`mln_opengl_offscreen_attach`. Go binding:
`Map.AttachOpenGLOffscreen(OpenGLOffscreenDescriptor)`.

**Rationale:** For consumers who only need CPU pixel readback (via
`ReadPremultipliedRGBA8Into`), the texture-attached FBO costs
unnecessary detile/blit overhead in Mesa's readback path. The
texture handle the owned-texture API exposes is unused.

**Measured impact:** ~1-2 ms p50 improvement in rampardos at
scale=2 (34.2 ms → ~33 ms; within noise but consistent across N
samples). Bench numbers showed only 0.4% (1.18 ms → 1.19 ms) but
real-world tile/label workloads benefit slightly more than trivial
bench scenes.

**Upstream pitch:** Smaller than expected as a perf win, **stronger
as an API improvement.** Downstream consumers (Go, Rust, Zig
bindings; future C# binding) shouldn't have to know about
texture-vs-renderbuffer to get the readback fast path. The
`Offscreen` session variant is the natural name and contract: "I
want pixels back, I don't care about the GL object." For symmetry
worth adding similar `OpenGLSurfaceOffscreen` if there's a
window-system-surface analogue; if not, the texture-session pair
covers everything.

## #4 — `mln_runtime_run_blocking` (KEY OUTSTANDING REQUEST)

**Status:** Requested 2026-05-28; not yet implemented.

**Proposed C ABI** (add to `include/maplibre_native_c/runtime.h`):

```c
/**
 * Blocks in the underlying event loop until at least one event is
 * processed or the timeout expires. Wraps uv_run(loop, UV_RUN_ONCE)
 * with a uv_timer arming the timeout.
 *
 * timeout_ms == 0       : poll once and return immediately
 *                         (equivalent to mln_runtime_run_once).
 * timeout_ms == UINT64_MAX (sentinel): block indefinitely.
 * timeout_ms > 0 finite : block up to timeout_ms then return.
 *
 * *out_had_event is true if any event was processed, false if the
 * deadline expired with no work.
 *
 * Threading: must be called from the runtime owner thread (same
 * constraint as mln_runtime_run_once).
 */
MLN_API mln_status mln_runtime_run_blocking(
    mln_runtime* runtime,
    uint64_t timeout_ms,
    bool* out_had_event
) MLN_NOEXCEPT;
```

Go binding addition (`bindings/go/runtime.go`):

```go
func (runtime *RuntimeHandle) RunBlocking(timeout time.Duration) (bool, error)
```

**Rationale:** The existing `mln_runtime_run_once` is non-blocking
(`uv_run(UV_RUN_NOWAIT)` semantics). Downstream callers polling it
must either busy-loop (burns CPU, context-switches with other
goroutines under load) or sleep between calls (kernel timer
resolution adds ~500 µs avg wake-up latency per event). Neither
matches what libuv-based consumers (e.g. Node binding) get for free
via `uv_run(UV_RUN_ONCE)`.

Rampardos instrumented prod measurement (N=1014 scale=1, robust)
attributed total render time:

```
total          31.0 ms
  pump sleep   14.8 ms  ← dominant
  RenderUpdate 12.3 ms
  readback      2.7 ms
  first event   0.09 ms
  other         ~1 ms
```

The 14.8 ms of "sleep" is mbgl waiting for worker threads to
deliver parsed tile data. **The wait itself is unavoidable** — Node's
binding pays the same. The wake-up latency is what's avoidable:
when mbgl emits an event mid-sleep, the kernel timer doesn't fire
until end of tick (~1 ms granularity), while libuv on Node wakes
from `epoll_wait` instantly. ~12 events × ~500 µs miss-rate
≈ 6 ms per render of avoidable latency.

**Implementation:** mbgl's `mbgl::util::RunLoop::run()` already
exposes a blocking mode via `RunLoop::Mode::Once`. The FFI's
`mln_runtime_run_once` uses `UV_RUN_NOWAIT`; the blocking variant
uses `UV_RUN_ONCE` with a `uv_timer_t` for the timeout. Estimated
~30 lines of C++ + matching Go binding.

**Expected impact:** Rampardos p50 should drop from ~28 ms to ~16-18
ms, closing the entire remaining gap to Node's 20 ms baseline and
likely beating it. Per-render CPU drops because `epoll_wait`
actually parks the thread instead of busy-polling.

**Upstream pitch:** This is the **single most impactful change** for
any downstream consumer that drives the renderer on a custom thread
(Go, Rust, Zig, Swift, C#, anyone embedding the FFI in a non-libuv
event loop). The existing `RunOnce` is fine when the caller has
their own event loop and integrates the runtime as a guest, but for
"give me a render and wait" patterns it's missing the matching
wait primitive. Strictly additive — no API break, no behaviour
change for existing `RunOnce` users.

## #5 — File-source bypass / custom `ResourceLoader` provider

**Status:** Cancelled 2026-05-28 — instrumentation showed
file-source contribution is 0.09 ms/render. Not worth pursuing.

**Background:** Earlier hypothesis (from source-level investigation
into the Go-vs-Node gap) was that mbgl's standard `MainResourceLoader`
adds significant per-render latency via actor-mailbox dispatch from
mbgl-main → ResourceLoaderThread → per-source worker thread (e.g.
`MBTilesFileSource` runs SQLite on its own thread) → response back.
Estimated cost: 2-6 ms cumulative per render at 25 resource
requests.

Node binding sidesteps this entirely via `NodeFileSource` (in
`platform/node/src/node_map.cpp:208-216`) which **replaces** the
`FileSourceType::ResourceLoader` factory in `FileSourceManager` with
a synchronous JS callback path. The JS callback uses `better-sqlite3`
to query tiles inline on the mbgl-main thread — zero actor hops,
zero worker thread context switches.

Proposed equivalent for the FFI: a new C ABI letting downstream
consumers register a `ResourceLoader` provider (broader than the
existing network-only `resource_provider`) that intercepts all
request schemes. The Go callback would handle mbtiles/file/asset
queries inline, mirroring `NodeFileSource`'s pattern.

**Why cancelled:** When the per-render-breakdown instrumentation
(commit `1cbedca`) was deployed and ran for N=1014 renders, the
`pump_time_to_first_event_seconds` metric (which captures the time
from `RequestStillImage` to mbgl's first emitted event, including
all file-source work) measured at:

- scale=1: **0.09 ms per render** (mean over N=1014)
- scale=2: **0.04 ms per render** (mean over N=3440)

That's the total file-source contribution per render including
actor hops, SQLite parse-compile-execute, and the
`MainResourceLoader` waterfall. The cache hit path dominates as
predicted — after warmup, most tile/glyph/sprite requests hit
mbgl's in-memory cache and never reach the file source at all.

A bypass would save ~0.1 ms per render. Not worth the API surface
or implementation work.

**Upstream relevance:** Could still be worth proposing for use
cases with cold-cache or very-many-tile workloads (offline render
farms, batch tile pre-rendering), where file-source work might
actually be the bottleneck. **Not part of our spike's upstream pitch
based on rampardos's measured workload.**

## Suggested PR ordering for upstream

If we send these to upstream as PRs, suggested sequence:

1. **First PR: #1 + #2 combined.** Minimal patch (two-line change in
   one file), both clearly bugfix-flavoured ("align texture session
   with HeadlessBackend defaults"). Easy review, easy merge. Sets
   up the cleanest baseline for everything else.

2. **Second PR: #4 (blocking RunOnce).** Most strategic — unblocks
   downstream bindings that aren't libuv-native. Well-bounded
   change (one new C function, one new Go binding method). May need
   discussion on sentinel timeout value semantics
   (`UINT64_MAX` vs negative-as-infinite vs separate function).

3. **Third PR: #3 (offscreen session variant).** Larger API surface
   addition. Discussion needed on naming (`AttachOpenGLOffscreen`
   vs other names) and whether to add equivalent
   surface/borrowed-renderbuffer variants for symmetry. Less
   urgent — perf win is small, API improvement is the real benefit.

## Spike configuration tested

For reference, rampardos's final configuration with all in-flight
fixes:

- `MLN_FFI_REV = jfberry/maplibre-native-ffi@856d5ae` (items 1+2+3)
- Go renderer uses `AttachOpenGLOffscreen` (commit
  `rampardos@6277680`)
- Pump uses `time.Sleep(100µs)` adaptive (only on idle iterations;
  see commit `rampardos@28aa036`)
- EGL Desktop GL 3.3 Compatibility Profile via Mesa surfaceless
  platform
- All in `rampardos-go-renderer` branch `feat/in-process-go-renderer`
