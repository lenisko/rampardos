# Rampardos

A Go port of swifttileservercache with some new features - a caching proxy for tile servers with static map generation capabilities.

## Features

- **Tile Caching**: Proxy and cache tiles from any tile server
- **Static Map Generation**: Generate static map images with markers, polygons, and circles
- **Multi-Static Maps**: Combine multiple static maps into grids
- **Template System**: JSON templates with variable substitution
- **Admin Dashboard**: Web UI for managing datasets, styles, fonts, and templates
- **Prometheus Metrics**: Built-in metrics endpoint for monitoring
- **WebSocket Support**: Real-time progress for dataset operations

## Docker Setup

- Convert Leaf templates to Jet using WebUI helper `/admin/convert`
- You might need to adjust volume bindings, should work ootb on Swifttileserver Cache/Data.

```yml
services:
  tileserver:
    image: maptiler/tileserver-gl:latest
    command: -p 8080
    container_name: tileserver
    restart: unless-stopped
    tty: true
    user: "${PUID:-0}:${PGID:-0}"
    volumes:
      - ./TileServer:/data
    healthcheck:
      test: ["CMD", "wget", "--no-verbose", "--tries=1", "--spider", "http://localhost:8080/health"]
      interval: 30s
      timeout: 10s
      retries: 3
      start_period: 30s
    deploy:
      resources:
        limits:
          memory: 3G
        reservations:
          memory: 1G
  rampardos:
    image: ghcr.io/lenisko/rampardos:latest
    container_name: rampardos
    restart: unless-stopped
    tty: true
    user: "${PUID:-0}:${PGID:-0}"
    pid: "service:tileserver" # share PID namespace - required to get mbtiles reload working
    volumes:
      - ./Cache:/app/Cache
      - ./Templates:/app/Templates
      - ./TileServer:/app/TileServer
      - ./Markers:/app/Markers
    environment:
      PORT: ${PORT:-9000}
      HOSTNAME: ${HOSTNAME:-0.0.0.0}
      TILE_SERVER_URL: ${TILE_SERVER_URL:-http://tileserver:8080}
      TILE_CACHE_MAX_AGE_MINUTES: ${TILE_CACHE_MAX_AGE_MINUTES:-10080}
      TILE_CACHE_DELAY_SECONDS: ${TILE_CACHE_DELAY_SECONDS:-3600}
      STATIC_CACHE_MAX_AGE_MINUTES: ${STATIC_CACHE_MAX_AGE_MINUTES:-10080}
      STATIC_CACHE_DELAY_SECONDS: ${STATIC_CACHE_DELAY_SECONDS:-3600}
      STATIC_MULTI_CACHE_MAX_AGE_MINUTES: ${STATIC_MULTI_CACHE_MAX_AGE_MINUTES:-60}
      STATIC_MULTI_CACHE_DELAY_SECONDS: ${STATIC_MULTI_CACHE_DELAY_SECONDS:-900}
      MARKER_CACHE_MAX_AGE_MINUTES: ${MARKER_CACHE_MAX_AGE_MINUTES:-1440}
      MARKER_CACHE_DELAY_SECONDS: ${MARKER_CACHE_DELAY_SECONDS:-3600}
      TEMPLATES_CACHE_DELAY_SECONDS: ${TEMPLATES_CACHE_DELAY_SECONDS:-60}
      PREVIEW_LATITUDE: ${PREVIEW_LATITUDE:-51.5165753}
      PREVIEW_LONGITUDE: ${PREVIEW_LONGITUDE:--0.1543177}
      ADMIN_USERNAME: ${ADMIN_USERNAME:-}
      ADMIN_PASSWORD: ${ADMIN_PASSWORD:-}
      PYROSCOPE_SERVER_ADDRESS: ${PYROSCOPE_SERVER_ADDRESS:-}
      PYROSCOPE_APPLICATION_NAME: ${PYROSCOPE_APPLICATION_NAME:-tileserver-cache}
      PYROSCOPE_API_KEY: ${PYROSCOPE_API_KEY:-}
      PYROSCOPE_BASIC_AUTH_USER: ${PYROSCOPE_BASIC_AUTH_USER:-}
      PYROSCOPE_BASIC_AUTH_PASSWORD: ${PYROSCOPE_BASIC_AUTH_PASSWORD:-}
      PYROSCOPE_MUTEX_PROFILE_FRACTION: ${PYROSCOPE_MUTEX_PROFILE_FRACTION:-5}
      PYROSCOPE_BLOCK_PROFILE_RATE: ${PYROSCOPE_BLOCK_PROFILE_RATE:-5}
      PYROSCOPE_LOGGER: ${PYROSCOPE_LOGGER:-false}
      TILESERVER_MONITOR_ENABLED: ${TILESERVER_MONITOR_ENABLED:-true}
      TILESERVER_MONITOR_INTERVAL_SECONDS: ${TILESERVER_MONITOR_INTERVAL_SECONDS:-15}
      TILESERVER_MONITOR_TIMEOUT_SECONDS: ${TILESERVER_MONITOR_TIMEOUT_SECONDS:-10}
      TILESERVER_MONITOR_THRESHOLD: ${TILESERVER_MONITOR_THRESHOLD:-3}
    ports:
      - ${PORT:-9000}:9000
```

