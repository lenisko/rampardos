# rampardos-render-worker-rs

Proof-of-concept Rust render worker for rampardos.

Drop-in protocol-compatible replacement for `rampardos-render-worker/render-worker.js`
— same stdin/stdout framed-JSON protocol (see `rampardos/internal/services/renderer/protocol.go`),
same CLI flags, same mbtiles+file:// scheme handling. Goal is to evaluate
whether per-worker memory is lower and the known headless-rendering leak
(maplibre-native#248) is reduced versus the Node binding.

## Dependency

This crate pulls in a **forked** `maplibre-native-rs` that adds a
Rust-supplied `FileSource` callback — upstream v0.4.5 has no way for a
Rust caller to intercept `mbtiles://` or other custom URL schemes. See
`../../maplibre-native-rs-fork/src/cpp/rust_file_source.{h,cpp}` and
`../../maplibre-native-rs-fork/src/renderer/file_source.rs`.

## Building

macOS + Metal fails to link binaries against the prebuilt v0.4.5 amalgam
(armerge symbol localization). All dev/test runs inside the dev Docker
image, which uses the prebuilt Linux amalgam and Mesa's lavapipe software
Vulkan ICD for headless CPU rendering.

```sh
# Build the dev image (once).
docker build -f docker/Dockerfile.dev -t rampardos-rust-poc-dev .

# Interactive shell with sources + fixtures mounted.
docker run --rm -it \
  -v $PWD/..:/work \
  -v $PWD/../../maplibre-native-rs-fork:/maplibre-native-rs-fork \
  -v /Users/james/GolandProjects/rampardos-tileserver-replacement:/fixtures:ro \
  -e CARGO_TARGET_DIR=/tmp/target-linux \
  rampardos-rust-poc-dev
# inside the container:
#   cd /work/rampardos-render-worker-rs
#   cargo build --release
```

## Running

```sh
./rampardos-render-worker-rs \
  --style-id klokantech-basic \
  --style-path /fixtures/TileServer/Styles/klokantech-basic/style.prepared.json \
  --mbtiles /fixtures/TileServer/Datasets/Combined.mbtiles \
  --styles-dir /fixtures/TileServer/Styles \
  --fonts-dir /fixtures/TileServer/Fonts \
  --ratio 1
```

Speaks the frame protocol defined in
`rampardos/internal/services/renderer/protocol.go`. Writes an `H`
handshake frame on startup; then loops reading `R` requests and writing
`K` (RGBA) or `E` (error) responses.

## Bench

```sh
./bench \
  --node-script   /work/rampardos-render-worker/render-worker.js \
  --rust-binary   ./rampardos-render-worker-rs \
  --style-path    /fixtures/TileServer/Styles/klokantech-basic/style.prepared.json \
  --mbtiles       /fixtures/TileServer/Datasets/Combined.mbtiles \
  --styles-dir    /fixtures/TileServer/Styles \
  --fonts-dir     /fixtures/TileServer/Fonts \
  --n 1000 \
  --csv /tmp/bench.csv
```

Reports p50/p95/p99 per-render latency for each worker, RSS at cold /
100 / 500 / 1000 renders, and byte-equivalence verdict on the first
render.
