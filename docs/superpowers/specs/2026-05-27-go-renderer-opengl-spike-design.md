# Go renderer OpenGL spike — design

## Status

Design approved 2026-05-27. Implementation plan to follow via `writing-plans`.

## Goal

Determine whether the in-process Go renderer, driven through Mesa OpenGL
(EGL surfaceless) instead of Vulkan (lavapipe), can match or beat the Node
renderer's perf in production. The Vulkan implementation (via the
`jfberry/maplibre-native-go` binding) was evaluated in prod and lost
consistently to Node on the metrics we care about. This spike tests the
hypothesis that the GPU API — not the binding overhead — was the cause.

If OpenGL ties or beats Node, the Go renderer becomes the deprecation path
for Node and we follow up with a proper migration. If not, the Go renderer
is sunk effort and node-pool remains the only production backend.

## Non-goals

- **Not** a production migration. Maintainability concerns are descoped.
- **Not** keeping the Vulkan path. The prod measurement is conclusive enough
  that side-by-side comparison adds no information.
- **Not** introducing a `RENDERER_GPU_BACKEND` runtime selector. Backend is
  fixed at link time by which FFI variant is bundled.
- **Not** a tri-backend CI matrix. Single image, single variant.
- **Not** changing the Node renderer path, wire protocol, or any other
  in-process subsystem.
- **Not** local-dev-friendly. The host has glibc skew that requires Docker
  deployment; local dev with the new binding is out of scope.

## Constraints

- Must deploy via Docker (glibc skew on the prod host).
- Must observe via the existing rampardos Grafana dashboards — no new bench
  harness inside the repo.
- Must preserve the `RENDERER_BACKEND={node-pool,go-pool}` env switch and
  the `renderer.Renderer` interface so the existing integration test and
  HTTP handlers work unchanged.
- Must keep the untagged build (CGO_ENABLED=0) green for environments that
  don't have the FFI bundled — i.e. `gopool_stub.go` continues to compile.

## Architecture

One Dockerfile, one Go binary, one FFI variant (`linux-<arch>-egl`).

### FFI build stage

- Source repo: `https://github.com/jfberry/maplibre-native-ffi` (was
  `sargunv/maplibre-native-ffi`).
- Pinned commit: `2587cf28854ae0636f6d8512572c0f387b58e81a` (was
  `b438365…`). Includes the upstream `golang` binding branch, `origin/main`
  merge for the WGL/EGL C ABI, plus the Go OpenGL Linux binding and the
  `go-readback` example.
- `MISE_ENV=linux-<arch>-egl` selected from `$TARGETARCH` (`amd64→x64`,
  `arm64→arm64`). Variant build produces
  `build/linux-<arch>-egl/libmaplibre-native-c.so` and matching pkg-config.
- Symlink `build/linux-<arch>-egl` → `build/current` so downstream stages
  don't need to thread the variant name through.
- The existing `ldd` bundling and `patchelf` step retarget to
  `build/current/`.
- `cp -r bindings/go /vendor-maplibre-go` so the Go build stage can
  consume the binding source via a `replace` directive.
- Memory note: C++ unity files can need ~6 GB per cc1plus instance. If CI
  OOMs, set `CMAKE_BUILD_PARALLEL_LEVEL=3`. GitHub-hosted Ubuntu runners
  have 16 GB so the default parallelism should be fine.

### Go build stage

- `go.mod`: remove `github.com/jfberry/maplibre-native-go`; add
  `github.com/maplibre/maplibre-native-ffi/bindings/go v0.0.0` with a
  `replace` directive pointing at `/vendor-maplibre-go` (in-Docker copy
  from the FFI stage).
- `COPY --from=mln-ffi-build /ffi/build/current /ffi/build/current` (was
  `/ffi/build`).
