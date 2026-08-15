# Converting the Go renderer to the FFI native-executor model (PR 631)

**Status:** design accepted, plan at
`docs/superpowers/plans/2026-08-15-ffi-native-executor-conversion.md`
**Upstream:** https://github.com/maplibre/maplibre-native-ffi/pull/631
("bring execution model into the native core"), head commit
`422f853f6cf8adaa4d0a878597fbd48c15dd1c08` on branch
`t3code/native-executor` in the main maplibre/maplibre-native-ffi repo
(so `go get @<sha>` resolves through the module proxy).
**Our current pin:** `92e6736979d5` (#530), an ancestor of the PR head.
The conversion absorbs pin→PR-head in one move: ~6.4k insertions /
7.9k deletions in `bindings/go` alone.

## What the PR changes

The old binding made the host pump the runtime: every native call for a
Runtime had to originate on the OS thread that created it, and the
worker looped `Pump → PollEvent → RenderUpdate` until a still-image
event arrived. PR 631 moves execution into the native core:

- **Runtime, map, and camera handles are goroutine-safe.** Native
  workers make progress autonomously (style load, tile IO, layout);
  there is no `Pump` and no `PollEvent`.
- **Commands** (`SetStyleURL`, `JumpTo`, `Resize`, style mutations)
  return runtime-wide monotonic command IDs `(uint64, error)` and are
  executed in order by the native executor.
- **Operations** replace fire-and-observe calls: `RequestStillImage()`
  now returns `*OperationHandle[struct{}]` with `Poll/Wait/Cancel/
  Status/Diagnostic/Take/Release`. Attach, resize, readback, and detach
  are all operations.
- **Render sessions keep a driver.** Graphics calls remain
  thread-affine. For EGL contexts, both surface and owned-texture
  targets **require `MLN_RENDER_DRIVER_CALLER_GRAPHICS_THREAD`**
  (`include/maplibre_native_c/texture.h`: "WGL, EGL, and existing WebGL
  contexts require the caller driver"). The core-worker driver is only
  available to transferred WebGL canvases, so our workers keep their
  pinned OS threads and now call `ServiceDriverWork` to execute queued
  draws/readbacks.
- **Frames are demand-driven.** `RequestFrame(FrameDemand{Token,...})`
  queues a demand; `ServiceDriverWork` executes it; results arrive via
  `DrainFrameResults` with a per-demand disposition
  (`RenderResultRendered`, `NoUpdate`, `SizePending`, ...).
- **Notifications replace the pump's park.** The runtime accepts a
  callback (`SetNotificationCallback`) that may fire from any native
  thread and coalesces; the receiver calls `DrainReady` and then drains
  the typed queues (runtime events, frame results, driver work).
- **Events still exist** (`DrainEvents` on the runtime) for
  `MapLoadingFailed`, `MapRenderError`, `MapRenderUpdateAvailable`,
  etc. `MapOptions.EventMask` selects which types a map queues.

The canonical still-image flow is `docs/snippets/c/still-image.c` in the
PR branch: create map (operation) → attach owned texture with caller
driver (operation, serviced to completion) → `set_style_url` (command) →
`request_still_image` (operation) → loop { service driver work, drain
frame results, poll still, re-demand } → readback (operation) → detach
(operation).

## Decisions

**D1 — Driver: `RenderDriverCallerGraphicsThread`.** Not a choice: EGL
targets require it. Each worker's pinned OS thread is the session's
caller graphics thread.

**D2 — Context ownership: keep `Shared` (the zero value) and keep
`egl_linux.go` unchanged.** Shared ownership reproduces today's
semantics exactly: our 3.3-Compatibility context stays the share-group
anchor and stays current on the worker thread; the session makes its
own context current around servicing and restores ours. Our config
already requests `EGL_PBUFFER_BIT`, which OpenGL texture sessions
require. `OpenGLContextDescriptor` gained an `Ownership` field and
`EGLContextDescriptor` gained `ClientAPI`; both zero-value correctly
for shared ownership, so the descriptor construction compiles as-is.
*Deferred follow-up:* `OpenGLContextOwnershipDedicated` would let the
session own the thread's context (make-current once, no per-service
save/restore) and let us delete context creation from `egl_linux.go`
(keep display+config+`ClientAPI: OpenGLClientAPIGL`); do that as a
separate measured experiment, not in this conversion — the GL context
setup was performance-tuned against Node and must not change silently.

**D3 — Topology: unchanged.** One Runtime + Map + RenderSession per
worker, workers still `runtime.LockOSThread`-pinned, lazy
`(style, scale)` pools, two-level concurrency, elastic grow/reap. The
new model would allow one shared Runtime with N maps, but that changes
failure isolation and executor scheduling in ways we can't measure
until the basic conversion is soaking. Keep the delta mechanical.

**D4 — Worker loop: notification-parked service loop.** The
`Pump/PollEvent` loop becomes: park on a buffered-1 wake channel that
the runtime notification callback feeds with a non-blocking send; on
wake, `DrainReady` (consumes and re-arms the edge-triggered ready
state), `ServiceDriverWork`, `DrainEvents` (fail fast on
`MapLoadingFailed`/`MapRenderError`), `DrainFrameResults` (track
rendered, re-demand on non-rendered dispositions), `Poll` the awaited
operation. Parks are defensively capped (100 ms) so a missed edge
degrades to a slow poll instead of a stall; raise the cap after soak if
notifications prove reliable.

**D5 — Still completion comes from the operation, not events.** Trim
the map's `EventMask` to `MapRenderUpdateAvailable | MapLoadingFailed |
MapRenderError`. Keep the old "finished but never rendered a frame is
an error" invariant by tracking rendered dispositions for our demand
tokens.

**D6 — Cancellation replaces the settle dance, poison stays as a
backstop.** On timeout/ctx-cancel with a still outstanding:
`still.Cancel()`, then service (background context, bounded by
`settleTimeout`) until the operation reaches a terminal state. Native
cancellation is expected to settle without needing host frames, making
the old `settleAbandonedStill` spiral mostly dead code — but a worker
whose still cannot reach terminal state within the settle budget is
still poisoned and retired, same as today.

**D7 — Readback returns an owned copy.** `ReadPremultipliedRGBA8Start`
→ `Take()` yields `TextureReadback{Data []byte, Info}` (binding-copied).
The reusable `w.buf` dies; unpremultiply straight from `tr.Data` into
the `image.NRGBA`. One extra allocation per render; if it shows in GC
profiles, an upstream read-into variant is the fix, not host-side
pooling.

**D8 — Metrics keep their names.** `RecordRendererPumpBreakdown`
continues to be emitted with reinterpreted inputs: iterations = service
loop turns, sleep = park time, timeToFirstEvent = first
event/frame-result observed, renderUpdate total/count = time in
`ServiceDriverWork` / frames rendered. Dashboards survive; the Grafana
panel descriptions can be touched up later.

**D9 — Build: from-source FFI, pinned to the PR head.** PR 631 has no
published artifact and the `unstable-native-snapshot` tag tracks main,
so resurrect the pre-`e5c7a00` `mln-ffi-build` compile stage
(`git show e5c7a00^:Dockerfile`) with
`MLN_FFI_REV=422f853f6cf8adaa4d0a878597fbd48c15dd1c08`. Keep the
go.mod-pseudo-version-vs-`MLN_FFI_REV` consistency check in
`rampardos-build`. When the PR merges and the snapshot artifact moves
past it, revert to artifact consumption (that revert is the exact
inverse of this commit, which is why it's kept isolated).

## Risks

- **Perf regression.** The pump loop was tuned to ~Node parity. New
  costs: session context make-current/restore per service call (shared
  ownership), readback copy, notification latency vs `Pump`'s direct
  park. Mitigation: the pump-breakdown metrics keep flowing, so the
  prod A/B is one dashboard away; scale=1 and scale=2 must both be
  exercised (CLAUDE.md regression hotspot).
- **PR churn.** 631 is open; the head may advance. Everything pins the
  one SHA; bumping is `go get @<new-sha>` + the `MLN_FFI_REV` ARG.
- **Notification semantics.** If the ready-state edge doesn't re-arm
  the way we expect, the capped park bounds the damage to +100 ms on a
  starved wake; the soak test watches p99 for exactly this.
- **Unknowns in cancel-while-loading.** If a cancelled still can't
  reach terminal state without frames, the settle loop still drives
  frames (it services with demands enabled), and the poison backstop
  catches the remainder. Watch `renderer_worker_replacement_total{reason=
  "error"}` in soak.

## As-built amendments (2026-08-15, found on the real backend)

- **D-resize (new):** a session `ResizeStart` cannot be awaited on its
  own in static mode — nothing publishes a map update without a pending
  still, so the ordered resize work parks forever (and, being ordered,
  blocks detach behind it). As built, `renderOne` starts the resize and
  lets the following still's demand loop drive it to completion,
  verifying the resize operation's terminal status after the still.
- **D4 amendment (supersedes an earlier needs_repaint theory):** in
  static mode mbgl never emits frame-finished observer callbacks (they
  are gated to Continuous), and the session scheduler that receives
  mbgl worker continuations (tile parses, placement results) has no
  notification hook installed upstream — only an executed frame demand
  drains it (`render_session_common.cpp`: "or a frame with no update
  to render strands them"). The loop therefore issues a keep-alive
  demand after every park while a still is in flight (park cap 5 ms
  during stills, 100 ms otherwise), and does NOT re-demand instantly
  on non-rendered dispositions. This mirrors the keep-alive re-demand
  in upstream's own still-image.c example, which is load-bearing, not
  a poll.
- **DrainFrameResults** reports `ErrNotReady` for an empty queue;
  treated as an empty drain.
- The Zig cross toolchain and the mise sync scripts
  (`sync-submodules`, `sync-rustls-platform-verifier`) are required by
  the PR-era FFI build — D9's "resurrect the pre-e5c7a00 stage" needed
  those two updates.
