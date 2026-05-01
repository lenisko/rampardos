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
ARG MLN_FFI_REV=f1d00086e0da85617edc1ce5281b4c5f4e5938e1
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
    ca-certificates curl git xz-utils \
 && rm -rf /var/lib/apt/lists/*
RUN curl -fsSL https://mise.run | sh
ENV PATH=/root/.local/bin:$PATH
WORKDIR /ffi
RUN git clone https://github.com/sargunv/maplibre-native-ffi . \
 && git checkout ${MLN_FFI_REV}
RUN mise trust --yes && mise install
SHELL ["/bin/bash", "-c"]
RUN eval "$(mise activate bash)" && mise run build
RUN test -f build/libmaplibre-native-c.so \
 && test -f build/pkgconfig/maplibre-native-c.pc

# The FFI's CMake links the .so against the pixi conda env's libs
# (libicu 78, libuv 1, libjpeg 8, libwebp 7, libpng16) — versions that
# don't match Debian Trixie / Ubuntu 24.04 system packages. Without
# this bundling step, a downstream `ld` against libmaplibre-native-c.so
# fails to resolve DT_NEEDED entries (libicuuc.so.78, etc.) and the
# Go build errors out with "undefined reference" linker errors.
#
# Strategy: ldd recursively expands the dependency closure, awk filters
# to entries living under /ffi/.pixi/, cp -L dereferences any symlinks
# so the destination filename matches the DT_NEEDED soname directly.
# Bundled libs land alongside libmaplibre-native-c.so in /ffi/build,
# so both -L${libdir} from pkg-config and the dynamic linker's view of
# the .so's transitive deps resolve in one place.
RUN for lib in $(LD_LIBRARY_PATH=/ffi/.pixi/envs/default/lib ldd build/libmaplibre-native-c.so | awk '/=> .*pixi/ {print $3}'); do \
        cp -L "$lib" build/; \
    done \
 && ls -la build/*.so*

# RPATH the bundled .so to find its transitive deps via $ORIGIN (its
# own directory) at load time, regardless of where the runtime image
# places the bundle. Without this, the runtime image had to register
# /opt/rampardos/lib with ldconfig, which shadowed Ubuntu's system
# libraries (libpng/libjpeg/libuv/libicu) for ALL processes — so the
# Node binding's mbgl.node crashed on libpng version mismatch when
# RENDERER_BACKEND=node-pool was selected. RPATH-on-the-.so keeps the
# bundle invisible to anything that doesn't load libmaplibre-native-c.so.
RUN apt-get update \
 && apt-get install -y --no-install-recommends patchelf \
 && rm -rf /var/lib/apt/lists/* \
 && for lib in build/*.so*; do \
        patchelf --set-rpath '$ORIGIN' "$lib" || true; \
    done \
 && readelf -d build/libmaplibre-native-c.so | grep -E 'RUNPATH|RPATH'

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
# libvulkan-dev provides vulkan.pc — required because the binding's
# texture_vulkan_linux.go declares `#cgo linux pkg-config: vulkan`.
# Compile-time dep only; the runtime libvulkan1 / mesa-vulkan-drivers
# are installed on the runtime image.
RUN apt-get update \
 && apt-get install -y --no-install-recommends pkg-config libvulkan-dev \
 && rm -rf /var/lib/apt/lists/*
COPY --from=mln-ffi-build /ffi/build /ffi/build
COPY --from=mln-ffi-build /ffi/include /ffi/include
WORKDIR /src
COPY --from=git-info /git-commit.txt /git-commit.txt
COPY rampardos/go.mod rampardos/go.sum ./
RUN go mod download
COPY rampardos/ ./
RUN GIT_COMMIT=$(cat /git-commit.txt) && \
    PKG_CONFIG_PATH=/ffi/build/pkgconfig \
    CGO_LDFLAGS="-Wl,-rpath-link=/ffi/build -Wl,-rpath,/opt/rampardos/lib" \
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
    # maplibre-native (Go binding via FFI) runtime deps — Vulkan with the
    # lavapipe software rasterizer for fully headless render
    libvulkan1 mesa-vulkan-drivers \
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

# maplibre-native-ffi shared library + its pixi-conda-env transitive deps.
# Bundle is reached only via the chain
#     rampardos (DT_RPATH=/opt/rampardos/lib)
#       → libmaplibre-native-c.so (DT_RPATH=$ORIGIN, set in mln-ffi-build)
#         → its bundled deps (also DT_RPATH=$ORIGIN)
# so the Node binding's mbgl.node and any other process that doesn't
# load libmaplibre-native-c.so resolves libpng / libjpeg / libuv / libicu
# from /usr/lib/x86_64-linux-gnu/ as Ubuntu intended. Earlier revisions
# of this Dockerfile registered /opt/rampardos/lib with ldconfig globally,
# which shadowed Ubuntu's libpng for the Node renderer and caused a
# version-mismatch abort on RENDERER_BACKEND=node-pool selection.
COPY --from=mln-ffi-build /ffi/build /tmp/ffi-build
RUN mkdir -p /opt/rampardos/lib \
 && cp -L /tmp/ffi-build/*.so* /opt/rampardos/lib/ \
 && rm -rf /tmp/ffi-build

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
