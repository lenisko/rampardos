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
#                     version in rampardos/go.mod — the artifact records
#                     the commit it was built from and this script checks it.
#   MLN_FFI_SNAPSHOT_TAG  Release tag. Default: unstable-native-snapshot.

set -euo pipefail

# NOTE: while MLN_FFI_REV points at a PR branch (maplibre-native-ffi#631)
# there is NO published artifact for it — this script will fail the rev
# check by design. Build in Docker (mln-ffi-build stage) or copy
# /ffi/install out of that stage for host development.
MLN_FFI_DIR_HOST="${MLN_FFI_DIR_HOST:-$HOME/dev/maplibre-native-ffi}"
MLN_FFI_REV="${MLN_FFI_REV:-57237a83f98bfb51a90baedf04741c0fce46bbe1}"
MLN_FFI_SNAPSHOT_TAG="${MLN_FFI_SNAPSHOT_TAG:-unstable-native-snapshot}"

case "$(uname -s)-$(uname -m)" in
    Linux-aarch64|Linux-arm64) VARIANT=linux-arm64-egl ;;
    Linux-x86_64)              VARIANT=linux-x64-egl ;;
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

GOT="$(grep -o '[0-9a-f]\{40\}' "$MLN_FFI_DIR_HOST/install/share/maplibre-native-c/artifact.json" | head -1)"
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
