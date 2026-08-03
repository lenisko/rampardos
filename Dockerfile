# syntax=docker/dockerfile:1

# Global so both the FFI build and the Go build see the same value: the
# native library and the Go binding pinned in go.mod must come from one
# upstream commit, and the rampardos-build stage asserts that.
ARG MLN_FFI_REV=92e6736979d5ced7b17e867f22bc087ae6053dc0

# ================================
# Get Git commit SHA
# ================================
FROM alpine:3.20 AS git-info
RUN apk add --no-cache git
WORKDIR /repo
COPY .git/ .git/
RUN git rev-parse HEAD > /git-commit.txt

# ================================
# Build tippecanoe (for mbtiles combine/tile-join in admin)
# ================================
FROM alpine:3.20 AS tippecanoe-build

RUN apk add --no-cache build-base sqlite-dev sqlite-static zlib-dev zlib-static git bash

RUN git clone --depth 1 -b 1.36.0 https://github.com/mapbox/tippecanoe.git \
    && cd tippecanoe \
    && make -j$(nproc) LDFLAGS="-static -static-libgcc -static-libstdc++" \
    && make install \
    && mkdir -p /tippecanoe-out \
    && cp /usr/local/bin/tippecanoe* /usr/local/bin/tile-join /tippecanoe-out/

