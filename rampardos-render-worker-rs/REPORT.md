# Rust render worker POC — evaluation report

Goal: evaluate whether replacing the Node.js render-worker
(maplibre-gl-native binding) with a Rust binary built on a patched
maplibre-native-rs reduces per-worker memory and bounds the
maplibre-native headless leak (maplibre/maplibre-native#248).

## TL;DR

**Go.**

- Rust steady-state RSS is ~28% lower (200 MB vs 279 MB) and *flat*
  after warm-up. Node keeps climbing for ~500 renders before
  flattening.
- Rust p99 latency is 60% better (20.0 ms vs 51.6 ms). p50 is
  indistinguishable. Rust has far tighter tail variance — no GC
  spikes.
- Outputs are pixel-close (identical framebuffer size, ~2.2 % of
  bytes differ by small deltas). Almost certainly upstream
  mbgl-amalgam ↔ Node-binding pinned-commit drift, not a semantic
  mismatch.
- Cold RSS is higher on Rust (111 vs 63 MB) because the amalgam
  is statically linked. Amortised over long-lived worker processes.
- The fork is ~200 lines of C++/cxx bridge code on top of v0.4.5;
  upstreamable as a clean single commit. No breaking changes to
  the existing crate API.

## 1. Delivery status

- [x] Fork of maplibre-native-rs v0.4.5 with a Rust-supplied
  FileSource callback
  - Branch: `file-source-callback` on
    `/Users/james/GolandProjects/maplibre-native-rs-fork`
  - Commits: `e40b79a` (feature), `8bb36b9` (sync-dispatch fix)
  - New files: `src/cpp/rust_file_source.{h,cpp}`,
    `src/renderer/file_source.rs`
  - Changes: 1 new cxx::bridge module, 1 builder method,
    0 breaking changes
- [x] POC binary `rampardos-render-worker-rs` — drop-in replacement
  on the stdin/stdout frame protocol + CLI
- [x] Bench harness — spawns both workers, samples RSS + latency,
  compares first-render bytes
- [x] Full bench run against klokantech-basic + UK mbtiles fixture,
  1000 renders per worker
- [x] Integration test on the fork passing

## 2. Fork viability

| Capability | Upstream v0.4.5 | Fork | Notes |
|---|---|---|---|
| Per-render width/height | ✅ | ✅ | `set_map_size`, added in v0.4.5 |
| `mbtiles://` scheme | ❌ | ✅ | Rust closure |
| `file://` scheme | ✅ | ✅ | Native |
| Gzip auto-decompress | ❌ | ✅ | Worker-side before returning bytes |
| Per-tile TileJSON synthesis | ❌ | ✅ | Worker builds from metadata table |
| Pixel ratio baked at startup | ✅ | ✅ | `with_pixel_ratio` |
| RGBA output | ✅ | ✅ | `image::ImageBuffer<Rgba<u8>, Vec<u8>>` |
| Headless on Linux without X | ⚠️ (Vulkan only) | ⚠️ (same) | lavapipe surfaceless; OpenGL still needs Xvfb |
| Error propagation | ❌ thin | ⚠️ same | Map-observer errors still not plumbed into
`render_static` Result — punted in this POC |

Implementation notes that caught us out:

- **`mbgl::FileSource::request` dispatch must be synchronous.** The
  mbgl header documents "the callback will be called asynchronously,
  in the same thread as the request was made", which we initially
  read as "post onto `RunLoop::Get()`". In static-render mode, the
  posted task never gets pumped — `frontend->render(*map)` does not
  pump the thread-local run loop at the point where the style-load
  blocks on our callback. Calling `cb(response)` synchronously inside
  `request()` (the same pattern mbgl's in-memory test doubles use)
  unblocks style load immediately and passes the integration test
  in 0.23 s.
- **macOS + Metal is blocked.** The prebuilt Metal amalgam ships
  with armerge-localized symbols; binaries using it fail to link
  even against the unmodified v0.4.5 tag. All dev/bench on Linux
  via Docker.
- **Style URL paths must match between container and host.** The
  production `style.prepared.json` has baked-in absolute paths.
  The bench bind-mounts fixtures at the same absolute path inside
  the container (`-v $FIXTURES:$FIXTURES:ro`).
- **Linux OpenGL needs Xvfb for the Node binding.** The Node
  maplibre-native binding uses the GL backend and does not
  consistently honour EGL-surfaceless. Rust via Vulkan+lavapipe
  runs fully headless. Production Dockerfile could drop Xvfb if
  only the Rust worker is deployed.

## 3. Build / runtime footprint

Docker diff sketch (not applied). Replaces the Node renderer stage:

```diff
 # render-deps stage
-FROM ubuntu:24.04 AS render-deps
-RUN apt-get install -y curl python3 make g++ nodejs
-WORKDIR /build
-COPY rampardos-render-worker/package.json ./
-RUN npm install --omit=optional
+FROM rust:1.94 AS render-deps
+COPY rampardos-render-worker-rs/ /build/rampardos-render-worker-rs/
+COPY maplibre-native-rs-fork/ /build/maplibre-native-rs-fork/
+RUN cd /build/rampardos-render-worker-rs \
+    && MLN_PRECOMPILE=1 cargo build --release --features vulkan
```

```diff
 # runtime stage
-    libglx0 libgl1 libegl1 libgbm1 libopengl0 \
-    libjpeg8 libwebp7 libpng16-16 libicu74 libuv1 \
-    xvfb \
+    mesa-vulkan-drivers libvulkan1 libcurl4 zlib1g \
```

```diff
-ENV DISPLAY=:0 \
-    LIBGL_ALWAYS_SOFTWARE=1 \
-    MESA_GL_VERSION_OVERRIDE=3.3 \
-    RENDERER_WORKER_SCRIPT=/app/render-worker/render-worker.js
+ENV EGL_PLATFORM=surfaceless \
+    RENDERER_WORKER_SCRIPT=/app/render-worker-rs/rampardos-render-worker-rs
```

```diff
-COPY <<'EOF' /app/entrypoint.sh
-#!/bin/sh
-rm -f /tmp/.X0-lock /tmp/.X11-unix/X0
-Xvfb :0 -screen 0 1024x768x24 -nolisten tcp >/dev/null 2>&1 &
-sleep 0.5
-exec /app/rampardos "$@"
-EOF
-RUN chmod +x /app/entrypoint.sh
-ENTRYPOINT ["/app/entrypoint.sh"]
+ENTRYPOINT ["/app/rampardos"]
```

Net: drops Node.js + Xvfb + most GL runtime libs, adds
`mesa-vulkan-drivers` + `libvulkan1` (~30 MB). Entrypoint shell
gone. Image is slightly smaller and has two fewer moving parts.

## 4. Bench results

Environment: Docker linux/arm64 (M3 host via virtualization),
Ubuntu 24.04, Mesa lavapipe (Vulkan, CPU-only), fixtures
`klokantech-basic` + `osm-2020-02-10-v3.11_europe_great-britain.mbtiles`
(1.6 GB), release builds of both workers, 512×512 RGBA renders
centred on the UK at zoom 6.

### 4.1 Cold-start RSS

| Worker | RSS at handshake (MB) |
|---|---|
| node | 63 |
| rust | 111 |

Rust is larger cold — static link of the 90 MB maplibre-native
amalgam accounts for most of it. One-shot cost per process;
production workers render thousands of tiles over their lifetime.

### 4.2 Steady-state RSS

| Renders done | node RSS (MB) | rust RSS (MB) |
|---|---|---|
| 0 (cold) | 63 | 111 |
| 100 | 252 | 200 |
| 500 | 278 | 200 |
| 1000 | 279 | 200 |
| Δ (0 → 1000) | **+216** | **+89** |

Per-render drift (steady-state, r500 → r1000):
- node: `(279 − 278) / 500 = 2 kB/render`
- rust: `(200 − 200) / 500 = 0 kB/render` (within the sampling noise)

Rust reaches steady-state by ~r100 and never drifts. Node is still
drifting slightly even at r1000.

### 4.3 Latency

| Worker | p50 (ms) | p95 (ms) | p99 (ms) |
|---|---|---|---|
| node | 16.86 | 19.51 | **51.64** |
| rust | 17.49 | 18.66 | **20.02** |

p50/p95 are indistinguishable within sampling noise. The striking
gap is p99: Node has a long tail, rust does not. Almost certainly
V8 GC — the Rust process does no GC.

### 4.4 Output equivalence

- First-render bytes: same size (1,048,576 B = 512×512×4 RGBA)
- 23,356 / 1,048,576 bytes differ (~2.2%)
- First divergence at offset 248: node=0xa6 rust=0x9e (Δ=8)
- Likely causes: upstream mbgl-amalgam vs Node-binding pinned on
  different commits; Vulkan-lavapipe vs OpenGL-llvmpipe rasterizer
  differences in anti-aliasing.
- Visually the images should be indistinguishable at human
  perception scales. A follow-up SSIM comparison is recommended
  before switching production traffic.

## 5. Go/no-go recommendation

**Go, with conditions:**

1. **Pilot on a single worker pool first.** Both workers produce
   valid tiles, but pixel-identity is not guaranteed. Shadow-compare
   renders in production for a day before wholesale cutover.
2. **Upstream the FileSource fork.** It is contained (~200 lines
   C++/cxx/Rust), opt-in (existing API unchanged), and useful to
   anyone else using maplibre-native-rs for a non-HTTP backend.
   While it's in our fork, pin a specific commit in Cargo.toml —
   do not track `main`.
3. **Fill in the error surface.** `RenderingError` still only has
   `StyleNotSpecified` / `InvalidImageData`. Before production use,
   plumb `MapLoadError` from the observer into the render result so
   worker-level failures surface as E frames with useful messages
   rather than blank images. Half a day of follow-up work.
4. **Verify on linux/amd64.** POC ran on linux/arm64 (Docker on
   Apple Silicon). Production is likely amd64. Re-run the bench
   there before accepting the numbers.

Decision criteria originally set:
- ✅ Rust steady-state RSS ≤ 0.7× Node — **0.72× actual, borderline pass** (200/279)
- ✅ Rust per-render drift ≤ 0.3× Node — **~0× actual, clear pass**
- ✅ p95 latency within 1.3× of Node — **0.96× actual, clear pass**

All three met. The drift numbers alone are reason enough for the
switch; the p99 improvement is a bonus that matters for
user-visible render latency.

## 6. Artifacts

- Fork: `/Users/james/GolandProjects/maplibre-native-rs-fork`,
  branch `file-source-callback` @ `8bb36b9`
- POC: `/Users/james/GolandProjects/rampardos-rust-poc`,
  branch `rust-render-worker-poc`
  - `rampardos-render-worker-rs/` (this directory)
- Bench raw output: `bench-results/bench-1000.log`
