# Migrate rampardos-go-renderer to upstream Go binding + OpenGL backend

You are picking this up cold. Read the whole brief before doing anything — the dependency order between the Dockerfile changes, the Go code changes, and the C ABI variant build matters.

## Mission

The unofficial Go binding `github.com/jfberry/maplibre-native-go` we've been using has been deprecated in favour of the official upstream binding at `github.com/maplibre/maplibre-native-ffi/bindings/go` (branch `golang`). We've extended the upstream binding with Linux OpenGL EGL render-target support on a branch we plan to PR upstream. This rampardos-go-renderer repo currently consumes the unofficial binding and must migrate to upstream so we can A/B test OpenGL vs Vulkan in production.

Two work tracks must land together — neither is useful on its own:

- **Track A (Docker / build infra):** clone our fork at the right commit, parameterize the build by GPU backend, bundle the right `libmaplibre-native-c.so` variant + Mesa runtime drivers.
- **Track B (Go renderer code):** swap import path, rewrite `gopool.go` against upstream's lower-level API, add EGL context creation, add `runtime.LockOSThread`-pinned worker goroutines.

The output is two deployable container images per build (`*:opengl-<sha>`, `*:vulkan-<sha>`) — both functionally equivalent, differing only in which GPU API the in-process renderer drives.

## Reference state — pin to these

- **Our fork:** https://github.com/jfberry/maplibre-native-ffi
- **Branch:** `go-opengl-linux`
- **Commit SHA to pin:** `9b349e12beeb732575b1227f09e0f866b67b13f0`
  - Contains:
    - upstream `golang` branch merged with `origin/main` (WGL/EGL C ABI support)
    - `fa264fd` — Go binding for OpenGL Linux render targets
    - `2587cf2` — `go-readback` example
    - `8ea3345` — texture session: drop `glFinish()` from `swap()`, switch `ContextMode` to `Unique` (matches mbgl `HeadlessBackend`). Single biggest perf change.
    - `856d5ae` — `AttachOpenGLOffscreen` + `OpenGLOffscreenDescriptor` (renderbuffer-color FBO, mbgl `HeadlessBackend` layout). Optional alternative to owned-texture.
    - `19807fc` — `BypassResourceLoader` (opt-in via `MLN_FFI_RESOURCE_LOADER=bypass`). Synchronous MainResourceLoader replacement. Spike result: doesn't move the needle on measured workloads, kept as documented option.
    - `dfd7091` — `mln_runtime_run_blocking` + `RuntimeHandle.RunBlocking(timeout)`. Bounded libuv-blocking pump primitive. Requires submodule bump — `.gitmodules` now points at `jfberry/maplibre-native` for a small RunLoop patch.
    - `7206a1a` — **`mln_runtime_wait_for_event` + `RuntimeHandle.WaitForEvent(timeout)`**. Filters spurious libuv-internal wakes *inside C* — only returns when the runtime event queue is non-empty or the timeout expires. This is the primitive to use when the next caller step is `PollEvent`. Each spurious wake costs one mutex acquire instead of a cgo crossing.
    - `9b349e1` — **render-update event coalescing in the runtime queue.** One `MAP_RENDER_UPDATE_AVAILABLE` per map per pump cycle instead of one per `onInvalidate`, matching mbgl's `HeadlessFrontend` (which coalesces implicitly via `uv_async_send`). Caller contract unchanged — still "render the latest" per event. Validated against EGL: byte-identical output, cold-frame events drop (12→5 at z3, 14→5 at z5), warm frames stay at 1, `StillImageFinished` always fires (no deadlock). Under our `WaitForEvent` pump the per-drain burst accumulates larger, so `pump_render_update_count` should fall toward ~1 — verify in prod metrics.

The Go binding lives at `bindings/go/` inside that repo. Working examples to copy patterns from:

