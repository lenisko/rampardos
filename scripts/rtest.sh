#!/bin/bash
# rtest.sh <image-tag> [go test args...]: build the test stage, then run
# go test in it with Mesa installed (the renderer needs EGL at runtime).
# Pass -tags renderer_integration,mln_pkgconfig for the integration
# tests: an explicit -tags replaces the image's GOFLAGS tag, and the
# binding needs mln_pkgconfig to link the library (see the Dockerfile).
# Exits with go test's status.
set -e
tag=$1; shift
cd "$(dirname "$0")/.."
docker build -q --target rampardos-test -t "$tag" . >/dev/null
docker run --rm "$tag" bash -o pipefail -c "apt-get update -qq >/dev/null && apt-get install -y -qq --no-install-recommends libegl1 libegl-mesa0 libgl1-mesa-dri libgles2 sqlite3 >/dev/null 2>&1 && PKG_CONFIG_PATH=/ffi/install/share/pkgconfig CGO_ENABLED=1 go test -count=1 $* 2>&1 | grep -vE '^time='"