- `COPY --from=mln-ffi-build /ffi/include /ffi/include` (unchanged).
- `PKG_CONFIG_PATH=/ffi/build/current/pkgconfig` (was `/ffi/build/pkgconfig`).
- Drop `libvulkan-dev` from `apt-get install` (compile-time dep for the
  jfberry binding's Vulkan headers, no longer needed).
- Build tags unchanged: `-tags 'nodynamic mln_ffi'`.

### Runtime stage

- **Add** Mesa EGL runtime packages: `libegl-mesa0 libgl1-mesa-dri`.
  (`libegl1` and `libgles2` are already pulled in by Node's deps.) These
  provide the EGL implementation + the llvmpipe software DRI driver.
- **Remove** `mesa-vulkan-drivers libvulkan1` — no longer needed since the
  Vulkan path is removed.
- `COPY --from=mln-ffi-build /ffi/build/current/libmaplibre-native-c.so
  /usr/local/lib/` plus the same retargeting for the bundled transitive
  `.so` files. `ldconfig` as today.
- `ENV EGL_PLATFORM=surfaceless` — selects Mesa's headless EGL platform
  (no display server required).
- Xvfb is kept — the Node renderer still needs it for GLX-on-X11. Once
  Node is deprecated (post-spike), Xvfb can go too.

### Go renderer code (`rampardos/internal/services/renderer/`)

**`gopool.go` — full rewrite in place.**

The current code uses jfberry's high-level `*maplibre.Session`
abstraction, which hides thread management and exposes a single blocking
`RenderInto`. Upstream's binding is lower-level: three separate handles
(`RuntimeHandle`, `MapHandle`, `RenderSessionHandle`), no built-in
still-image helper, no thread dispatcher (caller must
`runtime.LockOSThread`).

**Worker model:**

The current code acquires a `*Session` from a channel and uses it inline
on the caller goroutine. That doesn't work under upstream's API — every
native call must originate on the OS thread that created the Runtime.
Workers become long-lived goroutines pinned via `runtime.LockOSThread`,
communicating with the dispatcher via command channels.

Per-worker state:

```
goWorker {
    rt      *maplibre.RuntimeHandle           // created once at init
    m       *maplibre.MapHandle                // created once, SetStyleURL on reload
    sess    *maplibre.RenderSessionHandle     // resized on extent change
    egl     *eglContext                       // one EGL context per OS thread
    buf     []byte                             // reusable readback buffer
    extent  (uint32, uint32, float64)         // current (W, H, scale)
}
```

Worker loop:

```
LockOSThread → init (EGL → Runtime → Map → Attach session at default 256×256 → SetStyleURL)
for cmd := range cmds:
  switch cmd.kind:
    case render:
      if cmd.extent != current: sess.Resize(W, H, scale); current = cmd.extent
      m.JumpTo(camera options)
      m.RequestStillImage()
      pumpUntilStillFinished(rt, sess, budget)
      sess.ReadPremultipliedRGBA8Into(buf)
      reply(image, err)
    case reload:
      m.SetStyleURL(newURL)
      reply(err)
close: sess.Close → m.Close → rt.Close → egl.close → UnlockOSThread
```

`pumpUntilStillFinished` is copied verbatim from upstream's
`examples/go-readback/main.go`. The single load-bearing rule, documented
in a block comment on the function: **never `time.Sleep` between
`RunOnce` iterations** — `runtime.Gosched()` only. A 1 ms sleep costs
2.5× p50 per the upstream reference numbers.

**Pool structure (mostly unchanged):**

- Per-`(styleID, scale)` pool, lazily created via `getOrCreatePool` —
  same as today.
- N worker goroutines per pool (where N = per-pool slot count, default
  `cfg.PoolSize` = `GOMAXPROCS(0)`).
- Unbuffered `chan workerCommand` to the workers — natural backpressure,
  one in-flight render per worker.
- Global process-wide semaphore acquired in dispatch before sending the
  command — same two-level concurrency as today.
- `ReloadStyles` broadcasts reload commands to every worker in every
  pool, blocks until all reply. Reload serialises behind any in-flight
  render on each worker. Under sustained load this briefly serialises
  reload, matching today's behaviour.

**`egl_linux.go` — new file.**

Copied verbatim from upstream's `examples/go-readback/main.go`:

- `#cgo linux pkg-config: egl` directive.
- C struct `mln_go_egl_context` with `mln_go_egl_init` /
  `mln_go_egl_destroy` / `mln_go_egl_get_proc_address` helpers.
- Go `eglContext` type wrapping the raw struct, with `descriptor()`
  returning `maplibre.OpenGLContextDescriptor` via
  `maplibre.NewOpenGLContextEGL(...)` and a `close()` method.

One EGL context per worker. EGL is thread-local — never share across
goroutines.

**`gopool_stub.go` — minimal edit.**

Same stub body, just swap the import path to the upstream binding so the
untagged build still compiles. Continues to error with
"build with -tags mln_ffi" if `NewGoPoolRenderer` is called without the
tag.

### Startup validation

Single check at `NewGoPoolRenderer`:

```go
backends := maplibre.SupportedRenderBackends()
if !backends.Has(maplibre.RenderBackendOpenGL) {
    return nil, fmt.Errorf("FFI built without OpenGL support; check Dockerfile MISE_ENV variant")
}
```

Catches a wrong FFI variant at process start, not at first render.

The existing canary render in `main.go` covers everything else — EGL
surface creation, Mesa driver load, style parse, tile render. If the
canary passes, the dispatch path is alive.

### Pool size

`RENDERER_POOL_SIZE` continues to default to `GOMAXPROCS(0)`. The plan's
note about "~100% per active worker" via the Gosched pump applies, but
that's symmetric with the Node renderer (which also pegs 100% per active
worker during render). Workers idle on a channel between renders → 0% CPU.
No tuning needed for the spike; revisit if prod measurements show
over-subscription.

## Testing scope

**Verified:**

- `go build` and `go vet` with `-tags 'nodynamic mln_ffi'` (CI).
- `go build` untagged stays green via `gopool_stub.go` (CI, no FFI needed).
- Existing `integration_test.go` parameterised over
  `RENDERER_BACKEND={node-pool, go-pool}` exercises both dispatch paths
  against the same expected output — should pass for the rewritten
  `gopool.go` with no test changes, since the `renderer.Renderer`
  interface is unchanged.
- `Dockerfile` builds cleanly for both `linux/amd64` and `linux/arm64`
  via the existing post-rebase parallel-arch CI.
- One-off manual smoke render in a freshly built container against
  `klokantech-basic` before deploying to prod.

**Skipped:**

- Unit tests for the new worker model. Integration test covers
  correctness; unit-testing the pump loop mostly tests the upstream
  binding.
- Synthetic bench harness inside this repo. Measurement happens via the
  prod Grafana dashboards.
- `RENDERER_GPU_BACKEND` runtime selector (no backend selection in the
  binary — link-time fixed).
- Tri-backend CI matrix.
- Local dev workflow. The host's glibc skew already forces Docker
  deployment; the spike inherits that constraint.

## Rollout

1. Land the changes on a feature branch.
2. CI builds the EGL-variant image for amd64 + arm64.
3. Deploy the image to one prod node with `RENDERER_BACKEND=go-pool`.
4. Observe latency histograms, throughput, and RSS in Grafana vs the
   node-pool baseline on a peer node.
5. Decision point: if OpenGL ties or beats Node on the killer metric
   from the previous prod evaluation, schedule the proper migration. If
   not, abandon the Go renderer and the spike branch.

## Risks

- **EGL surface creation fails in the prod container.** Already validated
  by the user — EGL works on the prod host. Spike inherits that as a
  given.
- **The new pump loop has a perf footgun.** A `time.Sleep` between
  `RunOnce` iterations costs 2-4× p50 per upstream's reference. Mitigated
  by a block-comment warning on `pumpUntilStillFinished` and by copying
  the function verbatim from upstream's working example.
- **Pin drift.** `MLN_FFI_REV` is a custom fork pin. Lock once for the
  spike, do not bump until the comparison is done.
- **FFI variant mismatch.** Building with the wrong variant would link
  successfully but fail at first render. Caught at startup by the
  `SupportedRenderBackends` check.
- **OpenGL still loses to Node.** This is the spike's purpose — the
  branch gets abandoned and we have a confirmed answer. No production
  surface area is changed during the spike.

## What "done" looks like

1. CI produces a runnable image for both `linux/amd64` and `linux/arm64`.
2. Integration tests pass for both `RENDERER_BACKEND` values.
3. The image, when deployed to prod with `RENDERER_BACKEND=go-pool`,
   serves real traffic without crashing or producing degraded renders.
4. A directional comparison vs Node is visible in Grafana within 24 hours
   of deploy.