```env
$ cat .env
PREVIEW_LATITUDE=50.50
PREVIEW_LONGITUDE=17.17
ADMIN_USERNAME=REDACTED
ADMIN_PASSWORD=REDACTED
PUID=1000
PGID=1000
```

## Requirements (non-docker)

- Go 1.25+
- tippecanoe (for mbtiles operations (admin utils))
- fontnik/build-glyphs (for font processing (admin utils))

## Environment Variables

| Variable                       | Description                           | Default      |
| ------------------------------ | ------------------------------------- | ------------ |
| `TILE_SERVER_URL`              | URL of the upstream tile server       | **Required** |
| `PORT`                         | Server port                           | `9000`       |
| `HOSTNAME`                     | Server hostname                       | `0.0.0.0`    |
| `TILE_CACHE_MAX_AGE_MINUTES`   | Max age for tile cache                | -            |
| `TILE_CACHE_DELAY_SECONDS`     | Cleanup interval for tile cache       | -            |
| `STATIC_CACHE_MAX_AGE_MINUTES` | Max age for static map cache          | -            |
| `STATIC_CACHE_DELAY_SECONDS`   | Cleanup interval for static map cache | -            |
| `MARKER_CACHE_MAX_AGE_MINUTES` | Max age for marker cache              | -            |
| `MARKER_CACHE_DELAY_SECONDS`   | Cleanup interval for marker cache     | -            |
| `TILE_URL_<name>`              | External tile URL template            | -            |

## Building

```bash
cd go
go mod tidy
go build -o rampardos ./cmd/server
```

## Running

```bash
export TILE_SERVER_URL=http://localhost:8080 ./rampardos
```

## Docker

```bash
docker build -t rampardos .
docker run -p 9000:9000 -e TILE_SERVER_URL=http://tileserver:8080 rampardos
```

Or with docker-compose:

```bash
docker-compose up -d
```

## API Endpoints

### Public

- `GET /styles` - List available styles
- `GET /tile/{style}/{z}/{x}/{y}/{scale}/{format}` - Get a tile
- `GET /staticmap` - Generate a static map (query params)
- `POST /staticmap` - Generate a static map (JSON body)
- `GET /staticmap/{template}` - Generate from template
- `GET /staticmap/pregenerated/{id}` - Get pregenerated map
- `GET /multistaticmap` - Generate multi-static map
- `GET /metrics` - Prometheus metrics

### Admin

- `GET /admin/stats` - Cache statistics
- `GET /admin/datasets` - Manage datasets
- `GET /admin/styles` - Manage styles
- `GET /admin/fonts` - Manage fonts
- `GET /admin/templates` - Manage templates

## Static Map Parameters

| Parameter   | Type   | Description                    |
| ----------- | ------ | ------------------------------ |
| `style`     | string | Map style ID                   |
| `latitude`  | float  | Center latitude                |
| `longitude` | float  | Center longitude               |
| `zoom`      | float  | Zoom level                     |
| `width`     | int    | Image width in pixels          |
| `height`    | int    | Image height in pixels         |
| `scale`     | int    | Scale factor (1-4)             |
| `format`    | string | Output format (png, jpg, webp) |
| `markers`   | JSON   | Array of markers               |
| `polygons`  | JSON   | Array of polygons              |
| `circles`   | JSON   | Array of circles               |

## License

MIT License - see LICENSE file
