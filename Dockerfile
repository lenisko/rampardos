# syntax=docker/dockerfile:1

# maplibre-native-ffi release the native library is fetched from. The Go
# binding pinned in go.mod must be the same release, and the
# rampardos-build stage asserts that.
ARG MLN_FFI_VERSION=0.202610.3

# ================================
# Get Git commit SHA
# ================================
FROM alpine:3.20 AS git-info
RUN apk add --no-cache git
WORKDIR /repo
COPY .git/ .git/
# In a git worktree the context's .git is a pointer file into a gitdir
# outside the build context, so HEAD cannot be resolved; stamp "unknown"
# there instead of failing the build. CI builds from a full clone and
# still stamps the real SHA.
RUN git rev-parse HEAD > /git-commit.txt 2>/dev/null || echo unknown > /git-commit.txt

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
    && cp /usr/local/bin/tippecanoe* /usr/local/bin/tile-join /tippecanoe-out/ \
    && strip /tippecanoe-out/*

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
# The arm64 from-source build leaves its toolchain, sources and dev
# dependencies behind (~270 MB, mostly mason_packages and aws-sdk). The
# addon build-glyphs loads is lib/binding/fontnik.node; build/ only holds
# an intermediate copy. On amd64 none of these paths exist.
RUN rm -rf \
        /fontnik/.git \
        /fontnik/.mason \
        /fontnik/.toolchain \
        /fontnik/mason_packages \
        /fontnik/build \
        /fontnik/src \
        /fontnik/test \
        /fontnik/bench \
        /fontnik/scripts \
        /fontnik/cloudformation \
        /fontnik/fonts \
        /fontnik/proto \
        /fontnik/package-lock.json \
        /fontnik/binding.gyp \
        /fontnik/common.gypi \
        /fontnik/local.env \
        /fontnik/node_modules/aws-sdk \
        /fontnik/node_modules/benchmark \
        /fontnik/node_modules/.cache \
    && find /fontnik/node_modules -type f \( -name "*.md" -o -name "*.ts" -o -name "*.map" -o -name "LICENSE*" -o -name "README*" -o -name "CHANGELOG*" \) -delete \
    && find /fontnik/node_modules -type d \( -name "test" -o -name "tests" -o -name "docs" -o -name "example" -o -name "examples" \) -exec rm -rf {} + 2>/dev/null || true
# The runtime takes its node from here so build-glyphs runs on the same
# Node ABI the addon was built against. node needs only libc, libstdc++
# and libgcc_s, which the runtime base provides at newer versions.
RUN cp /usr/bin/node /node-stripped && strip /node-stripped

# ================================
# maplibre-native-ffi — the C ABI shared library the in-process Go
# renderer links against.
#
# Upstream's versioned release archive for the target arch, checked
# against the SHA-256 pinned here (from the release's SHA256SUMS). The
# archive is already an install tree (lib/, include/, share/pkgconfig
# with a ${pcfiledir}-relative .pc), so it extracts straight into the
# /ffi/install path every downstream stage consumes. Releases are
# immutable, unlike the rolling unstable-native-snapshot.
#
# To bump: set MLN_FFI_VERSION, copy the two linux-gnu-*-egl sums from
# the release's SHA256SUMS, and `go get` the matching binding tag (the
# rampardos-build stage asserts it).
# ================================
FROM alpine:3.20 AS mln-ffi-build
ARG MLN_FFI_VERSION
ARG MLN_FFI_SHA256_AMD64=8543931826fec23e3650913b7e7922ed678dd5ac866e08233e806f5e369a091f
ARG MLN_FFI_SHA256_ARM64=2d5e2c88b9ee582e51234f3ad6a4ff05b74ca6c56cd01d111c1e68a2ba89ebb5
ARG TARGETARCH
RUN apk add --no-cache curl
RUN set -eu; \
    case "$TARGETARCH" in \
        arm64) MLN_ARCH=arm64; SUM="$MLN_FFI_SHA256_ARM64";; \
        amd64) MLN_ARCH=x64;   SUM="$MLN_FFI_SHA256_AMD64";; \
        *) echo "unsupported TARGETARCH=$TARGETARCH" >&2; exit 1;; \
    esac; \
    ASSET="maplibre-native-c-linux-gnu-${MLN_ARCH}-egl.tar.gz"; \
    curl -fsSL "https://github.com/maplibre/maplibre-native-ffi/releases/download/v${MLN_FFI_VERSION}/${ASSET}" -o "/tmp/${ASSET}"; \
    echo "${SUM}  /tmp/${ASSET}" | sha256sum -c -; \
    mkdir -p /ffi/install; \
    tar -xzf "/tmp/${ASSET}" --strip-components=1 -C /ffi/install; \
    rm "/tmp/${ASSET}"; \
    test -f /ffi/install/lib/libmaplibre-native-c.so; \
    test -f /ffi/install/share/pkgconfig/maplibre-native-c.pc; \
    grep -q "\"version\": \"${MLN_FFI_VERSION}\"" /ffi/install/share/maplibre-native-c/artifact.json

# ================================
# Build Go binary
#
# The renderer is Linux-only by file suffix (gopool_linux.go), so a Linux
# build always includes it — there is no build tag to forget. The binding
# comes from the module proxy like any other dependency; only the native
# library and its headers come from the mln-ffi-build stage.
#
# Switched from golang:1.26-alpine (musl, CGO_ENABLED=0) to glibc-based
# golang:1.26 because the linux-gnu libmaplibre-native-c.so links
# against glibc. CGO must use a compatible runtime ABI; alpine's musl
# would not.
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
# The Go binding and the C ABI it calls must come from the same release.
# Compare go.mod's binding version against the version the installed
# library's artifact.json records, rather than trusting the two to be
# bumped together.
RUN set -eu; \
    want="v$(sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' /ffi/install/share/maplibre-native-c/artifact.json)"; \
    got="$(go list -m -f '{{.Version}}' github.com/maplibre/maplibre-native-ffi/bindings/go)"; \
    if [ "$want" != "$got" ]; then \
      echo "binding/ABI mismatch: go.mod has $got, native library is $want" >&2; \
      echo "run: cd rampardos && go get github.com/maplibre/maplibre-native-ffi/bindings/go@$want" >&2; \
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
# The runtime glibc must cover the symbol versions the Go binary picks up
# from rampardos-build (golang:1.26, Debian trixie, glibc 2.41); today it
# references nothing newer than GLIBC_2.34. libmaplibre-native-c.so is
# built with `zig cc` against glibc 2.17. Ubuntu 24.04 ships glibc 2.39.
FROM --platform=$TARGETPLATFORM ubuntu:24.04 AS runtime
WORKDIR /app

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
    ca-certificates \
    # maplibre-native (Go binding via FFI) runtime deps — Mesa EGL with
    # the llvmpipe software DRI driver for fully headless render. EGL
    # surfaceless platform is selected via EGL_PLATFORM=surfaceless below.
    # libgles2 provides libGLESv2.so.2: upstream's EGL build links it
    # directly (DT_NEEDED on libmaplibre-native-c.so), where the previous
    # fork build did not — without it the binary fails at load with
    # "libGLESv2.so.2: cannot open shared object file".
    # libmaplibre-native-c.so itself links only EGL/GLES/libc because
    # upstream vendors ICU and static-links libuv and zlib, and
    # build-glyphs needs only libc/libstdc++, so no image or network
    # libraries are installed for either.
    libegl1 libegl-mesa0 libgl1-mesa-dri libgles2 \
    # SQLite — mbgl's mbtiles file source reads the dataset directly
    libsqlite3-0 \
 && rm -rf /var/lib/apt/lists/*

# Tippecanoe binaries (tile-join for mbtiles operations)
COPY --from=tippecanoe-build /tippecanoe-out/ /usr/local/bin/

# Fontnik (build-glyphs for font processing), with the node it was built
# against — build-glyphs is the only Node consumer left in the image.
COPY --from=fontnik-build /node-stripped /usr/bin/node
COPY --from=fontnik-build /fontnik /app/fontnik
RUN if [ -x /app/fontnik/bin/build-glyphs ]; then \
      ln -s /app/fontnik/bin/build-glyphs /usr/local/bin/build-glyphs; \
    else \
      ln -s /app/fontnik/node_modules/.bin/build-glyphs /usr/local/bin/build-glyphs; \
    fi

# maplibre-native-ffi shared library. Reached only via the chain
#     rampardos (DT_RPATH=/opt/rampardos/lib)
#       → libmaplibre-native-c.so (RUNPATH=$ORIGIN, as published)
# so any process that doesn't load libmaplibre-native-c.so resolves its
# deps from /usr/lib/<triple>/ as Ubuntu intended. Earlier revisions
# registered /opt/rampardos/lib with ldconfig globally, which shadowed
# Ubuntu's libpng for everything else in the image — fontnik's node addon
# aborted on a version mismatch.
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
# Each renderer worker brings its own native threads, and glibc gives
# threads their own malloc arenas that it rarely returns to the OS. With
# workers retiring and regrowing, the default arena count let RSS climb
# without bound (~3 MB per retired worker in a local churn probe); two
# shared arenas held it flat.
ENV MALLOC_ARENA_MAX=2
# The Go-side counterpart. With GOGC=100 the heap grows to ~2x the live
# set before collecting, and freed spans return to the OS only slowly. A
# soft limit makes the GC collect and scavenge as the heap nears it. Keep
# it comfortably above COMPOSITE_IMAGE_CACHE_MB plus TILE_IMAGE_CACHE_MB
# and the count-bounded marker image cache, or the GC runs continuously.
# Override per deployment; see #34.
ENV GOMEMLIMIT=1GiB
EXPOSE 9000

ENTRYPOINT ["/app/rampardos"]
