#!/usr/bin/env bash
# Run the Rust vs Node render-worker bench inside the dev Docker image.
#
# Prerequisites:
#   - docker image `rampardos-rust-poc-dev` built from docker/Dockerfile.dev
#   - The mbtiles + klokantech-basic fixtures at
#     /Users/james/GolandProjects/rampardos-tileserver-replacement/
#
# Iteration:
#   ./scripts/bench-in-docker.sh                      # default: release, 1000 renders
#   PROFILE=debug ./scripts/bench-in-docker.sh --n 100
#   ./scripts/bench-in-docker.sh --csv /tmp/b.csv
#
# The bench binary's own defaults come from src/bin/bench.rs (e.g. --n 1000).

set -euo pipefail
cd "$(dirname "$0")/.."

FIXTURES="${FIXTURES:-/Users/james/GolandProjects/rampardos-tileserver-replacement}"
POC_ROOT="${POC_ROOT:-$(cd .. && pwd)}"
FORK_ROOT="${FORK_ROOT:-$(cd ../../maplibre-native-rs-fork && pwd)}"
PROFILE="${PROFILE:-release}"

# Guard: rampardos-render-worker/ must contain package.json so the bench can
# install Node deps into the persistent volume once.
NODE_WORKER_DIR="$POC_ROOT/rampardos-render-worker"
if [ ! -f "$NODE_WORKER_DIR/package.json" ]; then
    echo "ERROR: $NODE_WORKER_DIR/package.json not found" >&2
    exit 1
fi

# The style.prepared.json has baked-in host paths; mount the fixtures at the
# SAME path inside the container so the file:// + mbtiles:// URLs resolve.
BENCH_ARGS=(
    --node-script /work/rampardos-render-worker/render-worker.js
    --rust-binary "/target-linux/$PROFILE/rampardos-render-worker-rs"
    --style-path   "$FIXTURES/TileServer/Styles/klokantech-basic/style.prepared.json"
    --mbtiles      "$FIXTURES/TileServer/Datasets/Combined.mbtiles"
    --styles-dir   "$FIXTURES/TileServer/Styles"
    --fonts-dir    "$FIXTURES/TileServer/Fonts"
    --style-id     klokantech-basic
    "$@"
)

docker run --rm \
    -v "$POC_ROOT":/work \
    -v "$FORK_ROOT":/maplibre-native-rs-fork \
    -v "$FIXTURES:$FIXTURES:ro" \
    -v rampardos-rust-target:/target-linux \
    -v rampardos-cargo-registry:/root/.cargo/registry \
    -v rampardos-node-modules:/work/rampardos-render-worker/node_modules \
    -e CARGO_TARGET_DIR=/target-linux \
    -e EGL_PLATFORM=surfaceless \
    -e PROFILE="$PROFILE" \
    -e BENCH_ARGS="${BENCH_ARGS[*]}" \
    -w /work/rampardos-render-worker-rs \
    rampardos-rust-poc-dev \
    bash -c '
set -euo pipefail

# Ensure Node deps are installed in the shared volume (idempotent).
if [ ! -d /work/rampardos-render-worker/node_modules/@maplibre ]; then
    echo ">> installing node_modules (first run)"
    cd /work/rampardos-render-worker && npm install --omit=optional
fi

# Build Rust worker + bench. Target dir is persisted in the rampardos-rust-target
# volume; the cargo registry is persisted separately so crate sources do not
# redownload on every run.
PROFILE_FLAG=""
[ "$PROFILE" = "release" ] && PROFILE_FLAG="--release"
cd /work/rampardos-render-worker-rs
cargo build $PROFILE_FLAG --bin rampardos-render-worker-rs --bin bench

# Xvfb for the Node maplibre-native binding. It uses OpenGL via GLX/EGL and
# does not consistently work with EGL_PLATFORM=surfaceless, so we fall back
# to an X11 surface on an in-container Xvfb screen.
rm -f /tmp/.X0-lock /tmp/.X11-unix/X0 2>/dev/null || true
Xvfb :0 -screen 0 1024x768x24 -nolisten tcp >/dev/null 2>&1 &
export DISPLAY=:0
export LIBGL_ALWAYS_SOFTWARE=1
export MESA_GL_VERSION_OVERRIDE=3.3
sleep 0.5

echo ">> running bench ($PROFILE) with: $BENCH_ARGS"
exec /target-linux/$PROFILE/bench $BENCH_ARGS
'