- **`bindings/go/render.go`** in the upstream checkout — read the OpenGL types (`OpenGLContextDescriptor`, `EglContextDescriptor`, `NewOpenGLContextEGL`, `AttachOpenGLOwnedTexture`, `AcquireOpenGLTextureFrame`) and Vulkan types (`VulkanContextDescriptor`, `AttachVulkanOwnedTexture`, etc.).
- **`examples/go-readback/main.go`** — full lifecycle reference: EGL context setup, attach owned-texture session, request still image, drive RunOnce/PollEvent pump, readback to PPM. Most of `egl_linux.go` for this repo should be a direct copy of the cgo preamble + `eglContext` type there.
- **`bindings/go/maplibre_abi_test.go`** — shows how to query `SupportedRenderBackends()` + `SupportedOpenGLContextProviders()` at runtime.

Cross-reference (validated locally — Mesa software stack in Apple Silicon Docker linux/arm64). Two workload modes; **only the cache-killer numbers are representative of a real tile-server load.** The linear-walk numbers are kept for context but should NOT be used as the target for prod sizing.

### Workload: linear walk (NOT representative — high cache reuse)

Adjacent ~1 km steps SE from Dumfries at z15. After warmup, mbgl's in-memory tile pyramid covers most of the viewport from the previous frame, so per-frame I/O is minimal. FPS looks great but the resource pipeline isn't exercised.

| Metric | OpenGL (llvmpipe) owned-texture |
|---|---|
| Median FPS (3 replicates) | 810 |
| p50 | 1.10 ms |
| p99 | 2.95 ms |
| tile/frame requested | 5.0 (3.3 from cache / 1.6 from net) |

### Workload: cache-killer (random viewports in GB bbox) — **use this for tuning**

Each frame teleports the camera to a uniformly random point in the GB mainland bounding box (`lat 50.0–58.5`, `lon -6.0–1.5`) with a seeded PRNG. This mimics tile-server traffic where successive requests have no temporal coherence. Available in the bench as `--walk-mode random-gb`.

| Metric | OpenGL (llvmpipe) owned-texture, actor loader |
|---|---|
| Median FPS (3 replicates) | 387 |
| p50 | 1.86 ms |
| p90 | ~4.0 ms |
| p99 | 13.47 ms |
| frame max | ~32 ms |
| tile/frame requested | 7.4 (5.1 cache / 2.3 net) |
| tile/frame loaded | 5.2 (2.9 cache / 2.3 net) |
| tile/frame parsed | 10.9 started / 8.3 finished (24% mid-flight cancels) |

Notes:
- Software renderers (Mesa llvmpipe/lavapipe). Real hardware GPU paths will be much faster.
- **Use the cache-killer numbers** when sizing prod capacity. The linear-walk's ~800 FPS is what mbgl gives you when nothing is loading; under real traffic you'll see ~400 FPS / p99 ~13 ms on this software stack.
- **The single largest perf knob found so far is the pump cadence**: `time.Sleep(1*time.Millisecond)` between `RunOnce` calls gave 191.7 FPS — 2.8× worse than the hybrid `RunOnce` + `RunBlocking(10ms)` pump in §B3. `runtime.Gosched()` works too but burns 100% of one core per worker; `RunBlocking` parks in `epoll_wait`.

### What we tried that didn't help

| Change | Verdict |
|---|---|
| `AttachOpenGLOffscreen` (renderbuffer color FBO) vs owned-texture | Within 1–2% noise on both workloads. Optional. |
| `MLN_FFI_RESOURCE_LOADER=bypass` (synchronous MainResourceLoader) | Within noise even under cache-killer. Code is kept opt-in but default stays on `actor`. |
| Mesa env vars (`vblank_mode=0`, `mesa_glthread=true`, `MESA_SHADER_CACHE_DIR`, …) | Variance dominates. A few hurt p99 (notably `mesa_glthread=true`). See "Mesa tuning" below — recommended set is now **none**. |
| `MESA_VK_VERSION_OVERRIDE=1.3` | Catastrophic frame-max spikes (91 ms). Don't set. `1.2` is benign-or-slight-win. |

### Where the remaining cost lives

