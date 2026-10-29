#!/usr/bin/env bash
# Fetch upstream's published maplibre-native-ffi release archive for local
# development with the in-process Go renderer.
#
# The Docker build fetches the same archive in its mln-ffi-build stage.
# This script exists so a Linux developer can build and test outside
# Docker against the identical library.
#
# Env:
#   MLN_FFI_DIR_HOST  Where to extract. Default: ~/dev/maplibre-native-ffi
#   MLN_FFI_VERSION   Release version. KEEP IN SYNC with the ARG at the top
#                     of the Dockerfile and with the binding version in
#                     rampardos/go.mod.

set -euo pipefail

MLN_FFI_DIR_HOST="${MLN_FFI_DIR_HOST:-$HOME/dev/maplibre-native-ffi}"
MLN_FFI_VERSION="${MLN_FFI_VERSION:-0.202610.3}"

case "$(uname -s)-$(uname -m)" in
    Linux-aarch64|Linux-arm64) VARIANT=linux-gnu-arm64-egl ;;
    Linux-x86_64)              VARIANT=linux-gnu-x64-egl ;;
    Darwin-*)
        echo "The renderer is Linux-only; build and test in Docker on macOS." >&2
        echo "  docker build --target rampardos-test ." >&2
        exit 1 ;;
    *) echo "unsupported host: $(uname -s)-$(uname -m)" >&2; exit 1 ;;
esac

ASSET="maplibre-native-c-${VARIANT}.tar.gz"
BASE="https://github.com/maplibre/maplibre-native-ffi/releases/download/v${MLN_FFI_VERSION}"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo ">> fetching $ASSET from v$MLN_FFI_VERSION"
curl -fsSL "${BASE}/${ASSET}" -o "$WORK/$ASSET"
curl -fsSL "${BASE}/SHA256SUMS" -o "$WORK/SHA256SUMS"
(cd "$WORK" && grep " ${ASSET}\$" SHA256SUMS | sha256sum -c -)

rm -rf "$MLN_FFI_DIR_HOST/install"
mkdir -p "$MLN_FFI_DIR_HOST/install"
tar -xzf "$WORK/$ASSET" --strip-components=1 -C "$MLN_FFI_DIR_HOST/install"

cat <<MSG

================================================================
 maplibre-native-ffi $MLN_FFI_VERSION
 extracted to $MLN_FFI_DIR_HOST/install

 Build rampardos against it:
   make build-go-renderer

 Or directly:
   cd rampardos
   PKG_CONFIG_PATH=$MLN_FFI_DIR_HOST/install/share/pkgconfig \\
   CGO_ENABLED=1 go build -tags nodynamic ./cmd/server
================================================================
MSG
