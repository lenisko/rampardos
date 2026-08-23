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
| 4 | `mln_runtime_run_blocking` → superseded by `mln_runtime_wait_for_event` (filtered, blocking) | ✅ landed in jfberry@7206a1a (run_blocking dfd7091 was worse, replaced) | partial — removed busy-poll, but per-frame crossings remain | **HIGH** |
| 5 | File-source bypass / custom `ResourceLoader` provider | ❌ cancelled — instrumentation showed 0.09 ms/render | ~0 in our workload | **LOW** for us |
| 6 | Render-update event coalescing in the runtime queue | ✅ landed in jfberry@9b349e1 | reduces raw event spam (cgo `PollEvent` crossings); no per-frame draw reduction | **MEDIUM** |
| 7 | **Blocking render-to-completion primitive** (`mln_map_render_still_blocking`) | ❌ closed — superseded by upstream #392 | see "Upstream outcome" below | — |

## Upstream outcome (2026-07-28)

All of this was taken to upstream as issue
[maplibre/maplibre-native-ffi#280](https://github.com/maplibre/maplibre-native-ffi/issues/280).
Result:

- **#1/#2 (GL defaults)** — our PR #281 merged the owned-texture
  `ContextMode::Unique` change. Upstream then finished the job in #398:
  the borrowed ctor is `Unique` too, and the `glFinish()` question was
  resolved *properly* — `swap()` now syncs only for **caller-owned**
  (borrowed) textures, while **session-owned** textures defer completion
  to acquire-frame. rampardos reads back on the CPU and never acquires,
  so it gets the per-frame `glFinish` removed safely. Our original
  "just delete it" patch was **not** safe (it would have broken
  GPU-interop consumers); the retraction was correct.
- **#4/#6 (wait-for-event, coalescing)** — landed generically in
  upstream #392 as `mln_runtime_pump(runtime, timeout_ms)` (parks the
  owner thread, replaces `run_once`) plus render-update coalescing
  against an unread event at the queue tail.
- **#7 (blocking render)** — our PR #282 was **closed**: "closing for
  #392 which I think solves for the same use case at a lower level".

**Attribution correction.** The #282 pitch led with "~15 round trips
across the language boundary". That over-attributed the win: a cgo call
is ~50-100 ns, so ~60 crossings per render is ~6 µs against a measured
~7 ms delta — three orders of magnitude out. The real mechanisms were
(a) mbgl's frontend `update()` queues to the *runtime's* event queue,
not libuv's, so a non-blocking `run_once` advanced nothing and the
caller had to **sleep** (~15 × ~0.5-1 ms), and (b) rendering per event
redrew successively newer state N times per frame of progress — real GPU
work. Both are what #392 fixes. Our own instrumentation already said
this (`pump_sleep` 22.9 ms, `pump_render_update` 17.9 ms over ~15 draws);
the PR framing did not follow it.

Upstream's open question, and the reason to re-measure: *"I don't think
[the round trips are] resulting in a meaningful impact to performance,
but if you have evidence to the contrary, we can still add the render to
completion primitive."* rampardos is now ported to upstream `main`
(`91ecc920`) so a warm prod grab of `viewport_duration` can answer it
against the recorded baselines: **~24 ms** (blocking primitive),
**~31 ms** (old event pump), **~20 ms** (Node).

Cumulative spike progress: Go p50 scale=1 from **34 ms** (baseline)
→ **~31 ms** (event pump, #1+#2+#3+#4+#6) → **~24 ms** (#7 blocking
render, FFI `2209a5c`). Node baseline is ~20 ms. **Effective parity
reached** on a different machine (Intel VPS) under real load.

**Final warm prod measurement (N=479 scale=1, N=1050 scale=2,
RENDERER_BLOCKING_RENDER=true, MLN_FFI_RESOURCE_LOADER=bypass):**