Per-tile telemetry under cache-killer (the `tile/frame …` lines the bench now prints) shows the dominant cost is in `mbgl::MBTilesFileSource` itself, not the loader above it:

1. **Single worker thread per `MBTilesFileSource`** — all ~2.3 net fetches per frame serialise through one thread.
2. **SQLite per-request statement parse** — `mbtiles_file_source.cpp` builds the SQL string by integer concatenation and constructs a new `mapbox::sqlite::Statement` per tile, throwing away the prepared bytecode every time. Should be cacheable per database.
3. **Tile decompression** — `util::decompress(*response.data)` runs on the same worker thread; gzipped GB OSM tiles are 1–3 ms each to decompress.

Optimisation candidates we have NOT tried, cheapest first:

- **Pre-decompressed mbtiles.** Re-export the GB data without per-tile gzip compression. Zero code change in the binding; modest disk-size cost. Should remove the 1–3 ms decompression cost per net fetch.
- **MBTilesFileSource statement cache.** Small mbgl patch (or vendor a replacement `FileSourceType::Mbtiles` factory in the FFI). Steady-state win is probably <1% but it's free perf.
- **Per-source thread pool.** Replace `MBTilesFileSource`'s single worker with a small pool so the ~2.3 net fetches/frame go in parallel. Bigger change; biggest potential upside.

### Mesa tuning — what we tried, in short

A sweep through `vblank_mode=0`, `mesa_glthread=true`, `MESA_SHADER_CACHE_DIR`, `LP_NUM_THREADS=N`, `MESA_NO_ERROR=1`, `MESA_VK_VERSION_OVERRIDE` produced no robust wins on either workload (3+ replicates each). Initial single-shot results suggested ~+24% from `vblank_mode=0 mesa_glthread=true` but those evaporated on replication and `mesa_glthread=true` introduced 35–55 ms tail-latency spikes (the GL marshalling thread fights the Go scheduler).

**Recommended Mesa env: none.** If you want to A/B in your prod env anyway, do it with N ≥ 5 replicates per config and measure p99 + frame max separately from median FPS. The one solid don't: never set `MESA_VK_VERSION_OVERRIDE=1.3` — frame max spikes 6× in our test.

## Track A: Dockerfile changes

The existing `mln-ffi-build` stage in this repo's `Dockerfile` clones upstream at a pinned revision and runs `mise run build`. Upstream's build layout changed: `libmaplibre-native-c.so` now lives in a per-variant subdirectory (`build/linux-<arch>-<variant>/`). All downstream stages that reference `build/libmaplibre-native-c.so` must be updated.

### A1. Source pin

```dockerfile
# was: ARG MLN_FFI_REV=b43836502281b9d091d7d78b7ad3219a9c805c7e
ARG MLN_FFI_REPO=https://github.com/jfberry/maplibre-native-ffi
ARG MLN_FFI_REV=2587cf28854ae0636f6d8512572c0f387b58e81a

# was: RUN git clone https://github.com/sargunv/maplibre-native-ffi . && git checkout ${MLN_FFI_REV}
RUN git clone "${MLN_FFI_REPO}" . && git checkout ${MLN_FFI_REV}
```

### A2. Backend-parameterised variant build

Add a build-arg for the backend and resolve `MISE_ENV` from it + `TARGETARCH`:

