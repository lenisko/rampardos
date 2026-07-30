# syntax=docker/dockerfile:1

# Global so both the FFI build and the Go build see the same value: the
# native library and the Go binding pinned in go.mod must come from one
# upstream commit, and the rampardos-build stage asserts that.
ARG MLN_FFI_REV=91ecc920462420f977959d546d2735823d2f0092

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
# Build maplibre-native-ffi (libmaplibre-native-c.so) — the C ABI shared
# library the in-process Go renderer (RENDERER_BACKEND=go-pool) links
# against via the maplibre-native-go binding.
#
# Heavy: ~10-20 min cold. mise installs pixi, which brings clang +
# cmake + ninja into a conda env; CMake then fetches maplibre-native
# source at configure time. BuildKit's layer cache (cache-from/to=gha)
# amortises this across PRs as long as MLN_FFI_REV is stable.
#
# Pin MUST track the maplibre-native-go binding's required FFI revision.
# See scripts/build-mln-ffi.sh for the host-side equivalent.
# ================================
FROM ubuntu:24.04 AS mln-ffi-build
ARG MLN_FFI_REPO=https://github.com/maplibre/maplibre-native-ffi
ARG MLN_FFI_REV
# Keep in sync with the FFI's mise.toml [vars] at MLN_FFI_REV.
ARG MLN_CMAKE_VERSION=4.3.3
ARG MLN_RUST_VERSION=1.95.0
ARG MLN_CARGO_ABOUT_VERSION=0.9.1
ARG TARGETARCH
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
    ca-certificates curl git xz-utils \
 && rm -rf /var/lib/apt/lists/*
SHELL ["/bin/bash", "-c"]
WORKDIR /ffi
RUN git clone "${MLN_FFI_REPO}" . \
 && git checkout ${MLN_FFI_REV}

# Bypass mise entirely. The FFI's root mise.toml pulls a tool per binding
# (dotnet, Java, Rust, Node, Zig, Python, Swift, ...) and mise auto-installs
# during activation even when only one is asked for, turning every missing
# toolchain into a build break. We assemble the C-library toolchain by hand
# instead — three sources:
#
#   * apt      — the project's own Linux bootstrap set, copied from
#                mise.linux.toml [bootstrap.packages]: system compilers,
#                EGL/GLES headers, ICU, ninja, pkg-config, glslang.
#                Upstream dropped pixi (no pixi.toml as of 91ecc92), so the
#                C/C++ toolchain is now the distro's, and libuv/zlib are
#                FetchContent-built while ICU is vendored — no conda env
#                and no library bundling needed downstream.
#   * Kitware  — CMake, pinned to the version mise pins (vars.cmake_version).
#                Ubuntu 24.04 ships 3.28; CMakeLists requires >= 4.0.
#   * rustup   — cargo for the Rust platform layer (ureq HTTP, linked into
#                the Linux build). Ubuntu's cargo 1.75 cannot parse the
#                workspace manifest (`resolver = "3"` needs >= 1.84).
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
    build-essential clang glslang-tools libclang-dev \
    libegl1-mesa-dev libgles2-mesa-dev libicu-dev \
    libncurses6 libsqlite3-0 libvulkan-dev \
    ninja-build pkg-config \
 && rm -rf /var/lib/apt/lists/*

ENV PATH=/opt/cmake/bin:/root/.cargo/bin:$PATH

RUN case "$TARGETARCH" in \
        arm64) TOOL_ARCH=aarch64;; \
        amd64) TOOL_ARCH=x86_64;; \
        *) echo "unsupported TARGETARCH=$TARGETARCH" >&2; exit 1;; \
    esac \
 && curl -fsSL "https://github.com/Kitware/CMake/releases/download/v${MLN_CMAKE_VERSION}/cmake-${MLN_CMAKE_VERSION}-linux-${TOOL_ARCH}.tar.gz" -o /tmp/cmake.tgz \
 && mkdir -p /opt/cmake \
 && tar -xzf /tmp/cmake.tgz -C /opt/cmake --strip-components=1 \
 && rm /tmp/cmake.tgz \
 && cmake --version \
 && curl -fsSL https://sh.rustup.rs | sh -s -- -y --profile minimal --default-toolchain "${MLN_RUST_VERSION}" \
 && cargo --version \
 # cargo-about is find_program(... REQUIRED) in cmake/mln_rust.cmake — it
 # generates the Rust dependency license notices linked into the library,
 # with no opt-out. Take the pinned prebuilt (static musl) rather than
 # `cargo install`, which would compile it from source on every build.
 && curl -fsSL "https://github.com/EmbarkStudios/cargo-about/releases/download/${MLN_CARGO_ABOUT_VERSION}/cargo-about-${MLN_CARGO_ABOUT_VERSION}-${TOOL_ARCH}-unknown-linux-musl.tar.gz" -o /tmp/cargo-about.tgz \
 && tar -xzf /tmp/cargo-about.tgz -C /tmp \
 && install -m 0755 "/tmp/cargo-about-${MLN_CARGO_ABOUT_VERSION}-${TOOL_ARCH}-unknown-linux-musl/cargo-about" /usr/local/bin/cargo-about \
 && rm -rf /tmp/cargo-about.tgz "/tmp/cargo-about-${MLN_CARGO_ABOUT_VERSION}-${TOOL_ARCH}-unknown-linux-musl" \
 && cargo-about --version

# Fetch the maplibre-native submodule (normally done by mise's
# postinstall hook).
RUN git submodule sync --recursive third_party/maplibre-native \
 && git submodule update --init --recursive --depth 1 third_party/maplibre-native

# Configure + build + install via the upstream CMake preset. The preset
# carries the backend/provider cache vars (opengl + egl) and installs into
# build/<preset>/install, which is the layout the Go binding expects
# (bindings/go/mise.toml points PKG_CONFIG_PATH at
# <install>/share/pkgconfig). `cmake --workflow` runs configure+build; the
# install step is explicit because the workflow preset does not include it.
RUN case "$TARGETARCH" in \
        arm64) MLN_ARCH=arm64;; \
        amd64) MLN_ARCH=x64;; \
        *) echo "unsupported TARGETARCH=$TARGETARCH" >&2; exit 1;; \
    esac \
 && MLN_PRESET="linux-${MLN_ARCH}-egl" \
 && echo "$MLN_PRESET" > /tmp/mln_preset \
 && cmake --workflow --preset "$MLN_PRESET" \
 && cmake --install "build/${MLN_PRESET}" \
 && ln -sfn "${MLN_PRESET}/install" build/current \
 && test -f build/current/lib/libmaplibre-native-c.so \
 && test -f build/current/share/pkgconfig/maplibre-native-c.pc \
 && ls -la build/current/lib build/current/share/pkgconfig

# Point the .so at its own directory so any transitive private deps resolve
# without registering a global ldconfig path. A global path previously
# shadowed Ubuntu's libpng/libjpeg for other consumers in the image
# (fontnik's node addon still links its own copies).
RUN apt-get update \
 && apt-get install -y --no-install-recommends patchelf binutils \
 && rm -rf /var/lib/apt/lists/* \
 && for lib in build/current/lib/*.so*; do \
        patchelf --set-rpath '$ORIGIN' "$lib" || echo "WARN: patchelf failed on $lib"; \
    done \
 && echo "--- libmaplibre-native-c.so dynamic deps ---" \
 && readelf -d build/current/lib/libmaplibre-native-c.so | grep -E 'RUNPATH|RPATH|NEEDED' || true

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
# build/current is a symlink into the preset's install tree, so copy the
# resolved directory (lib/, include/, share/pkgconfig/).
COPY --from=mln-ffi-build /ffi/build/current/ /ffi/install/
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
# Upstream 91ecc92 dropped pixi: libuv/zlib are FetchContent-built and ICU
# is vendored into the .so, so there is no conda-env bundle to carry — only
# the library itself, plus the system libs Ubuntu already provides.
COPY --from=mln-ffi-build /ffi/build/current/lib /tmp/ffi-lib
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
