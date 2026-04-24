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
RUN apk add --no-cache build-base sqlite-dev zlib-dev git bash
RUN git clone --depth 1 -b 1.36.0 https://github.com/mapbox/tippecanoe.git \
    && cd tippecanoe \
    && make -j$(nproc) \
    && make install \
    && mkdir -p /tippecanoe-out \
    && cp /usr/local/bin/tippecanoe* /tippecanoe-out/

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
    && CXXFLAGS="-Wno-error=maybe-uninitialized" npm install --build-from-source; \
    else \
    mkdir -p /fontnik && cd /fontnik && npm install fontnik@0.7.4; \
    fi
RUN find /fontnik/node_modules -type f \( -name "*.md" -o -name "*.ts" -o -name "*.map" -o -name "LICENSE*" -o -name "README*" -o -name "CHANGELOG*" \) -delete \
    && find /fontnik/node_modules -type d \( -name "test" -o -name "tests" -o -name "docs" -o -name "example" -o -name "examples" \) -exec rm -rf {} + 2>/dev/null || true

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
# Build the Rust render worker (alternative to the Node one)
# ================================
# Ubuntu 24.04 matches the runtime base, so the release binary links
# against the same glibc/libcurl/libz/libvulkan the runtime ships.
# maplibre_native is a git dep on the FileSource-callback fork; the
# build downloads the prebuilt linux-$arch amalgam via MLN_PRECOMPILE=1
# (no local clone / source build required).
FROM --platform=$TARGETPLATFORM ubuntu:24.04 AS rust-render-deps
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
    ca-certificates curl git \
    build-essential cmake pkg-config \
    # Link-time libs: the maplibre-native amalgam unconditionally links
    # curl + zlib. libssl-dev is pulled by the build-time `downloader`
    # crate.
    libcurl4-openssl-dev libssl-dev zlib1g-dev \
 && curl -fsSL https://sh.rustup.rs | sh -s -- -y --default-toolchain 1.94.0 --profile minimal \
 && rm -rf /var/lib/apt/lists/*
ENV PATH="/root/.cargo/bin:${PATH}" \
    MLN_PRECOMPILE=1
WORKDIR /build
COPY rampardos-render-worker-rs/Cargo.toml rampardos-render-worker-rs/Cargo.lock ./
COPY rampardos-render-worker-rs/src ./src
RUN cargo build --release --bin rampardos-render-worker-rs

# ================================
# Build Go binary
# ================================
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS rampardos-build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY --from=git-info /git-commit.txt /git-commit.txt
COPY rampardos/go.mod rampardos/go.sum ./
RUN go mod download
COPY rampardos/ ./
RUN GIT_COMMIT=$(cat /git-commit.txt) && \
    GOOS=$TARGETOS GOARCH=$TARGETARCH CGO_ENABLED=0 \
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
    # maplibre-native runtime deps for the NODE worker (Mesa/OpenGL
    # headless rendering via Xvfb).
    libglx0 libgl1 libegl1 libgbm1 libopengl0 \
    libcurl4 libjpeg8 libwebp7 libpng16-16 libicu74 \
    libuv1 \
    xvfb \
    libsqlite3-0 \
    # Runtime deps for the RUST worker: Mesa Vulkan software ICD
    # (lavapipe). Adds ~30 MB; lets the Rust worker run fully
    # headless without Xvfb. The Node worker continues to use GL + Xvfb.
    mesa-vulkan-drivers libvulkan1 \
 && apt-get purge -y curl \
 && apt-get autoremove -y \
 && rm -rf /var/lib/apt/lists/*

# Tippecanoe binaries (tile-join for mbtiles operations)
COPY --from=tippecanoe-build /tippecanoe-out/ /usr/local/bin/

# Fontnik (build-glyphs for font processing)
COPY --from=fontnik-build /fontnik /app/fontnik
RUN ln -s /app/fontnik/node_modules/.bin/build-glyphs /usr/local/bin/build-glyphs
ENV PATH="/app/fontnik/node_modules/.bin:$PATH"

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

# Rust render worker (alternative to the Node one). The Go orchestrator
# picks the binary via RENDERER_NODE_BINARY + RENDERER_WORKER_SCRIPT; both
# workers ship in the image so operators can flip between them without a
# rebuild. See entrypoint.sh / docs below.
COPY --from=rust-render-deps \
     /build/target/release/rampardos-render-worker-rs \
     /app/render-worker-rs/rampardos-render-worker-rs

# Create directories
RUN mkdir -p Cache/Tile Cache/Static Cache/StaticMulti Cache/Marker Cache/Regeneratable \
    TileServer/Fonts TileServer/Styles TileServer/Datasets Templates Markers

# Defaults: NODE worker. To flip to the Rust worker at runtime, set:
#   RENDERER_NODE_BINARY=/app/render-worker-rs/rampardos-render-worker-rs
#   RENDERER_WORKER_SCRIPT=-   (any non-empty string; the Rust binary
#                               accepts and ignores this positional)
#
# The Node worker path (below) stays the default so existing deployments
# are unaffected. The Rust worker is headless via Mesa Vulkan (lavapipe) —
# it does not use Xvfb/DISPLAY, so flipping workers costs nothing.
ENV DISPLAY=:0
ENV LIBGL_ALWAYS_SOFTWARE=1
ENV MESA_GL_VERSION_OVERRIDE=3.3
ENV RENDERER_WORKER_SCRIPT=/app/render-worker/render-worker.js
# EGL surfaceless for the Rust worker — harmless to the Node worker,
# which ignores it.
ENV EGL_PLATFORM=surfaceless
EXPOSE 9000

# Start Xvfb (virtual framebuffer) then rampardos. The Node worker needs
# it; the Rust worker ignores it (Mesa Vulkan is surfaceless).
COPY <<'EOF' /app/entrypoint.sh
#!/bin/sh
rm -f /tmp/.X0-lock /tmp/.X11-unix/X0
Xvfb :0 -screen 0 1024x768x24 -nolisten tcp >/dev/null 2>&1 &
sleep 0.5
exec /app/rampardos "$@"
EOF
RUN chmod +x /app/entrypoint.sh
ENTRYPOINT ["/app/entrypoint.sh"]