```dockerfile
ARG GPU_BACKEND=opengl
# Upstream's variant naming: linux-{arm64|x64}-{egl|vulkan}.
# TARGETARCH from buildx is arm64|amd64 — remap amd64→x64.
# GPU_BACKEND opengl maps to upstream's "egl" variant suffix on Linux.
ENV GPU_BACKEND=${GPU_BACKEND}
RUN case "$TARGETARCH" in \
        arm64) MLN_ARCH=arm64;; \
        amd64) MLN_ARCH=x64;; \
        *) echo "unsupported TARGETARCH=$TARGETARCH" >&2; exit 1;; \
    esac \
 && case "$GPU_BACKEND" in \
        opengl) MLN_VARIANT_SFX=egl;; \
        vulkan) MLN_VARIANT_SFX=vulkan;; \
        *) echo "unsupported GPU_BACKEND=$GPU_BACKEND" >&2; exit 1;; \
    esac \
 && MLN_VARIANT="linux-${MLN_ARCH}-${MLN_VARIANT_SFX}" \
 && echo "$MLN_VARIANT" > /tmp/mln_variant
ENV MISE_ENV_FILE=/tmp/mln_variant
# Pass-through to subsequent steps. mise reads MISE_ENV from the env directly.
RUN export MISE_ENV=$(cat /tmp/mln_variant) \
 && eval "$(mise activate bash)" \
 && mise run configure \
 && mise run build
```

Memory note: the C++ unity files (notably `harfbuzz.cc`) can need up to 6 GB per cc1plus instance. If your GitHub Actions runner is memory-constrained, set `ENV CMAKE_BUILD_PARALLEL_LEVEL=3` before the `mise run build` invocation; the build OOMs at 8 GB total with default parallelism. GitHub-hosted Ubuntu runners have 16 GB, so this should be fine without throttling.

### A3. Variant-aware artifact paths

Everywhere the current Dockerfile references `build/libmaplibre-native-c.so` or `build/pkgconfig/maplibre-native-c.pc`, prefix with the variant subdir:

```dockerfile
# was: RUN test -f build/libmaplibre-native-c.so && test -f build/pkgconfig/maplibre-native-c.pc
RUN MLN_VARIANT=$(cat /tmp/mln_variant) \
 && test -f build/${MLN_VARIANT}/libmaplibre-native-c.so \
 && test -f build/${MLN_VARIANT}/pkgconfig/maplibre-native-c.pc
```

The existing ldd-bundling + patchelf step needs the same prefix:

```dockerfile
RUN MLN_VARIANT=$(cat /tmp/mln_variant) \
 && BUILD=build/${MLN_VARIANT} \
 && for lib in $(LD_LIBRARY_PATH=/ffi/.pixi/envs/default/lib ldd ${BUILD}/libmaplibre-native-c.so | awk '/=> .*pixi/ {print $3}'); do \
        cp -L "$lib" ${BUILD}/; \
    done \
 && for lib in ${BUILD}/*.so*; do \
        patchelf --set-rpath '$ORIGIN' "$lib"; \
    done
```

The runtime stage's `COPY --from=mln-ffi-build /ffi/build/...` and any `LD_LIBRARY_PATH` references need the same `linux-<arch>-<variant>` prefix.

Tip to avoid threading `MLN_VARIANT` through every stage: at the end of the `mln-ffi-build` stage, symlink the variant dir to a stable name so downstream stages can just say `build/libmaplibre-native-c.so`:

```dockerfile
RUN MLN_VARIANT=$(cat /tmp/mln_variant) \
 && ln -sfn ${MLN_VARIANT} build/current
# Downstream stages then COPY from /ffi/build/current/...
```

### A4. Runtime image — Mesa drivers

The runtime stage installs `mesa-vulkan-drivers libvulkan1` today (for the in-process Vulkan path). For OpenGL builds, also install:

```dockerfile
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        libegl1 libegl-mesa0 libgl1-mesa-dri libgles2 \
        # keep existing vulkan packages so the image works regardless of which
        # GPU_BACKEND it was built for — small size cost, ops simplicity gain.
        mesa-vulkan-drivers libvulkan1 \
 && rm -rf /var/lib/apt/lists/*

# Headless EGL — required when there's no display server (the normal prod case).
ENV EGL_PLATFORM=surfaceless
```

For diagnostics: `LIBGL_ALWAYS_SOFTWARE=true` forces Mesa software rendering (llvmpipe). Leave unset in prod so the GPU driver wins when present.

### A5. Build invocation in CI

In whichever workflow builds the image (`.github/workflows/*.yml`), add a matrix:

