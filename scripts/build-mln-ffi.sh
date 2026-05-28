#!/usr/bin/env bash
# Build libmaplibre-native-c.so on the host for local rampardos
# development with the in-process Go renderer (RENDERER_BACKEND=go-pool).
#
# The same FFI build runs inside the Dockerfile's `mln-ffi-build` stage
# for CI image builds. This script is purely for the developer iteration
# loop: run rampardos with -tags mln_ffi against a host-built .so so
# changes to the Go-side glue don't require a Docker rebuild.
#
# The actual compilation runs *inside* an Ubuntu 24.04 container even on
# Linux hosts, both to match the runtime image's glibc/libstdc++ and to
# work uniformly from macOS/Linux dev machines without per-host setup of
# mise + pixi + clang. The container writes its build/ output back into
# the host directory you point it at.
#
# Usage:
#   ./scripts/build-mln-ffi.sh
#
# Env:
#   MLN_FFI_DIR_HOST  Host directory used as the FFI checkout + build
#                     root. Default: ~/dev/maplibre-native-ffi-linux.
#                     The script clones a fresh copy on first run.
#   MLN_FFI_REV       Commit to check out. Bump this when the Go binding
#                     bumps its required FFI revision; KEEP IN SYNC with
#                     the ARG MLN_FFI_REV at the top of the Dockerfile's
#                     mln-ffi-build stage.

set -euo pipefail

MLN_FFI_DIR_HOST="${MLN_FFI_DIR_HOST:-$HOME/dev/maplibre-native-ffi-linux}"
MLN_FFI_REPO="${MLN_FFI_REPO:-https://github.com/jfberry/maplibre-native-ffi}"
MLN_FFI_REV="${MLN_FFI_REV:-dfd7091efabd6d3bafb9f149271db8ae728e369d}"

mkdir -p "$MLN_FFI_DIR_HOST"

if [ ! -d "$MLN_FFI_DIR_HOST/.git" ]; then
    echo ">> cloning maplibre-native-ffi at $MLN_FFI_REV into $MLN_FFI_DIR_HOST"
    git clone "$MLN_FFI_REPO" "$MLN_FFI_DIR_HOST"
fi

echo ">> checking out $MLN_FFI_REV"
git -C "$MLN_FFI_DIR_HOST" fetch --depth 64 origin "$MLN_FFI_REV" || true
git -C "$MLN_FFI_DIR_HOST" checkout --force "$MLN_FFI_REV"

# Linux-only build dir keeps coexistence-friendly with a macOS
# checkout pointed at the same source tree (CMake caches are per-arch).
docker run --rm \
    -v "$MLN_FFI_DIR_HOST":/ffi \
    -w /ffi \
    -e MISE_DATA_DIR=/ffi/.mise-cache \
    -e PIXI_CACHE_DIR=/ffi/.pixi-cache \
    ubuntu:24.04 \
    bash -c '
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

apt-get update >/dev/null
apt-get install -y --no-install-recommends \
    ca-certificates curl git xz-utils \
    >/dev/null

curl -fsSL https://mise.run | sh
export PATH=/root/.local/bin:$PATH

mise trust --yes
mise install
eval "$(mise activate bash)"

mise run build

if [ ! -f build/libmaplibre-native-c.so ] || [ ! -f build/pkgconfig/maplibre-native-c.pc ]; then
    echo "ERROR: build did not produce expected artifacts" >&2
    ls -la build/ build/pkgconfig/ 2>&1 | head -40 >&2
    exit 1
fi

echo "OK"
echo "  $(realpath build/libmaplibre-native-c.so)"
echo "  $(realpath build/pkgconfig/maplibre-native-c.pc)"
'

cat <<EOF

================================================================
 FFI artifacts at $MLN_FFI_DIR_HOST/build/

 To build rampardos with the in-process Go renderer compiled in:

   cd rampardos
   PKG_CONFIG_PATH=$MLN_FFI_DIR_HOST/build/pkgconfig \\
   CGO_LDFLAGS="-Wl,-rpath,$MLN_FFI_DIR_HOST/build" \\
   CGO_ENABLED=1 \\
   go build -tags 'nodynamic mln_ffi' ./cmd/server

 Or just \`make build-go-renderer\` from the project root.

 To run with the Go backend selected:

   RENDERER_BACKEND=go-pool ./rampardos
================================================================
EOF
