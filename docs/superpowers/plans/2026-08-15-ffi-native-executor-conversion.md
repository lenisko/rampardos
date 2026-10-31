# FFI Native-Executor Conversion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Convert the in-process Go renderer from the host-pumping maplibre-native-ffi binding (pin `92e6736979d5`) to the native-executor binding from upstream PR 631 (head `422f853f6cf8adaa4d0a878597fbd48c15dd1c08`).

**Architecture:** Worker topology is unchanged (per-`(style,scale)` pools, OS-thread-pinned workers, elastic grow/reap, two-level concurrency). Inside each worker, the `Pump/PollEvent/RenderUpdate` loop is replaced by a notification-parked service loop driving `OperationHandle`s: still image, resize, readback, attach, and detach are all operations; frames are demand-driven via `RequestFrame` + `ServiceDriverWork` + `DrainFrameResults`; map/runtime calls are goroutine-safe commands.

**Tech Stack:** Go 1.26, cgo, `github.com/maplibre/maplibre-native-ffi/bindings/go`, EGL/Mesa (Linux-only renderer), Docker multi-stage build.

**Spec:** `docs/superpowers/specs/2026-08-15-ffi-native-executor-conversion.md`

## Global Constraints

- FFI pin everywhere: `422f853f6cf8adaa4d0a878597fbd48c15dd1c08` (Dockerfile `ARG MLN_FFI_REV`, Makefile `MLN_FFI_REV ?=`, `scripts/fetch-mln-ffi.sh` default, go.mod pseudo-version suffix `422f853f6cf8`).
- Render driver must be `maplibre.RenderDriverCallerGraphicsThread` — EGL targets reject the core-worker driver.
- `egl_linux.go` is not modified (shared context ownership is the descriptor's zero value; the 3.3-Compatibility context choice is performance-tuned and must not change).
- Worker goroutines stay `runtime.LockOSThread`-pinned; all `ServiceDriverWork`/session-graphics calls happen only on the worker's own thread.
- Public `Renderer` interface, pool config env vars, and Prometheus metric names/labels are unchanged.
- The only compile+test environment for this package is Docker: `docker build --target rampardos-test .` (macOS `go test ./...` silently skips the Linux-only renderer).
- Do not modify files under `docs/superpowers/specs/`.

## API mapping reference (old pin → PR 631)

Verified against the PR branch checkout; executors should trust this table over memory.

| Old (in our code today) | New |
|---|---|
| `maplibre.NewRuntime()` | unchanged signature; handle now goroutine-safe |
| `rt.Pump(d)` / `rt.PollEvent()` | gone — `rt.SetNotificationCallback(func())`, `rt.DrainReady() ([]NotificationEndpoint, error)`, `rt.DrainEvents(max) (RuntimeEventBatch, error)`; batch has `.Events []RuntimeEvent` (`Type`, `Message`) and `.RemainingCount` |
| `rt.NewMapWithOptions(MapOptions{Width, Height, ScaleFactor, Mode})` | same, plus `EventMask RuntimeEventMask` field (zero mask = no events; `NewMapOptions(...)` = all) |
| `m.SetStyleURL(url) error` | `m.SetStyleURL(url) (uint64, error)` — command ID |
| `m.JumpTo(cam) error` | `m.JumpTo(cam) (uint64, error)`; `CameraOptions{}.WithCenter/WithZoom/WithBearing/WithPitch` builders unchanged |
| `m.RequestStillImage() error` | `m.RequestStillImage() (*maplibre.OperationHandle[struct{}], error)` |
| `m.AttachOpenGLOwnedTexture(desc) (*RenderSessionHandle, error)` | `m.AttachOpenGLOwnedTexture(desc, opts) (*RenderSessionHandle, *OperationHandle[struct{}], error)` where `opts` is `RenderSessionAttachOptions{Driver, RequestedTextureRingDepth}` |
| `sess.RenderUpdate() (bool, error)` | gone — `sess.RequestFrame(FrameDemand) error` + `sess.ServiceDriverWork(maxWork) (int, error)` + `sess.DrainFrameResults(max) (*RenderFrameBatch, error)`; `batch.Results() ([]RenderFrameResult, error)`, `batch.Close()`; result has `.Token`, `.Disposition` (`RenderResultRendered`, `RenderResultNoUpdate`, `RenderResultSizePending`, `RenderResultTargetNotReady`, `RenderResultSuperseded`, `RenderResultDeadlineMissed`) |
| `sess.Resize(extent) error` | `sess.ResizeStart(extent) (*OperationHandle[struct{}], error)` — the operation "applies the extent and updates the map viewport" (same coupled semantics as old Resize) |
| `sess.ReadPremultipliedRGBA8Into(buf) (info, error)` | `sess.ReadPremultipliedRGBA8Start() (*OperationHandle[TextureReadback], error)`; `op.Take()` → `TextureReadback{Data []byte, Info TextureImageInfo{Width, Height, Stride, ByteLength}}` |
| `sess.Close()` | still exists; proper teardown is `sess.DetachStart()` (operation, needs servicing) then `Close()`; `sess.Abandon()` is the no-graphics-work escape hatch |
| — | `OperationHandle[T]`: `Poll() (bool, error)`, `Wait(timeout) (bool, error)`, `Cancel() error`, `Status() (int32, error)` (0 = OK), `Diagnostic() (string, error)`, `Take() (T, error)`, `Release()`. `Release()` on a pending op requests cancellation. Never `Wait()` on an op that needs this thread's driver servicing to complete — that deadlocks; use the service loop. |
| Event types | `RuntimeEventMapRenderUpdateAvailable`, `RuntimeEventMapLoadingFailed`, `RuntimeEventMapRenderError` unchanged; masks `RuntimeEventMaskMapRenderUpdateAvailable`, `RuntimeEventMaskMapLoadingFailed`, `RuntimeEventMaskMapRenderError` (runtime.go:414–417) |
| `maplibre.SupportedRenderBackends()` | unchanged |
| `sess.Capabilities()` | new: `(RenderSessionCapabilities{Driver, TextureRingDepth, Flags}, error)`; check `Flags` contains `RenderSessionCapabilityReadback` at init |

---

### Task 1: Dockerfile — build the FFI from source at the PR head

**Files:**
- Modify: `Dockerfile` (replace the artifact-download `mln-ffi-build` stage, bump `ARG MLN_FFI_REV`)
- Modify: `Makefile:36` (`MLN_FFI_REV ?=`)
- Modify: `scripts/fetch-mln-ffi.sh:25` (default rev; add a loud comment that no published artifact exists for a PR pin, so local Linux dev must build from source or extract from the Docker stage)

**Interfaces:**
- Produces: image stage `mln-ffi-build` exporting `/ffi/install/lib/libmaplibre-native-c.so` + `/ffi/install/share/pkgconfig/maplibre-native-c.pc` built from commit `422f853f6cf8adaa4d0a878597fbd48c15dd1c08`. Later tasks' Docker verification depends on this stage.

- [ ] **Step 1: Recover the old from-source stage**

Run: `git show e5c7a00^:Dockerfile > /tmp/old-dockerfile-ref.txt` and locate the `FROM ubuntu:24.04 AS mln-ffi-build` stage (it clones `MLN_FFI_REPO`, checks out `MLN_FFI_REV`, installs the apt toolchain + pinned CMake + rustup, then configures/builds/installs to `/ffi/install`). Copy that whole stage (including its `ARG MLN_FFI_REPO`, `ARG MLN_CMAKE_VERSION`, `ARG MLN_RUST_VERSION`, `ARG MLN_CARGO_ABOUT_VERSION` lines) over the current download-based `mln-ffi-build` stage in `Dockerfile` (currently lines 77–109). Keep the current stage's final `test -f` assertions if the old stage lacks them.

- [ ] **Step 2: Bump the pin in all three places**

In `Dockerfile` line 6: `ARG MLN_FFI_REV=422f853f6cf8adaa4d0a878597fbd48c15dd1c08`. Same value in `Makefile` (`MLN_FFI_REV ?= …`) and `scripts/fetch-mln-ffi.sh` (`MLN_FFI_REV="${MLN_FFI_REV:-…}"`). In the script, add above the default:

```sh
# NOTE: while MLN_FFI_REV points at a PR branch (maplibre-native-ffi#631)
# there is NO published artifact for it — this script will fail the rev
# check by design. Build in Docker (mln-ffi-build stage) or copy
# /ffi/install out of that stage for host development.
```

- [ ] **Step 3: Verify the FFI stage builds**

Run: `docker build --target mln-ffi-build .` (expect ~20–60 min cold). Expected: exits 0; the stage log shows `git checkout 422f853…` and the install-tree `test -f` assertions pass.

- [ ] **Step 4: Verify the C API surface the plan depends on**

Run inside the checkout used by the build (or the scratchpad clone of the PR branch): `grep -c "mln_map_request_still_image_start\|mln_render_session_service_driver_work\|mln_texture_read_premultiplied_rgba8_start" include/maplibre_native_c.h` (via `include/maplibre_native_c/*.h`). Expected: all three present.

- [ ] **Step 5: Commit**

```bash
git add Dockerfile Makefile scripts/fetch-mln-ffi.sh
git commit -m "build: compile the FFI from source at the PR 631 native-executor head

No published artifact exists for the PR branch; resurrect the pre-e5c7a00
mln-ffi-build compile stage pinned to 422f853f6cf8. Revert this commit to
return to artifact consumption once the PR merges and the snapshot tag
passes it."
```

Note: after this commit the Go build stage still fails (go.mod pins the old binding, which no longer matches the new .so's ABI check only when bumped — the mismatch guard in `rampardos-build` fails fast either way). That is expected until Task 2 lands; do not "fix" it here.

---

### Task 2: Bump the Go binding and convert the worker internals

**Files:**
- Modify: `rampardos/go.mod` / `rampardos/go.sum` (via `go get`)
- Modify: `rampardos/internal/services/renderer/gopool_linux.go` (worker init/loop/render/settle/cleanup; pool code above the worker section is untouched except the reload arm's call site)
- Test: existing `rampardos/internal/services/renderer/` suite via Docker (elastic/broadcast tests stub `startWorker`, so they exercise pool policy unchanged; the real worker path is covered by `integration_test.go` under `-tags renderer_integration` and by the soak in Task 4)

**Interfaces:**
- Consumes: the API mapping table above; Task 1's FFI install tree.
- Produces: `goWorker` with fields `rt *maplibre.RuntimeHandle`, `m *maplibre.MapHandle`, `sess *maplibre.RenderSessionHandle`, `egl *eglContext`, `wake chan struct{}`, `frameToken uint64`, `curW, curH uint32`, `curScale float64`, `poisoned bool` (drops `buf`, `stillPending`); helper `func (w *goWorker) service(ctx context.Context, op opPoller, budget time.Duration, frames *frameTracker, what string) error` and type `opPoller interface{ Poll() (bool, error) }`. Pool-side code (`dispatch`, `grow`, `reap`, `setStyleAll`, channel plumbing) must not change.

- [ ] **Step 1: Bump the module**

```bash
cd rampardos && go get github.com/maplibre/maplibre-native-ffi/bindings/go@422f853f6cf8adaa4d0a878597fbd48c15dd1c08 && go mod tidy
```

Expected: go.mod shows a pseudo-version ending `-422f853f6cf8`. (`go mod tidy` works on macOS — it doesn't build cgo.)

- [ ] **Step 2: Verify the build now fails for the expected reason**

Run: `docker build --target rampardos-build .` Expected: FAIL in `go build` with undefined/changed-signature errors in `gopool_linux.go` (e.g. `w.rt.Pump undefined`, `assignment mismatch: … m.SetStyleURL`). This is the failing state the rest of the task fixes. (The binding/ABI consistency check passes now — both sides say `422f853f6cf8`.)

- [ ] **Step 3: Replace the worker struct fields**

In `gopool_linux.go`, replace the `goWorker` field block `rt/m/sess/egl/buf/curW/curH/curScale/stillPending/poisoned` with:

```go
	rt   *maplibre.RuntimeHandle
	m    *maplibre.MapHandle
	sess *maplibre.RenderSessionHandle
	egl  *eglContext

	// wake is fed by the runtime's notification callback (which may fire
	// on any native thread and coalesces); buffered-1 + non-blocking send
	// turns bursts into one pending token. The worker parks on it instead
	// of the old rt.Pump.
	wake chan struct{}

	curW     uint32
	curH     uint32
	curScale float64

	// frameToken numbers frame demands so drained results can be matched
	// to the still image currently being driven.
	frameToken uint64

	// poisoned is set when an abandoned still-image operation could not be
	// settled even after Cancel. The pool retires the worker rather than
	// dispatching to a map that may never start another render.
	poisoned bool
```

- [ ] **Step 4: Rewrite `init()`**

```go
func (w *goWorker) init() error {
	var err error
	w.egl, err = newEGLContext()
	if err != nil {
		return err
	}

	w.rt, err = maplibre.NewRuntime()
	if err != nil {
		return fmt.Errorf("renderer: new runtime: %w", err)
	}

	w.wake = make(chan struct{}, 1)
	if err := w.rt.SetNotificationCallback(func() {
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}); err != nil {
		return fmt.Errorf("renderer: set notification callback: %w", err)
	}

	// MapModeStatic is the render-once mode; the executor disables the
	// animation tick and aligns with our request/reply pattern. The event
	// mask is trimmed to what the service loop consumes: render-update
	// (drives frame demands) and the two failure events. Still completion
	// arrives via the operation handle, not an event.
	w.curW, w.curH, w.curScale = 256, 256, float64(w.pool.cfg.ratio)
	w.m, err = w.rt.NewMapWithOptions(maplibre.MapOptions{
		Width:       w.curW,
		Height:      w.curH,
		ScaleFactor: w.curScale,
		Mode:        maplibre.MapModeStatic,
		EventMask: maplibre.RuntimeEventMaskMapRenderUpdateAvailable |
			maplibre.RuntimeEventMaskMapLoadingFailed |
			maplibre.RuntimeEventMaskMapRenderError,
	})
	if err != nil {
		return fmt.Errorf("renderer: new map: %w", err)
	}

	// Caller-graphics-thread driver: EGL targets reject the core-worker
	// driver, so this worker's pinned thread services all graphics work.
	// Shared context ownership keeps our 3.3-Compatibility context as the
	// share-group anchor, exactly as before the executor rewrite.
	sess, attach, err := w.m.AttachOpenGLOwnedTexture(
		maplibre.OpenGLOwnedTextureDescriptor{
			Extent: maplibre.RenderTargetExtent{
				Width:       w.curW,
				Height:      w.curH,
				ScaleFactor: w.curScale,
			},
			Context: w.egl.descriptor(),
		},
		maplibre.RenderSessionAttachOptions{
			Driver:                    maplibre.RenderDriverCallerGraphicsThread,
			RequestedTextureRingDepth: 1,
		},
	)
	if err != nil {
		return fmt.Errorf("renderer: attach OpenGL owned texture render target: %w", err)
	}
	w.sess = sess
	err = w.service(context.Background(), attach, w.pool.cfg.startupTimeout, nil, "attach")
	attach.Release()
	if err != nil {
		return err
	}

	caps, err := w.sess.Capabilities()
	if err != nil {
		return fmt.Errorf("renderer: session capabilities: %w", err)
	}
	if caps.Flags&maplibre.RenderSessionCapabilityReadback == 0 {
		return fmt.Errorf("renderer: attached session does not grant readback; cannot render stills")
	}

	// Style loading proceeds on the native executor; the first render's
	// service loop observes any failure via MapLoadingFailed.
	if _, err := w.m.SetStyleURL(w.pool.cfg.styleURL); err != nil {
		return fmt.Errorf("renderer: set style url: %w", err)
	}
	return nil
}
```

- [ ] **Step 5: Write the service loop (replaces `pumpUntilStillFinished`)**

Delete `pumpUntilStillFinished` and add:

```go
// opPoller is the completion probe of any OperationHandle[T]; the loop
// needs nothing else from the handle, and the interface erases T.
type opPoller interface{ Poll() (bool, error) }

// frameTracker carries per-still frame-demand state through the service
// loop. nil means the awaited operation needs no frames (attach, resize,
// readback, detach) — it completes on driver servicing alone.
type frameTracker struct {
	rendered   bool
	needDemand bool
}

// service drives the caller-thread driver until op completes, the budget
// expires, or ctx is cancelled. It is the executor-era replacement for
// the Pump/PollEvent loop: native workers make autonomous progress (style
// load, tile IO, layout) and signal the notification callback; this
// thread only executes queued graphics work and, when frames is non-nil,
// keeps a frame demand outstanding so the still image can complete.
//
// Parks are capped at 100ms as insurance against a missed notification
// edge: a lost wake degrades to a slow poll, not a stall. The old pump's
// per-render breakdown metrics keep flowing with the analogous inputs.
func (w *goWorker) service(ctx context.Context, op opPoller, budget time.Duration, frames *frameTracker, what string) error {
	deadline := time.Now().Add(budget)
	start := time.Now()
	iterations := 0
	servicedCnt := 0
	var totalPark, totalService time.Duration
	var timeToFirst time.Duration
	sawFirst := false

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("renderer: %s timed out after %s", what, budget)
		}
		iterations++

		svcStart := time.Now()
		serviced, err := w.sess.ServiceDriverWork(int(^uint(0) >> 1))
		totalService += time.Since(svcStart)
		if err != nil {
			return fmt.Errorf("renderer: %s: service driver work: %w", what, err)
		}
		servicedCnt += serviced

		batch, err := w.rt.DrainEvents(0)
		if err != nil {
			return fmt.Errorf("renderer: %s: drain events: %w", what, err)
		}
		for _, ev := range batch.Events {
			if !sawFirst {
				timeToFirst = time.Since(start)
				sawFirst = true
			}
			switch ev.Type {
			case maplibre.RuntimeEventMapRenderUpdateAvailable:
				if frames != nil {
					frames.needDemand = true
				}
			case maplibre.RuntimeEventMapLoadingFailed:
				return fmt.Errorf("renderer: map loading failed: %s", ev.Message)
			case maplibre.RuntimeEventMapRenderError:
				return fmt.Errorf("renderer: map render error: %s", ev.Message)
			}
		}

		if frames != nil {
			fb, err := w.sess.DrainFrameResults(0)
			if err != nil {
				return fmt.Errorf("renderer: %s: drain frame results: %w", what, err)
			}
			results, err := fb.Results()
			fb.Close()
			if err != nil {
				return fmt.Errorf("renderer: %s: frame results: %w", what, err)
			}
			for _, res := range results {
				if !sawFirst {
					timeToFirst = time.Since(start)
					sawFirst = true
				}
				if res.Disposition == maplibre.RenderResultRendered {
					frames.rendered = true
				} else {
					// No frame was presented for that demand; keep one
					// outstanding so tile arrivals can complete the still.
					frames.needDemand = true
				}
			}
		}

		done, err := op.Poll()
		if err != nil {
			return fmt.Errorf("renderer: %s: poll: %w", what, err)
		}
		if done {
			if services.GlobalMetrics != nil {
				services.GlobalMetrics.RecordRendererPumpBreakdown(
					w.pool.cfg.styleID, w.pool.cfg.scaleLabel,
					iterations,
					totalPark.Seconds(),
					timeToFirst.Seconds(),
					totalService.Seconds(),
					servicedCnt,
				)
			}
			return nil
		}

		if frames != nil && frames.needDemand {
			frames.needDemand = false
			w.frameToken++
			demand := maplibre.NewFrameDemand()
			demand.Token = w.frameToken
			if err := w.sess.RequestFrame(demand); err != nil {
				return fmt.Errorf("renderer: %s: request frame: %w", what, err)
			}
			continue
		}
		if serviced > 0 {
			continue
		}

		// Idle: nothing serviced and the operation is incomplete. Consume
		// and re-arm the edge-triggered ready state, then park until the
		// callback wakes us (or the defensive cap / deadline passes).
		if _, err := w.rt.DrainReady(); err != nil {
			return fmt.Errorf("renderer: %s: drain ready: %w", what, err)
		}
		park := min(time.Until(deadline), 100*time.Millisecond)
		if park <= 0 {
			continue
		}
		parkStart := time.Now()
		timer := time.NewTimer(park)
		select {
		case <-w.wake:
			timer.Stop()
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			totalPark += time.Since(parkStart)
			return ctx.Err()
		}
		totalPark += time.Since(parkStart)
	}
}
```

Note on metric inputs: `RecordRendererPumpBreakdown(style, scale, iterations, sleepSeconds, firstEventSeconds, renderSeconds, renderCount)` keeps its signature; only the meaning shifts (park time, ServiceDriverWork time, work-items serviced). Check `internal/services/metrics.go` for the exact parameter order before wiring — the call above mirrors the old call site's order.

- [ ] **Step 6: Rewrite `renderOne()`**

```go
// renderOne is the per-worker hot path. Resize only if extent changed
// (saves the texture-realloc when consecutive requests share size).
func (w *goWorker) renderOne(ctx context.Context, vp ViewportRequest, scale int) (*image.NRGBA, error) {
	if vp.Width <= 0 || vp.Height <= 0 {
		return nil, fmt.Errorf("renderer: invalid dimensions %dx%d", vp.Width, vp.Height)
	}

	wantW := uint32(vp.Width)
	wantH := uint32(vp.Height)
	wantScale := float64(scale)
	if wantW != w.curW || wantH != w.curH || wantScale != w.curScale {
		op, err := w.sess.ResizeStart(maplibre.RenderTargetExtent{
			Width:       wantW,
			Height:      wantH,
			ScaleFactor: wantScale,
		})
		if err != nil {
			return nil, fmt.Errorf("renderer: resize: %w", err)
		}
		err = w.service(ctx, op, w.pool.cfg.renderTimeout, nil, "resize")
		op.Release()
		if err != nil {
			return nil, err
		}
		w.curW, w.curH, w.curScale = wantW, wantH, wantScale
	}

	if _, err := w.m.JumpTo(maplibre.CameraOptions{}.
		WithCenter(maplibre.LatLng{Latitude: vp.Latitude, Longitude: vp.Longitude}).
		WithZoom(vp.Zoom).
		WithBearing(vp.Bearing).
		WithPitch(vp.Pitch)); err != nil {
		return nil, fmt.Errorf("renderer: camera: %w", err)
	}

	still, err := w.m.RequestStillImage()
	if err != nil {
		return nil, fmt.Errorf("renderer: request still image: %w", err)
	}
	frames := &frameTracker{needDemand: true}
	if err := w.service(ctx, still, w.pool.cfg.renderTimeout, frames, "still image"); err != nil {
		w.settleAbandonedStill(still)
		still.Release()
		return nil, err
	}
	if st, serr := still.Status(); serr != nil || st != 0 {
		diag, _ := still.Diagnostic()
		still.Release()
		return nil, fmt.Errorf("renderer: still image failed (status %d): %s", st, diag)
	}
	still.Release()
	if !frames.rendered {
		return nil, fmt.Errorf("renderer: still image finished without a render frame")
	}

	physW := vp.Width * scale
	physH := vp.Height * scale

	readStart := time.Now()
	rb, err := w.sess.ReadPremultipliedRGBA8Start()
	if err != nil {
		return nil, fmt.Errorf("renderer: read pixels: %w", err)
	}
	if err := w.service(ctx, rb, w.pool.cfg.renderTimeout, nil, "readback"); err != nil {
		rb.Release()
		return nil, err
	}
	tr, err := rb.Take()
	rb.Release()
	readDur := time.Since(readStart)
	if err != nil {
		return nil, fmt.Errorf("renderer: read pixels: %w", err)
	}
	if int(tr.Info.Width) != physW || int(tr.Info.Height) != physH {
		return nil, fmt.Errorf("renderer: size mismatch: got %dx%d, want %dx%d", tr.Info.Width, tr.Info.Height, physW, physH)
	}
	if services.GlobalMetrics != nil {
		services.GlobalMetrics.RecordRendererReadback(w.pool.cfg.styleID, w.pool.cfg.scaleLabel, readDur.Seconds())
	}

	out := image.NewNRGBA(image.Rect(0, 0, physW, physH))
	unpremultiplyRGBA(out.Pix, tr.Data)
	return out, nil
}
```

`unpremultiplyRGBA` is unchanged. If `len(tr.Data) != physW*physH*4` is possible per `Info.Stride`, guard: when `tr.Info.Stride != uint32(physW*4)`, copy row-by-row instead (`src := tr.Data[y*int(tr.Info.Stride):]`); add this guard only if the integration test shows padded strides — note the decision in the commit message either way.

- [ ] **Step 7: Rewrite the settle path**

Delete `settleAbandonedStill` (event-based) and add:

```go
// settleAbandonedStill cancels an outstanding still operation and drives
// the driver until it reaches a terminal state, discarding whatever it
// produced. The executor completes a cancelled still without needing
// further frames, so this normally returns in single-digit milliseconds;
// a worker whose still cannot settle within the budget is poisoned and
// retired by the pool, preserving the old invariant that we never hand
// out a map that may be unable to start another render.
//
// Deliberately not on the caller's context — that context is typically
// already cancelled, which is why we are here.
func (w *goWorker) settleAbandonedStill(still *maplibre.OperationHandle[struct{}]) {
	_ = still.Cancel()
	frames := &frameTracker{needDemand: true}
	if err := w.service(context.Background(), still, w.pool.cfg.settleTimeout, frames, "settle"); err != nil {
		w.poisoned = true
		slog.Warn("renderer worker retired: could not settle abandoned still-image request",
			"style", w.pool.cfg.styleID, "scale", w.pool.cfg.scaleLabel, "timeout", w.pool.cfg.settleTimeout, "error", err)
	}
}
```

The `if w.stillPending { w.settleAbandonedStill() }` guard in the old `renderOne` is gone — the call site in Step 6 always settles on service failure because `Cancel` on an already-terminal operation is harmless, which removes the `stillPending` bookkeeping entirely.

- [ ] **Step 8: Update the reload arm and `cleanup()`**

In `loop()`'s `goWorkerCmdReload` case (SetStyleURL now returns two values):

```go
		case goWorkerCmdReload:
			// SetStyleURL is a command — the executor loads the style in
			// the background and the next render's service loop observes
			// completion or failure. Reply immediately so reload doesn't
			// block renders.
			_, err := w.m.SetStyleURL(cmd.reload)
			cmd.reply <- goWorkerResult{err: err}
```

Rewrite `cleanup()` — detach is an operation that needs this thread's servicing; abandon is the fallback that requires no graphics work:

```go
func (w *goWorker) cleanup() {
	if w.sess != nil {
		detached := false
		if op, err := w.sess.DetachStart(); err == nil {
			if err := w.service(context.Background(), op, w.pool.cfg.settleTimeout, nil, "detach"); err == nil {
				detached = true
			}
			op.Release()
		}
		if !detached {
			_, _ = w.sess.Abandon()
		}
		_ = w.sess.Close()
	}
	if w.m != nil {
		_ = w.m.Close()
	}
	if w.rt != nil {
		_ = w.rt.ClearNotificationCallback()
		_ = w.rt.Close()
	}
	if w.egl != nil {
		w.egl.close()
	}
}
```

Also update the package/file header comment: the old text claims "the binding requires every native call for a Runtime to originate on the thread that created it" — replace with: workers stay OS-thread-pinned because EGL render sessions require the caller-graphics-thread driver; runtime and map handles themselves are goroutine-safe in the executor-era binding.

- [ ] **Step 9: Compile and vet in Docker**

Run: `docker build --target rampardos-test .`
Expected: `go vet ./...` and `go test -count=1 -race ./...` both pass (elastic/broadcast pool tests run against the stubbed worker constructor; nothing above the worker section changed, so they must pass unmodified — if they need edits, that's a signal the pool interface drifted: stop and re-check Step 3–8 against the "Produces" contract).

- [ ] **Step 10: Run the renderer integration test**

The integration test needs the FFI at runtime. Run it inside the test stage:

```bash
docker build --target rampardos-test -t rampardos-test-img .
docker run --rm rampardos-test-img sh -c 'cd /src && PKG_CONFIG_PATH=/ffi/install/share/pkgconfig CGO_ENABLED=1 go test -tags renderer_integration ./internal/services/renderer/ -v -timeout 120s'
```

(Adjust the in-image source path if `WORKDIR` differs — check with `docker run --rm rampardos-test-img pwd`.) Expected: PASS, including an actual still render through the new service loop. If the test env lacks a software GL stack, check what `integration_test.go` skips on and report rather than forcing.

- [ ] **Step 11: Commit**

```bash
git add rampardos/go.mod rampardos/go.sum rampardos/internal/services/renderer/gopool_linux.go
git commit -m "feat(renderer): convert workers to the FFI native-executor model

Pump/PollEvent/RenderUpdate becomes a notification-parked service loop
driving OperationHandles: still image, resize, readback, attach and
detach are operations; frames are demand-driven. Cancellation replaces
the event-settle dance; the poison backstop stays. Topology, config,
and metric names unchanged."
```

---

### Task 3: Sweep the comments, docs, and dashboard copy that describe the pump

**Files:**
- Modify: `rampardos/internal/services/metrics.go` (help strings that say "pump"/"Pump" describe park/service semantics now — reword the `RecordRendererPumpBreakdown` metric help texts only; keep metric and label names)
- Modify: `rampardos/cmd/server/main.go:117` (comment references the old build-tag/pump era if stale — verify and correct)
- Modify: `CLAUDE.md` (renderer section: no rule changes, but if any bullet describes Pump-era behavior, update it; the scale=1/scale=2 regression-hotspot rule stays verbatim)
- Modify: `grafana-dashboard.json` only if a panel description names "Pump" (do not touch queries)

**Interfaces:**
- Consumes: Task 2's loop (park/service/first-signal breakdown semantics).
- Produces: nothing programmatic — documentation truth.

- [ ] **Step 1: Find every stale reference**

Run: `grep -rn -i "pump\|PollEvent\|RenderUpdate\b" rampardos/ CLAUDE.md grafana-dashboard.json --include="*.go" --include="*.md" --include="*.json" | grep -v _test | grep -v superpowers`

- [ ] **Step 2: Reword each hit**

For metrics help strings: "Time parked awaiting runtime notifications" replaces "sleep backoff inside Pump"; "time in ServiceDriverWork (draws + readbacks)" replaces "RenderUpdate cgo cost". Keep every metric name, label, and bucket unchanged. For comments: describe the notification-parked service loop. Do not touch behavior.

- [ ] **Step 3: Verify and commit**

Run: `docker build --target rampardos-test .` Expected: PASS (comment/string-only diff).

```bash
git add -A rampardos CLAUDE.md grafana-dashboard.json
git commit -m "docs: describe the native-executor service loop where comments said Pump"
```

---

### Task 4: Local soak against real styles (verification gate, no code)

**Files:** none created in-repo (scratch output only).

**Interfaces:**
- Consumes: the full image from Tasks 1–3.

- [ ] **Step 1: Build the runtime image**

Run: `docker build -t rampardos:pr631 .` Expected: all stages pass including `rampardos-test`.

- [ ] **Step 2: Boot and render both scales**

Start via `docker-compose.yml` (or `docker run` with the styles volume as compose mounts it). Then exercise the CLAUDE.md regression hotspot — scale=1 AND scale=2, integer and fractional zoom:

```bash
curl -fsS "http://localhost:PORT/staticmap?..." # style=<local>, zoom=integer, scale=1
curl -fsS "..."                                 # same viewport, scale=2
curl -fsS "..."                                 # fractional zoom, scale=1
curl -fsS "..."                                 # fractional zoom, scale=2
```

(Take the exact request shape from README/`setup` examples or `internal/handlers/static_map.go` route registration.) Expected: 200s; scale=2 output has exactly 2× pixel dimensions of scale=1; fractional-zoom output visually plausible (spot-check the PNGs).

- [ ] **Step 3: Exercise reload, cancellation, and pool elasticity**

- Reload: hit the styles-reload endpoint (find it in route registration) mid-traffic; renders after it must still succeed.
- Cancellation: issue a burst of requests and kill the client early (`curl -m 0.05`); follow with normal requests. Expected: no failures on the followers, no `renderer worker retired` warnings in logs (a rare one is the backstop working; a steady stream is a bug in the settle path).
- Elasticity: burst > `STYLE_POOL_SIZE` concurrent requests, then idle past `RENDERER_POOL_IDLE_TTL`; watch the pool-size gauge grow then decay.

- [ ] **Step 4: Compare the render-time distribution**

Pull `/metrics` and compare viewport-render p50 against the pre-conversion baseline (~24 ms scale=1 on prod hardware; local numbers differ but the before/after ratio on the same machine is the signal — build `master`'s image for the baseline if needed). Expected: within ~20% of baseline p50. If the park cap dominates (`pump_sleep` ≈ N×100 ms), notifications aren't waking the loop — stop and debug `DrainReady`/callback wiring before merging anything.

- [ ] **Step 5: Report**

No commit. Summarize: pass/fail per probe, p50/p99 before vs after, any worker-retired warnings. This gate decides whether the branch is proposable.

---

## Self-review notes

- **Spec coverage:** D1/D2 (driver + ownership) land in Task 2 Step 4; D3 (topology unchanged) is enforced by Task 2's "Produces" contract; D4/D5 (service loop, event mask) Task 2 Steps 4–5; D6 (cancel + poison) Step 7; D7 (readback copy) Step 6; D8 (metrics) Steps 5 and Task 3; D9 (build pin) Task 1. Risks map to Task 4's probes.
- **Known deliberate gaps:** dedicated context ownership and single-runtime consolidation are explicitly out of scope (spec "Deferred follow-up"); reverting to artifact consumption happens when PR 631 merges, as the inverse of Task 1's isolated commit.
- **Type consistency:** `service(ctx, op, budget, frames, what)` is defined in Task 2 Step 5 and consumed with identical signatures in Steps 4, 6, 7, 8. `frameTracker` fields `rendered`/`needDemand` used only in Steps 5–7. `opPoller` erases the operation's type parameter for all five call sites.