# ================================
# Build fontnik / build-glyphs (for font processing in admin)
# ================================
FROM debian:bookworm-slim AS fontnik-build
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential python3 git ca-certificates curl zlib1g-dev \
    && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y nodejs \
    && rm -rf /var/lib/apt/lists/*
RUN if [ "$(dpkg --print-architecture)" = "arm64" ]; then \
    git clone --depth 1 -b fix-build-errors-node14 https://github.com/lenisko/node-fontnik.git /fontnik \
    && cd /fontnik \
    && mkdir .toolchain \
    && CC=gcc CXX=g++ CXXFLAGS="-Wno-error=maybe-uninitialized" \
       npm install --build-from-source --foreground-scripts; \
    else \
    mkdir -p /fontnik && cd /fontnik && npm install fontnik@0.7.4; \
    fi
RUN find /fontnik/node_modules -type f \( -name "*.md" -o -name "*.ts" -o -name "*.map" -o -name "LICENSE*" -o -name "README*" -o -name "CHANGELOG*" \) -delete \
    && find /fontnik/node_modules -type d \( -name "test" -o -name "tests" -o -name "docs" -o -name "example" -o -name "examples" \) -exec rm -rf {} + 2>/dev/null || true

# ================================
# maplibre-native-ffi — the C ABI shared library the in-process Go
# renderer links against.
#
# We consume upstream's published artifact rather than building from
# source. Building it ourselves meant tracking their bootstrap, and that
# bootstrap moved four times in as many months: pixi to system compilers,
# then CMake presets plus a Rust platform layer, then a Zig cross
# toolchain, then a submodule marked `update = none` with patches applied
# by their own script. Each break cost a build cycle to diagnose, and none
# of it was our concern — the artifact is the interface, not their build.
#
# The artifact is also better than what we produced: a glibc 2.17 floor
# from their Zig toolchain rather than the build image's, no libstdc++ ABI
# requirement, RUNPATH already $ORIGIN, and EGL resolved through their
# dlopen dispatch so the library needs only libc, libm, libpthread and
# libdl. That deletes the conda-lib bundling and the patchelf pass this
# stage used to carry.
#
# The snapshot tag is rolling: assets are replaced in place as upstream
# publishes. The gitSha check is what keeps that honest — a moved snapshot
# fails the build naming the SHA to bump to, rather than silently linking
# a library the Go binding in go.mod was not built against.
# ================================
FROM ubuntu:24.04 AS mln-ffi-build
ARG MLN_FFI_REV
ARG MLN_FFI_SNAPSHOT_TAG=unstable-native-snapshot
ARG TARGETARCH
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /tmp/mln
RUN set -eu; \
    case "$TARGETARCH" in \
      arm64) MLN_ARCH=arm64;; \
      amd64) MLN_ARCH=x64;; \
      *) echo "unsupported TARGETARCH=$TARGETARCH" >&2; exit 1;; \
    esac; \
    asset="maplibre-native-c-linux-${MLN_ARCH}-egl.tar.gz"; \
    base="https://github.com/maplibre/maplibre-native-ffi/releases/download/${MLN_FFI_SNAPSHOT_TAG}"; \
    curl -fsSL "${base}/${asset}" -o "$asset"; \
    curl -fsSL "${base}/SHA256SUMS" -o SHA256SUMS; \
    grep " ${asset}$" SHA256SUMS | sha256sum -c -; \
    mkdir -p /ffi/install; \
    tar -xzf "$asset" --strip-components=1 -C /ffi/install; \
    test -f /ffi/install/lib/libmaplibre-native-c.so; \
    test -f /ffi/install/share/pkgconfig/maplibre-native-c.pc; \
    got="$(grep -o '[0-9a-f]\{40\}' /ffi/install/share/maplibre-native-c/artifact.json | head -1)"; \
    if [ "$got" != "$MLN_FFI_REV" ]; then \
      echo "snapshot moved: artifact is $got, MLN_FFI_REV is $MLN_FFI_REV" >&2; \
      echo "bump MLN_FFI_REV, then: cd rampardos && go get github.com/maplibre/maplibre-native-ffi/bindings/go@$got" >&2; \
      exit 1; \
    fi; \
    echo "native artifact at $got"; \
    rm -rf /tmp/mln

# ================================
# Build Go binary
#
# The renderer is Linux-only by file suffix (gopool_linux.go), so a Linux
# build always includes it — there is no build tag to forget. The binding
# comes from the module proxy like any other dependency; only the native
# library and its headers come from the mln-ffi-build stage.
#
# Switched from golang:1.26-alpine (musl, CGO_ENABLED=0) to glibc-based
# golang:1.26 because libmaplibre-native-c.so is built on Ubuntu 24.04
# and links against its glibc/libstdc++. CGO must use a compatible
# runtime ABI; alpine's musl would not.
#
# No explicit --platform: buildx defaults to TARGETPLATFORM, which is
# what we want. CGO cross-compilation needs a per-arch toolchain we
# don't currently install, so under multi-arch builds QEMU runs the
# Go compile natively in each target arch.
# ================================
FROM golang:1.26 AS rampardos-build
ENV DEBIAN_FRONTEND=noninteractive
# pkg-config is required by BOTH cgo consumers here: egl_linux.go declares
# `#cgo linux pkg-config: egl` (from libegl1-mesa-dev), and the upstream Go
# binding declares `#cgo pkg-config: maplibre-native-c` — the latter is why
# PKG_CONFIG_PATH below points at the FFI's installed .pc. A cgo pkg-config
# directive still runs even when CGO_CFLAGS/CGO_LDFLAGS are set, so the .pc
# has to be findable; CGO_LDFLAGS only adds the runtime rpath on top.
RUN apt-get update \
 && apt-get install -y --no-install-recommends pkg-config libegl1-mesa-dev \
 && rm -rf /var/lib/apt/lists/*
COPY --from=mln-ffi-build /ffi/install/ /ffi/install/
WORKDIR /src
COPY --from=git-info /git-commit.txt /git-commit.txt
COPY rampardos/go.mod rampardos/go.sum ./
# The Go binding and the C ABI it calls must come from the same commit.
# go.mod pins a pseudo-version whose suffix is the upstream commit, so
# compare it against the revision the native library was built from rather
# than trusting the two to be updated together.
ARG MLN_FFI_REV
RUN set -eu; \
    want="$(printf '%s' "${MLN_FFI_REV}" | cut -c1-12)"; \
    got="$(go list -m -f '{{.Version}}' github.com/maplibre/maplibre-native-ffi/bindings/go | sed 's/.*-//')"; \
    if [ "$want" != "$got" ]; then \
      echo "binding/ABI mismatch: go.mod has $got, MLN_FFI_REV is $want" >&2; \
      echo "run: cd rampardos && go get github.com/maplibre/maplibre-native-ffi/bindings/go@${MLN_FFI_REV}" >&2; \
      exit 1; \
    fi; \
    echo "binding matches native library at $got"
RUN go mod download
COPY rampardos/ ./
RUN GIT_COMMIT=$(cat /git-commit.txt) && \
    PKG_CONFIG_PATH=/ffi/install/share/pkgconfig \
    CGO_LDFLAGS="-Wl,-rpath,/opt/rampardos/lib" \
    CGO_ENABLED=1 \
    go build -trimpath -tags nodynamic \
    -ldflags="-s -w -X github.com/lenisko/rampardos/internal/version.gitCommitFromLdflags=${GIT_COMMIT}" \
    -o /out/rampardos ./cmd/server

# ================================
# Test stage — vet + unit tests with the native library present.
#
# Built as its own target so CI can run it without producing an image, and
# based on rampardos-build so it inherits the source, the module cache and
# the FFI, making it a cheap layer on top of a cache hit rather than a
# second FFI build.
#
# This is the only place the renderer tests can run: they need
# libmaplibre-native-c.so at link time, and the renderer is Linux-only, so
# `go test ./...` on a developer Mac silently skips the whole package.
# ================================
FROM rampardos-build AS rampardos-test
# The test binaries link the .so but nothing has set an rpath for them, so
# point the loader at the install tree.
ENV LD_LIBRARY_PATH=/ffi/install/lib
RUN PKG_CONFIG_PATH=/ffi/install/share/pkgconfig CGO_ENABLED=1 \
    go vet ./...
RUN PKG_CONFIG_PATH=/ffi/install/share/pkgconfig CGO_ENABLED=1 \
    go test -count=1 -race ./...

# ================================
# Final runtime image
# ================================
# Ubuntu 24.04 (not Debian Bookworm) because maplibre-native's prebuilt
# mbgl.node requires glibc ≥ 2.38 and GLIBCXX ≥ 3.4.32, which Debian 12
# (glibc 2.36) does not provide. Ubuntu 24.04 ships glibc 2.39.
FROM --platform=$TARGETPLATFORM ubuntu:24.04 AS runtime
WORKDIR /app

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
    ca-certificates curl \
    # Node.js via NodeSource
 && curl -fsSL https://deb.nodesource.com/setup_24.x | bash - \
 && apt-get install -y --no-install-recommends nodejs \
    # maplibre-native (Node binding) runtime deps — Mesa/OpenGL headless
    libglx0 libgl1 libegl1 libgbm1 libopengl0 \
    # maplibre-native (Go binding via FFI) runtime deps — Mesa EGL with
    # the llvmpipe software DRI driver for fully headless render. EGL
    # surfaceless platform is selected via EGL_PLATFORM=surfaceless below.
    # libgles2 provides libGLESv2.so.2: upstream's EGL build links it
    # directly (DT_NEEDED on libmaplibre-native-c.so), where the previous
    # fork build did not — without it the binary fails at load with
    # "libGLESv2.so.2: cannot open shared object file".
    libegl-mesa0 libgl1-mesa-dri libgles2 \
    # fontnik / build-glyphs (admin font processing) runs on Node and
    # needs these; libmaplibre-native-c.so itself links only EGL/GLES/libc
    # because upstream vendors ICU and static-links libuv and zlib.
    libcurl4 libjpeg8 libwebp7 libpng16-16 libicu74 \
    # SQLite — mbgl's mbtiles file source reads the dataset directly
    libsqlite3-0 \
 && apt-get purge -y curl \
 && apt-get autoremove -y \
 && rm -rf /var/lib/apt/lists/*

# Tippecanoe binaries (tile-join for mbtiles operations)
COPY --from=tippecanoe-build /tippecanoe-out/ /usr/local/bin/

# Fontnik (build-glyphs for font processing)
COPY --from=fontnik-build /fontnik /app/fontnik
RUN if [ -x /app/fontnik/bin/build-glyphs ]; then \
      ln -s /app/fontnik/bin/build-glyphs /usr/local/bin/build-glyphs; \
    else \
      ln -s /app/fontnik/node_modules/.bin/build-glyphs /usr/local/bin/build-glyphs; \
    fi

# maplibre-native-ffi shared library. Reached only via the chain
#     rampardos (DT_RPATH=/opt/rampardos/lib)
#       → libmaplibre-native-c.so (DT_RPATH=$ORIGIN, set in mln-ffi-build)
# so the Node binding's mbgl.node and any other process that doesn't load
# libmaplibre-native-c.so resolves its deps from /usr/lib/<triple>/ as
# Ubuntu intended. Earlier revisions registered /opt/rampardos/lib with
# ldconfig globally, which shadowed Ubuntu's libpng for everything else in
# the image — fontnik's node addon aborted on a version mismatch.
#
# The published artifact carries RUNPATH=$ORIGIN already and needs only
# libc, libm, libpthread and libdl — EGL is resolved through upstream's
# dlopen dispatch — so there is nothing to bundle and no patchelf pass.
COPY --from=mln-ffi-build /ffi/install/lib /tmp/ffi-lib
RUN mkdir -p /opt/rampardos/lib \
 && cp -L /tmp/ffi-lib/*.so* /opt/rampardos/lib/ \
 && rm -rf /tmp/ffi-lib

# Go binary
COPY --from=rampardos-build /out/rampardos /app/rampardos

# Create directories
RUN mkdir -p Cache/Tile Cache/Static Cache/StaticMulti Cache/Marker Cache/Regeneratable \
    TileServer/Fonts TileServer/Styles TileServer/Datasets Templates Markers

# Software rasterisation via Mesa llvmpipe — there is no GPU on the host.
ENV LIBGL_ALWAYS_SOFTWARE=1
ENV MESA_GL_VERSION_OVERRIDE=3.3
# EGL surfaceless platform selects Mesa's headless EGL implementation, so
# egl_linux.go creates its context with no display server at all. This is
# why the image no longer needs Xvfb or DISPLAY: those existed only for
# the Node binding, which required an X display for its GL context.
ENV EGL_PLATFORM=surfaceless
EXPOSE 9000

ENTRYPOINT ["/app/rampardos"]