```yaml
strategy:
  matrix:
    gpu_backend: [vulkan, opengl]
steps:
  - uses: docker/build-push-action@v5
    with:
      platforms: linux/amd64,linux/arm64
      build-args: |
        GPU_BACKEND=${{ matrix.gpu_backend }}
        MLN_FFI_REV=2587cf28854ae0636f6d8512572c0f387b58e81a
      tags: |
        ghcr.io/jfberry/rampardos-go-renderer:${{ matrix.gpu_backend }}-${{ github.sha }}
```

The cache-from/to layer cache should be keyed on `GPU_BACKEND` so the two variants don't invalidate each other's mln-ffi-build layers.

## Track B: Go renderer code changes

The current `rampardos/internal/services/renderer/gopool.go` uses jfberry's high-level `maplibre.Session` abstraction. Upstream binding is lower-level: separate `RuntimeHandle`, `MapHandle`, `RenderSessionHandle`, no built-in `RenderStillImage` helper, no thread dispatcher (caller must `runtime.LockOSThread`).

### B1. `rampardos/go.mod`

Remove the jfberry dep, add upstream:

```diff
- github.com/jfberry/maplibre-native-go v0.0.0-...
+ github.com/maplibre/maplibre-native-ffi/bindings/go v0.0.0
```

You have two options for resolving the upstream binding:

- **Vendor** (recommended for prod reproducibility): inside the docker build, after the mln-ffi clone step, `cp -r /ffi/bindings/go /vendor-maplibre-go` and add a `replace` directive in `go.mod` pointing at `/vendor-maplibre-go`. This guarantees binding version == C ABI version, both pinned by `MLN_FFI_REV`.
- **Public module path** (only works after upstream merges our PR): plain `require` line, GOPROXY resolves it. Don't rely on this yet — the PR isn't open.

### B2. API translation reference

| jfberry (current code in `gopool.go`)                            | upstream replacement                                                                            |
|------------------------------------------------------------------|--------------------------------------------------------------------------------------------------|
| `s, _ := maplibre.NewSession(ctx, opts)`                          | three separate handles: `rt, _ := maplibre.NewRuntime()`; `m, _ := rt.NewMapWithOptions(mapOpts)`; `sess, _ := m.AttachOpenGLOwnedTexture(descriptor)` |
| `s.JumpTo(maplibre.Camera{Fields:..., Latitude:..., Zoom:...})`   | `m.JumpTo(maplibre.CameraOptions{}.WithCenter(maplibre.LatLng{Latitude:..., Longitude:...}).WithZoom(...).WithBearing(...))` |
| `s.LoadStyle(url)`                                               | `m.SetStyleURL(url)` (or `m.SetStyleJSON(json)` for inline)                                       |
| `s.RenderStillImage(ctx)` blocks until frame is ready             | call `m.RequestStillImage()` then drive `rt.RunOnce()` + `rt.PollEvent()` in a loop. On each `RuntimeEventMapRenderUpdateAvailable` call `sess.RenderUpdate()`; on `RuntimeEventMapStillImageFinished` return |
| `s.ReadPremultipliedRGBA8Into(buf)`                               | `sess.ReadPremultipliedRGBA8Into(buf)` — same shape, different receiver                          |
| (no thread management)                                            | **`runtime.LockOSThread()` is mandatory** at the top of every worker goroutine before touching native handles. Upstream has no dispatcher; calls must originate on the owner thread |
| `s.Close()`                                                      | close in reverse order: `sess.Close()`, `m.Close()`, `rt.Close()` (frame must be released before any of these) |

### B3. Worker goroutine skeleton

