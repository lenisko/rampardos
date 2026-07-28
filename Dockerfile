# syntax=docker/dockerfile:1

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
ARG MLN_FFI_REV=91ecc920462420f977959d546d2735823d2f0092
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

# Vendor the Go binding source so the rampardos-build stage can consume it
# via a go.mod `replace` directive. Bound to the FFI commit → guarantees
# binding version == C ABI version.
RUN cp -r bindings/go /vendor-maplibre-go \
 && ls /vendor-maplibre-go | head -20

# Point the .so at its own directory so any transitive private deps resolve
# without registering a global ldconfig path (which previously shadowed
# system libpng/libjpeg for the Node binding when RENDERER_BACKEND=node-pool).
RUN apt-get update \
 && apt-get install -y --no-install-recommends patchelf binutils \
 && rm -rf /var/lib/apt/lists/* \
 && for lib in build/current/lib/*.so*; do \
        patchelf --set-rpath '$ORIGIN' "$lib" || echo "WARN: patchelf failed on $lib"; \
    done \
 && echo "--- libmaplibre-native-c.so dynamic deps ---" \
 && readelf -d build/current/lib/libmaplibre-native-c.so | grep -E 'RUNPATH|RPATH|NEEDED' || true

# ================================
# Render worker deps (maplibre-gl-native + better-sqlite3)
# ================================
# Must use the same base as the runtime (Ubuntu 24.04) so npm downloads
# prebuilt binaries compatible with the runtime's glibc.
FROM --platform=$TARGETPLATFORM ubuntu:24.04 AS render-deps
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
    ca-certificates curl python3 make g++ \
 && curl -fsSL https://deb.nodesource.com/setup_24.x | bash - \
 && apt-get install -y --no-install-recommends nodejs \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /build
COPY rampardos-render-worker/package.json ./
RUN npm install --omit=optional \
 && rm -f /build/package-lock.json

# ================================
# Build Go binary
#
# Builds with -tags 'nodynamic mln_ffi' so the in-process Go renderer
# (gopool.go) is compiled in alongside the Node-subprocess renderer.
# The RENDERER_BACKEND env var selects which one runs at process start;
# default remains node-pool, so existing deployments are unchanged.
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
COPY --from=mln-ffi-build /vendor-maplibre-go /vendor-maplibre-go
WORKDIR /src
COPY --from=git-info /git-commit.txt /git-commit.txt
COPY rampardos/go.mod rampardos/go.sum ./
RUN go mod download
COPY rampardos/ ./
RUN GIT_COMMIT=$(cat /git-commit.txt) && \
    PKG_CONFIG_PATH=/ffi/install/share/pkgconfig \
    CGO_LDFLAGS="-Wl,-rpath,/opt/rampardos/lib" \
    CGO_ENABLED=1 \
    go build -trimpath -tags 'nodynamic mln_ffi' \
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
    libegl-mesa0 libgl1-mesa-dri \
    # maplibre-native common deps
    libcurl4 libjpeg8 libwebp7 libpng16-16 libicu74 \
    libuv1 \
    # Xvfb for headless GL rendering (Node binding needs an X display)
    xvfb \
    # SQLite for better-sqlite3
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
# libmaplibre-native-c.so resolves libpng / libjpeg / libicu from
# /usr/lib/<triple>/ as Ubuntu intended. Earlier revisions registered
# /opt/rampardos/lib with ldconfig globally, which shadowed Ubuntu's libpng
# for the Node renderer and aborted on RENDERER_BACKEND=node-pool.
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

# Render worker: runtime-essential npm packages.
# better-sqlite3 needs the `bindings` package at runtime to locate its
# native .node addon. file-uri-to-path is a transitive dep of bindings.
COPY --from=render-deps \
     /build/node_modules/@maplibre/maplibre-gl-native \
     /app/render-worker/node_modules/@maplibre/maplibre-gl-native
COPY --from=render-deps \
     /build/node_modules/better-sqlite3 \
     /app/render-worker/node_modules/better-sqlite3
COPY --from=render-deps \
     /build/node_modules/bindings \
     /app/render-worker/node_modules/bindings
COPY --from=render-deps \
     /build/node_modules/file-uri-to-path \
     /app/render-worker/node_modules/file-uri-to-path

# Worker script
COPY rampardos-render-worker/render-worker.js /app/render-worker/render-worker.js
COPY rampardos-render-worker/package.json /app/render-worker/package.json

# Create directories
RUN mkdir -p Cache/Tile Cache/Static Cache/StaticMulti Cache/Marker Cache/Regeneratable \
    TileServer/Fonts TileServer/Styles TileServer/Datasets Templates Markers

# Force EGL backend for headless rendering (no X11 display in Docker).
# Without this, maplibre-native tries GLX and loops on "Failed to open X display".
ENV DISPLAY=:0
ENV LIBGL_ALWAYS_SOFTWARE=1
ENV MESA_GL_VERSION_OVERRIDE=3.3
ENV RENDERER_WORKER_SCRIPT=/app/render-worker/render-worker.js
# EGL surfaceless platform — selects Mesa's headless EGL implementation
# so the Go renderer's EGL context creation (egl_linux.go) works without
# an X display. Node renderer continues to use Xvfb (started below).
ENV EGL_PLATFORM=surfaceless
EXPOSE 9000

# Start Xvfb (virtual framebuffer) then rampardos. maplibre-native
# requires a GL context; Xvfb provides one without a physical display.
COPY <<'EOF' /app/entrypoint.sh
#!/bin/sh
rm -f /tmp/.X0-lock /tmp/.X11-unix/X0
Xvfb :0 -screen 0 1024x768x24 -nolisten tcp >/dev/null 2>&1 &
sleep 0.5
exec /app/rampardos "$@"
EOF
RUN chmod +x /app/entrypoint.sh
ENTRYPOINT ["/app/entrypoint.sh"]