| metric | event pump | blocking (#7) | Node |
|---|---|---|---|
| scale=1 p50 | ~31 ms | **~24 ms** | ~20 ms |
| scale=1 p99 | ~242 ms | **~136 ms** | — |
| scale=2 p50 | ~35 ms | **~31 ms** | — |
| scale=2 p99 | ~175 ms | **~148 ms** | — |

#7 improved p50 **and** p99 on both scales (no tail regression).
7.5% of scale=1 renders came in ≤10 ms (the warm single-draw path);
an isolated warm `RenderStillBlocking` measured ~9 ms FFI-side.

**Residual ~4 ms to Node:** the floor is mbgl tile-IO + parse/layout
wait (~23 ms cumulative `pump_sleep` on the event path), which #7
does not touch. `MLN_FFI_RESOURCE_LOADER=bypass` is already enabled,
so the ResourceLoader actor hop is already gone — but bypass still
delegates to mbgl's `MBTilesFileSource`, which keeps its own worker
thread **and** non-prepared SQL per query. Node sidesteps both via
`NodeFileSource` → `better-sqlite3` (prepared statements). Closing
this would require an upstream mbgl prepared-statement change or a
Go-side direct-mbtiles reader; not pursued — unconfirmed gain
(SQLite may be only 2-3 ms of the 23 ms), deep work, parity already
met.

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

## #6 — Render-update event coalescing in the runtime queue

**Status:** Landed in `jfberry/maplibre-native-ffi@9b349e1`.

**Change:** The FFI's frontend `update()` (`src/map/map.cpp:304-309`)
pushed a `MLN_RUNTIME_EVENT_MAP_RENDER_UPDATE_AVAILABLE` event on
**every** mbgl `update()` call. Now coalesces: one event per map per
pump cycle until drained, matching mbgl's `HeadlessFrontend::update()`
which coalesces implicitly via `uv_async_send` (idempotent across
multiple sends-before-pump).

**Measured impact (FFI bench, EGL):** byte-identical output;
cold-frame raw events drop 12→5 (z3) / 14→5 (z5); warm frames stay
at 1; `StillImageFinished` always fires.

**Why it's only a partial fix:** This reduces the number of *events*
sitting in the queue, which reduces Go-side `PollEvent` cgo
crossings. It does **not** reduce the number of *draws*: each genuine
progressive frame (tiles arriving) still requires Go to call back in
via `mln_texture_render_update`. rampardos's pump already coalesced
multiple events per drain into one `RenderUpdate()` call, so the
observable rampardos metric (`pump_render_update_count`, which counts
`RenderUpdate` *calls*, not raw events) is unchanged by this. The
real per-frame-crossing cost is addressed by #7.

**Upstream pitch:** Strict alignment with `HeadlessFrontend`'s
coalescing semantics. Strictly additive — caller contract unchanged
("render the latest" per event). Good hygiene regardless of #7.

## #7 — Blocking render-to-completion primitive (KEY ARCHITECTURAL REQUEST)

**Status:** Proposed 2026-05-29. Not yet implemented.

**The architectural problem.** The FFI decomposed mbgl's internal
render loop and lifted the per-frame draw orchestration across the
language boundary into the caller. Stock mbgl drives the whole render
to completion in C++:

```cpp
// platform/default/src/mbgl/gfx/headless_frontend.cpp
void HeadlessFrontend::update(updateParameters_) {
    updateParameters = updateParameters_;
    if (invalidateOnUpdate) asyncInvalidate.send();   // → renderFrame() → renderer->render(), C++-side
}

HeadlessFrontend::RenderResult HeadlessFrontend::render(Map& map) {
    map.renderStill([&](auto e){ result.image = backend->readStillImage(); });
    while (!result.image.valid() && !error)
        util::RunLoop::Get()->runOnce();   // ← every progressive redraw happens in here, C++
    return result;                          // ← finished image, ONE call out
}
```

The FFI's frontend instead does **not** self-draw — `update()`
(`src/map/map.cpp:304-309`) stores the params and pushes an event to
the caller, requiring the caller to call `mln_texture_render_update`
to actually draw:

```cpp
void update(std::shared_ptr<mbgl::UpdateParameters> update) override {
    const std::scoped_lock lock(latest_update_mutex_);
    latest_update_ = std::move(update);
    mln::core::push_runtime_map_event(runtime_, map_, MLN_RUNTIME_EVENT_MAP_RENDER_UPDATE_AVAILABLE);
}
```

So a downstream caller must run the pump itself:
`request_still_image` → loop { `run_once`/`wait_for_event` →
`poll_event` → on `RENDER_UPDATE_AVAILABLE` call
`texture_render_update` } until `STILL_IMAGE_FINISHED`. A cold render
with ~18 progressive frames = ~18 round trips across the boundary,
each with caller-side scheduler overhead (for Go: a goroutine
park/unpark per `wait_for_event`). The Node binding pays **none** of
this — `node_map.cpp:539-548` calls `map->renderStill(cb)` once and
gets exactly one completion callback; the entire update→renderFrame
loop stays in C++ on the libuv loop.

This is the root cause of the whole pump-tuning saga (#4, RunBlocking,
Gosched experiments, WaitForEvent filtering, #6 coalescing): all of it
is overhead created by pulling mbgl's internal loop across the cgo
boundary.

**Proposed C ABI** (add alongside `mln_map_request_still_image`):

```c
/**
 * Renders a still image to completion synchronously, returning only
 * when the still is finished or the timeout expires. Internally runs
 * the runtime's RunLoop to completion (mbgl HeadlessFrontend::render
 * pattern): the frontend self-draws into the attached render target
 * via its invalidate handler — NO per-frame MAP_RENDER_UPDATE_AVAILABLE
 * events are emitted, and the caller does NOT call
 * mln_texture_render_update during this call.
 *
 * On success the finished frame is left in the attached render target
 * exactly as if the caller had driven the pump to STILL_IMAGE_FINISHED;
 * the caller then reads it back with the existing readback API
 * (mln_texture_session_read_premultiplied_rgba8_into etc.).
 *
 * timeout_ms == 0       : sentinel for "no timeout", block until done.
 * timeout_ms  > 0       : return MLN_STATUS_TIMEOUT if not finished in time.
 *
 * Threading: must be called from the runtime owner thread (same
 * constraint as mln_runtime_run_once). Blocks that thread for the
 * full render — intended for a worker-pool-per-thread model where the
 * caller already blocks the thread on the render anyway.
 */
MLN_API mln_status mln_map_render_still_blocking(
    mln_map* map,
    uint64_t timeout_ms
) MLN_NOEXCEPT;
```

Go binding addition:

```go
// RenderStillBlocking renders to completion in C++ with no per-frame
// cgo crossings. Replaces the request_still_image + WaitForEvent +
// PollEvent + RenderUpdate pump loop with one call.
func (m *Map) RenderStillBlocking(timeout time.Duration) error
```

**Implementation sketch.** Mirror `HeadlessFrontend::render`. The FFI
frontend needs a mode flag so its `update()` calls
`asyncInvalidate.send()` (self-draw into the owned texture/offscreen
via the existing `renderFrame`/`texture_render_update` path) instead
of pushing the event, for the duration of a blocking render. Then:

```cpp
auto render_still_blocking(mln_map* map, uint64_t timeout_ms) -> mln_status {
    // enter blocking mode: update() self-draws, no events pushed
    map->frontend->setInvalidateOnUpdate(true);   // or equivalent FFI mode flag
    bool done = false; std::exception_ptr err;
    map->map->renderStill([&](std::exception_ptr e){ if (e) err = e; done = true; });
    const auto deadline = /* now + timeout_ms, or none */;
    while (!done && !err) {
        if (timed_out(deadline)) return MLN_STATUS_TIMEOUT;
        runtime->run_loop->runOnce();   // drives asyncInvalidate → renderFrame internally
    }
    map->frontend->setInvalidateOnUpdate(false);   // restore event-push mode
    if (err) { set_thread_error_from(err); return MLN_STATUS_RENDER_ERROR; }
    return MLN_STATUS_OK;   // frame is in the render target; caller reads back
}
```

The owned-texture / offscreen render target draw already happens in
`renderFrame` for stock `HeadlessFrontend`; the FFI just needs its
texture-session `renderFrame` equivalent invoked from the
`asyncInvalidate` handler rather than from the caller. Estimated a
modest C++ change (mode flag + the blocking entry point) plus the Go
binding.

**Expected impact (to be validated under warm load before rampardos
switches over):**

- *Cold / progressive renders (p99 tail):* large — ~18 boundary round
  trips + ~18 goroutine wakes collapse to 1 call.
- *Warm renders (the ~20 ms p50 target):* modest in absolute terms —
  `pump_render_update_count ≈ 1` warm, so only ~1-2 crossings saved.
  The warm render is dominated by the llvmpipe `renderer->render()`
  draw + readback, which Node pays identically. Any residual warm
  p50 gap to Node after this is **not** the pump — it's GL draw cost
  or caller-side per-request overhead, and must be localized
  separately.
- *Robustness:* removes the scheduler-contention sensitivity that
  made Gosched/RunBlocking pumps fragile under multi-tenant load
  (~18 µs/wake under contention). Fewer goroutine wakes per render →
  less load-dependent jitter even warm.

**Relationship to #6:** Coalescing becomes a no-op *inside* a
blocking render (no events cross to the caller) but stays correct and
useful for the event-driven path. #7 doesn't obsolete #6; it offers a
second, lower-overhead render mode.

**Upstream pitch:** This is the natural "give me a render and wait"
primitive every non-libuv-native binding (Go, Rust, Zig, Swift, C#)
wants, and it already exists inside mbgl as
`HeadlessFrontend::render`. The FFI currently only exposes the
decomposed event-driven pump, which is the right tool when the caller
integrates the runtime as a guest in its own event loop, but forces
per-frame boundary crossings on the common synchronous case. Strictly
additive — no API break; existing `request_still_image` + pump users
are unaffected.

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