```go
package renderer

import (
    "runtime"
    "errors"
    "fmt"
    "time"

    maplibre "github.com/maplibre/maplibre-native-ffi/bindings/go"
)

type worker struct {
    rt   *maplibre.RuntimeHandle
    m    *maplibre.MapHandle
    sess *maplibre.RenderSessionHandle
    egl  *eglContext  // see B4
    buf  []byte       // reusable readback buffer
}

func (w *worker) loop(reqs <-chan ViewportRequest, results chan<- *image.NRGBA) {
    runtime.LockOSThread()  // MUST be first
    defer runtime.UnlockOSThread()

    if err := w.init(); err != nil {
        // ... fatal log + exit
        return
    }
    defer w.close()

    for req := range reqs {
        img, err := w.renderOne(req)
        // ... deliver to results
    }
}

func (w *worker) renderOne(req ViewportRequest) (*image.NRGBA, error) {
    if err := w.m.JumpTo(req.toCameraOptions()); err != nil {
        return nil, err
    }
    if err := w.m.RequestStillImage(); err != nil {
        return nil, err
    }
    if err := pumpUntilStillFinished(w.rt, w.sess, 10*time.Second); err != nil {
        return nil, err
    }
    info, err := w.sess.ReadPremultipliedRGBA8Into(w.buf)
    if err != nil {
        return nil, err
    }
    return nrgbaFromPremultipliedRGBA8(w.buf, info), nil
}

// Pump cadence: WaitForEvent + drain.
//
// WaitForEvent blocks in libuv's epoll_wait until at least one runtime event
// is queued for PollEvent (or up to the timeout). Spurious libuv-internal
// wakeups are filtered inside C so they don't roundtrip through cgo; this is
// the primitive you want when the next step is always "drain events".
//
// Do NOT use time.Sleep here. A 1 ms sleep alone gave 191.7 FPS vs ~810
// with the right pump pattern.
//
// runtime.Gosched() works too but busy-spins at 100% of one core per active
// worker; WaitForEvent parks the thread in epoll_wait. CPU drops to ~30% of
// one core per worker for the same throughput, which lets you pack more
// workers into the same CPU quota.
//
// Pre-7206a1a, the pump used RunOnce + RunBlocking(10ms-on-idle). That works
// but pays a cgo crossing per libuv-internal wake; on workloads where
// non-event wakes outnumber productive events (the rampardos prod
// measurement was 60:1) those crossings dominate. WaitForEvent removes
// that overhead. On bench workloads where every wake is productive, the two
// patterns are equivalent.
func pumpUntilStillFinished(rt *maplibre.RuntimeHandle, sess *maplibre.RenderSessionHandle, budget time.Duration) error {
    rendered := false
    deadline := time.Now().Add(budget)
    for time.Now().Before(deadline) {
        remaining := time.Until(deadline)
        if remaining <= 0 {
            break
        }
        // Cap each wait at 10 ms so other lifecycle checks (cancellation,
        // health probes from the pool) can fire periodically. WaitForEvent's
        // internal loop already bounds the wait correctly; this outer cap
        // is just a heartbeat granularity.
        wait := remaining
        if wait > 10*time.Millisecond {
            wait = 10 * time.Millisecond
        }
        if _, err := rt.WaitForEvent(wait); err != nil {
            return fmt.Errorf("WaitForEvent: %w", err)
        }
        for {
            ev, err := rt.PollEvent()
            if err != nil {
                return fmt.Errorf("PollEvent: %w", err)
            }
            if ev == nil {
                break
            }
            switch ev.Type {
            case maplibre.RuntimeEventMapRenderUpdateAvailable:
                if err := sess.RenderUpdate(); err != nil {
                    return fmt.Errorf("RenderUpdate: %w", err)
                }
                rendered = true
            case maplibre.RuntimeEventMapStillImageFinished:
                if !rendered {
                    return errors.New("still image finished without a render frame")
                }
                return nil
            case maplibre.RuntimeEventMapLoadingFailed:
                return fmt.Errorf("map loading failed: %s", ev.Message)
            case maplibre.RuntimeEventMapRenderError:
                return fmt.Errorf("map render error: %s", ev.Message)
            case maplibre.RuntimeEventMapStillImageFailed:
                return fmt.Errorf("still image failed: %s", ev.Message)
            }
        }
    }
    return fmt.Errorf("render still timed out after %s", budget)
}
```

