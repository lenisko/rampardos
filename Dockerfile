# ================================
# Get Git commit SHA
# ================================
FROM alpine:3.20 AS git-info
RUN apk add --no-cache git
WORKDIR /repo
COPY .git/ .git/
RUN git rev-parse HEAD > /git-commit.txt

# ================================
# Build Go binary
# ================================
FROM golang:1.25-alpine AS go-build
WORKDIR /build

COPY --from=git-info /git-commit.txt /git-commit.txt

COPY rampardos/ .
RUN go mod download
RUN GIT_COMMIT=$(cat /git-commit.txt) && \
    CGO_ENABLED=0 GOOS=linux go build -tags nodynamic -ldflags="-w -s -X github.com/lenisko/rampardos/internal/version.gitCommitFromLdflags=${GIT_COMMIT}" -o rampardos ./cmd/server

# ================================
# Build tippecanoe
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
# Build fontnik (needs Debian for bash scripts)
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

# Strip node_modules
RUN find /fontnik/node_modules -type f \( -name "*.md" -o -name "*.ts" -o -name "*.map" -o -name "LICENSE*" -o -name "README*" -o -name "CHANGELOG*" \) -delete \
    && find /fontnik/node_modules -type d \( -name "test" -o -name "tests" -o -name "docs" -o -name "example" -o -name "examples" \) -exec rm -rf {} + 2>/dev/null || true

# ================================
# Final image
# ================================
FROM debian:bookworm-slim
WORKDIR /app

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl libsqlite3-0 \
    && curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && apt-get purge -y curl \
    && apt-get autoremove -y \
    && rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*

# Copy tippecanoe binaries
COPY --from=tippecanoe-build /tippecanoe-out/ /usr/local/bin/

# Copy fontnik
COPY --from=fontnik-build /fontnik /app/fontnik
ENV PATH="/app/fontnik/node_modules/.bin:$PATH"

# Copy Go binary
COPY --from=go-build /build/rampardos /app/rampardos

# Create directories
RUN mkdir -p Cache/Tile Cache/Static Cache/StaticMulti Cache/Marker Cache/Regeneratable \
    TileServer/Fonts TileServer/Styles TileServer/Datasets Templates Markers Temp

EXPOSE 9000

ENTRYPOINT ["/app/rampardos"]
