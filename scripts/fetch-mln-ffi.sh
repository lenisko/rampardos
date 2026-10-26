#!/usr/bin/env bash
# Fetch upstream's published maplibre-native-ffi artifact for local
# development with the in-process Go renderer.
#
# The Docker build does the same thing in its mln-ffi-build stage. This
# script exists so a Linux developer can build and test outside Docker
# against the identical library.
#
# Replaces the previous build-from-source script. Upstream's bootstrap
# moved four times in as many months (pixi, CMake presets + Rust, a Zig
# cross toolchain, a submodule marked `update = none` with patches), and
# tracking it bought us nothing the artifact does not already provide.
#
# Env:
#   MLN_FFI_DIR_HOST  Where to extract. Default: ~/dev/maplibre-native-ffi
#   MLN_FFI_REV       Expected upstream commit. KEEP IN SYNC with the ARG
#                     at the top of the Dockerfile and with the pseudo-
#                     version in rampardos/go.mod. The artifact no longer
#                     records its commit, so this script checks the commit
#                     the release names instead.
#   MLN_FFI_SNAPSHOT_TAG  Release tag. Default: unstable-native-snapshot.

set -euo pipefail

# NOTE: the Dockerfile builds the library from source at MLN_FFI_REV, and
# the rolling snapshot matches that commit only on the day it was pinned —
# after that this script fails the rev check by design. Copy /ffi/install
# out of the mln-ffi-build stage for host development instead.
MLN_FFI_DIR_HOST="${MLN_FFI_DIR_HOST:-$HOME/dev/maplibre-native-ffi}"
MLN_FFI_REV="${MLN_FFI_REV:-6ff238cb0c2618342518715027a616c974f1fb70}"
MLN_FFI_SNAPSHOT_TAG="${MLN_FFI_SNAPSHOT_TAG:-unstable-native-snapshot}"

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
BASE="https://github.com/maplibre/maplibre-native-ffi/releases/download/${MLN_FFI_SNAPSHOT_TAG}"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo ">> fetching $ASSET from $MLN_FFI_SNAPSHOT_TAG"
curl -fsSL "${BASE}/${ASSET}" -o "$WORK/$ASSET"
curl -fsSL "${BASE}/SHA256SUMS" -o "$WORK/SHA256SUMS"
(cd "$WORK" && grep " ${ASSET}\$" SHA256SUMS | sha256sum -c -)

rm -rf "$MLN_FFI_DIR_HOST/install"
mkdir -p "$MLN_FFI_DIR_HOST/install"
tar -xzf "$WORK/$ASSET" --strip-components=1 -C "$MLN_FFI_DIR_HOST/install"

GOT="$(curl -fsSL "https://api.github.com/repos/maplibre/maplibre-native-ffi/releases/tags/${MLN_FFI_SNAPSHOT_TAG}" | grep -o '"target_commitish": *"[0-9a-f]\{40\}"' | grep -o '[0-9a-f]\{40\}')"
if [ "$GOT" != "$MLN_FFI_REV" ]; then
    echo "" >&2
    echo "snapshot moved: artifact is $GOT, expected $MLN_FFI_REV" >&2
    echo "The tag is rolling, so upstream has published since this pin." >&2
    echo "To adopt it, bump MLN_FFI_REV in the Dockerfile, Makefile and this" >&2
    echo "script, then:" >&2
    echo "  cd rampardos && go get github.com/maplibre/maplibre-native-ffi/bindings/go@$GOT" >&2
    exit 1
fi

cat <<MSG

================================================================
 maplibre-native-ffi $GOT
 extracted to $MLN_FFI_DIR_HOST/install

 Build rampardos against it:
   make build-go-renderer

 Or directly:
   cd rampardos
   PKG_CONFIG_PATH=$MLN_FFI_DIR_HOST/install/share/pkgconfig \\
   CGO_ENABLED=1 go build -tags nodynamic ./cmd/server
================================================================
MSG