The whole `pumpUntilStillFinished` is the analogue of jfberry's `RenderStillImage` — copy it as-is from `examples/go-readback/main.go:pumpUntilStillImageFinished` in the upstream checkout.

### B4. `egl_linux.go` — copy from upstream example

Create `rampardos/internal/services/renderer/egl_linux.go` and copy the cgo preamble + `eglContext` type from `examples/go-readback/main.go` in the upstream checkout. Specifically the file should contain:

- The `#cgo linux pkg-config: egl` directive
- The `mln_go_egl_context` struct + `mln_go_egl_init` / `mln_go_egl_destroy` / `mln_go_egl_get_proc_address` C helpers
- A Go `eglContext` type wrapping the raw struct with `descriptor()` and `close()` methods returning `maplibre.OpenGLContextDescriptor` via `maplibre.NewOpenGLContextEGL(...)`.

One EGL context per worker (per OS thread). Don't try to share — EGL is thread-local.

### B5. Backend selection

```go
// in renderer/pool.go (or wherever workers are constructed)
backend := os.Getenv("RENDERER_GPU_BACKEND")  // "opengl" | "vulkan"

func (w *worker) attachSession(width, height uint32, scale float64) (*maplibre.RenderSessionHandle, error) {
    extent := maplibre.RenderTargetExtent{Width: width, Height: height, ScaleFactor: scale}
    switch w.backend {
    case "opengl":
        return w.m.AttachOpenGLOwnedTexture(maplibre.OpenGLOwnedTextureDescriptor{
            Extent:  extent,
            Context: w.egl.descriptor(),
        })
    case "vulkan":
        return w.m.AttachVulkanOwnedTexture(maplibre.VulkanOwnedTextureDescriptor{
            Extent:  extent,
            Context: w.vk.descriptor(),
        })
    default:
        return nil, fmt.Errorf("unknown RENDERER_GPU_BACKEND=%q", w.backend)
    }
}
```

The image variant (`GPU_BACKEND=opengl` build-arg → only the EGL native library + Mesa GL drivers shipped) and the renderer config (`RENDERER_GPU_BACKEND=opengl`) **must agree**. Add a startup check that calls `maplibre.SupportedRenderBackends().Has(maplibre.RenderBackendOpenGL)` and refuses to start if the runtime config picks a backend the linked native library doesn't support.

### B6. `gopool_stub.go`

The CGO_ENABLED=0 stub file at `rampardos/internal/services/renderer/gopool_stub.go` references the jfberry import too — update it to match the new import path. Keep the rest of the stub logic identical.

## Build & test loop

Inside this repo:

```bash
# Build both variants locally (linux/arm64 on Apple Silicon, linux/amd64 on x64 hosts).
docker buildx build \
  --build-arg GPU_BACKEND=opengl \
  --build-arg MLN_FFI_REV=2587cf28854ae0636f6d8512572c0f387b58e81a \
  -t rampardos-go-renderer:opengl-dev \
  --load .

docker buildx build \
  --build-arg GPU_BACKEND=vulkan \
  --build-arg MLN_FFI_REV=2587cf28854ae0636f6d8512572c0f387b58e81a \
  -t rampardos-go-renderer:vulkan-dev \
  --load .

# Quick smoke
docker run --rm \
  -e RENDERER_BACKEND=go-pool \
  -e RENDERER_GPU_BACKEND=opengl \
  -e EGL_PLATFORM=surfaceless \
  rampardos-go-renderer:opengl-dev \
  /opt/rampardos/bin/rampardos --help
```

For real perf comparison, two complementary harnesses:

1. **`github.com/jfberry/maplibre-bench-opengl`** — standalone Go bench dedicated to the upstream binding. Drives the same workload as the rampardos renderer (klokantech-basic + GB OSM mbtiles, same z15 viewport) but isolates the per-frame cost from the rampardos orchestrator overhead. Has a `--walk-mode random-gb` cache-killer mode that defeats mbgl's in-memory tile cache (mimics tile-server traffic where successive requests have no temporal coherence) and prints per-frame tile-action telemetry. **Use this as the reference for perf changes** — it's where the numbers in the perf table above came from. `make uk-walk` runs the linear-walk default; override `BENCH_ARGS` for cache-killer.

