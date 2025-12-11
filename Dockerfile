# ================================
# Build Go binary
# ================================
FROM golang:1.25 AS go-build
WORKDIR /build

COPY rampardos/go.mod rampardos/go.sum ./
RUN go mod download

COPY rampardos/ .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o rampardos ./cmd/server

# ================================
# Build tippecanoe
# ================================
FROM debian:bookworm-slim AS tippecanoe-build

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential libsqlite3-dev zlib1g-dev git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN git clone --depth 1 -b 1.36.0 https://github.com/mapbox/tippecanoe.git \
    && cd tippecanoe \
    && make -j$(nproc) \
    && make install \
    && mkdir -p /tippecanoe-out \
    && cp /usr/local/bin/tippecanoe* /tippecanoe-out/

# ================================
# Build fontnik
# ================================
FROM debian:bookworm-slim AS fontnik-build

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential python3 git ca-certificates curl zlib1g-dev \
    && rm -rf /var/lib/apt/lists/*

# Install Node.js 20
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

# ================================
# Final image
# ================================
FROM debian:bookworm-slim
WORKDIR /app

# Install runtime dependencies + Node.js in single layer, then cleanup
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl libsqlite3-0 \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && apt-get purge -y curl \
    && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*

# Copy tippecanoe binaries
COPY --from=tippecanoe-build /tippecanoe-out/ /usr/local/bin/

# Copy fontnik (only essential files)
COPY --from=fontnik-build /fontnik/node_modules /app/fontnik/node_modules
COPY --from=fontnik-build /fontnik/bin /app/fontnik/bin
ENV PATH="/app/fontnik/node_modules/.bin:$PATH"

# Copy Go binary
COPY --from=go-build /build/rampardos /app/rampardos

# Create directories
RUN mkdir -p Cache/Tile Cache/Static Cache/StaticMulti Cache/Marker Cache/Regeneratable \
    TileServer/Fonts TileServer/Styles TileServer/Datasets Templates Markers Temp

EXPOSE 9000

ENTRYPOINT ["/app/rampardos"]
