# ================================
# Build image
# ================================
FROM golang:1.25 AS build
WORKDIR /build

# Copy go mod files
COPY rampardos/go.mod rampardos/go.sum ./
RUN go mod download

# Copy source code
COPY rampardos/ .

# Build
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o rampardos ./cmd/server

# ================================
# Run image
# ================================
FROM debian:bookworm-slim
WORKDIR /app

# Install runtime dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    libsqlite3-dev \
    zlib1g-dev \
    git \
    curl \
    nodejs \
    npm \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Install tippecanoe
RUN git clone https://github.com/mapbox/tippecanoe.git -b 1.36.0 \
    && cd tippecanoe \
    && make -j \
    && make install \
    && cd .. \
    && rm -rf tippecanoe

# Install fontnik (arm64 needs custom build with relaxed compiler warnings)
RUN if [ "$(dpkg --print-architecture)" = "arm64" ]; then \
    git clone -b fix-build-errors-node14 https://github.com/lenisko/node-fontnik.git ./fontnik \
    && cd fontnik \
    && mkdir .toolchain \
    && CXXFLAGS="-Wno-error=maybe-uninitialized" npm install --build-from-source \
    && npm link; \
    else \
    npm install -g fontnik@0.7.4; \
    fi

# Copy binary
COPY --from=build /build/rampardos /app/rampardos

# Copy HTML templates for admin UI
COPY --from=build /build/internal/templates /app/internal/templates

# Create cache directories (Templates is for user Jet templates, mounted or created at runtime)
RUN mkdir -p Cache/Tile Cache/Static Cache/StaticMulti Cache/Marker Cache/Regeneratable \
    TileServer/Fonts TileServer/Styles TileServer/Datasets Templates Markers Temp

EXPOSE 9000

ENTRYPOINT ["/app/rampardos"]