2. **`rampardos-rust-poc/rampardos-render-worker-rs/scripts/backend-bench.sh`** — drives the orchestrator + worker E2E. Already has a `BACKEND=vulkan|opengl` matrix for the Rust worker. Extend the Go worker branch the same way (separate `--go-binary` per backend, pointing at the relevant container's binary). The bench docs at `/Users/james/GolandProjects/maplibre-native-go/docs/bench-go-worker.md` describe the worker protocol.

## Production deployment

Two images per build (`*:opengl-<sha>`, `*:vulkan-<sha>`). To deploy:

1. Pull whichever image to the prod host.
2. Set in the orchestrator (k8s/compose/systemd):
   - `RENDERER_BACKEND=go-pool` (or whatever existing var picks the in-process renderer)
   - `RENDERER_GPU_BACKEND=opengl` (or `vulkan`)
   - `EGL_PLATFORM=surfaceless` (always, for headless containers)
3. **Do NOT** set `LIBGL_ALWAYS_SOFTWARE=true` in prod unless you want to force the software path. Without it, EGL picks the first ICD it finds, which on a host with a real GPU + drivers will be hardware-accelerated.
4. **Required** prod env (no measurable tradeoffs):
   ```
   EGL_PLATFORM=surfaceless          # OpenGL builds only
   ```
5. **Optional** env vars — A/B test in *your* prod env before shipping; none of these reproduced as wins under replication on the dev stack, but workloads differ. See "What we tried that didn't help" above for context.
   ```
   # If you want to experiment, do it one at a time with N ≥ 5 replicates,
   # measuring p50 + p99 + frame max separately.
   MLN_FFI_RESOURCE_LOADER=bypass    # synchronous MainResourceLoader
   ```

**Do not** set:
- `MESA_VK_VERSION_OVERRIDE=1.3` — frame max ballooned 6× in our test.
- `LIBGL_ALWAYS_SOFTWARE=true` in prod unless forcing the software path.
- `MESA_NO_ERROR=1` — small consistent regression.
- `mesa_glthread=true` — adds 5–7× tail-latency jitter.

Pre-flight check on the prod machine before rolling out:

```bash
docker run --rm rampardos-go-renderer:opengl-<sha> eglinfo | head -20
# Look for the EGL_VERSION line and "Vendor" string. Mesa software is "Mesa";
# nvidia hardware is "NVIDIA"; Intel is "Intel".
```

If the prod GPU driver doesn't support pbuffer surfaces (the binding's chosen surface type), EGL init will fail at session creation. Test in staging first.

## Things explicitly NOT in scope

- Don't touch the Node renderer path (`mbgl-renderer` worker) — only the in-process Go pool.
- Don't change the wire protocol between orchestrator and render workers.
- Don't update `mln-ffi` to anything past `2587cf28854ae0636f6d8512572c0f387b58e81a` without coordinating — that pin includes the merge of `origin/main` (which has WGL/EGL C ABI) into the upstream `golang` branch. Newer commits may have moved.

## What "done" looks like

1. CI produces `rampardos-go-renderer:opengl-<sha>` and `rampardos-go-renderer:vulkan-<sha>` images for both `linux/amd64` and `linux/arm64`.
2. Locally on the dev machine, both images pass smoke tests (the in-process renderer produces a non-degenerate PNG for a known viewport).
3. A bench run with `backend-bench.sh` + extended Go worker matrix produces four numbers (rust-vulkan, rust-opengl, go-vulkan, go-opengl) for the same workload, all within reasonable variance.
4. The opengl image, when run on the prod machine with the GPU driver present, reports a GPU vendor string other than "Mesa" via `eglinfo` (confirming hardware acceleration).
