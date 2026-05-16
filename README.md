# Rampardos

In-process vector tile renderer, static map generator, and caching
server. Originally a Go port of swifttileservercache; now ships its
own maplibre-native renderer (no external tileserver-gl required).

## Features

- Vector tile rendering from local mbtiles, in-process via a Node
  worker pool (maplibre-native under the hood)
- Static map generation with markers, polygons, and circles
- Multi-static maps (grids of composed static maps)
- Template system with variable substitution
- Admin dashboard for managing datasets, styles, fonts, and templates
- ETag-based revalidation for static map responses
- Prometheus metrics + optional Pyroscope profiling
- External tile-source support (Google Satellite, custom XYZ URLs)

## Quick start (non-Docker host setup)

This is the recommended way to bootstrap a new install. Run on the
host machine in the directory you intend to keep the data — typically
the same directory as your `docker-compose.yml`:

```bash
curl -fsSL https://raw.githubusercontent.com/lenisko/rampardos/master/setup/setup.sh | bash
```

What it does:

- Creates `TileServer/{Styles,Fonts,Datasets/List}/`
- Installs the bundled `klokantech-basic` style
- Downloads OpenMapTiles v2.0 fonts (~80 MB) and extracts them

The script is idempotent: re-running it leaves existing styles, fonts,
and datasets in place. Requires only `curl` and `unzip` on the host.

After it finishes:

1. Save the docker-compose snippet below as `docker-compose.yml`
2. Write a `.env` with `ADMIN_USERNAME` / `ADMIN_PASSWORD`
3. `docker compose up -d`
4. Open `http://localhost:9000/admin/datasets` and upload your mbtiles
   file (or paste a download URL — MapTiler's signed download links
   work). Activate the dataset from the same page.

The setup script deliberately does not handle the mbtiles step itself:
MapTiler URLs are short-lived and single-use, and the admin UI's
WebSocket downloader handles progress reporting + activation more
robustly than `curl` in a shell script.

### Overrides

```
TARGET_DIR=/some/path  RAMPARDOS_REF=master  curl ... | bash
```

`TARGET_DIR` defaults to `$PWD`. `RAMPARDOS_REF` defaults to `master`
and selects which branch/tag the bundled style ZIP is fetched from.

## Docker

```yaml
services:
  rampardos:
    image: ghcr.io/lenisko/rampardos:latest
    container_name: rampardos
    restart: unless-stopped
    user: "${PUID:-0}:${PGID:-0}"
    volumes:
      - ./Cache:/app/Cache
      - ./Templates:/app/Templates
      - ./TileServer:/app/TileServer
      - ./Markers:/app/Markers
    environment:
      ADMIN_USERNAME: ${ADMIN_USERNAME:-}
      ADMIN_PASSWORD: ${ADMIN_PASSWORD:-}
      PREVIEW_LATITUDE: ${PREVIEW_LATITUDE:-51.5165753}
      PREVIEW_LONGITUDE: ${PREVIEW_LONGITUDE:--0.1543177}
      TILE_CACHE_MAX_AGE_MINUTES: ${TILE_CACHE_MAX_AGE_MINUTES:-10080}
      TILE_CACHE_DELAY_SECONDS: ${TILE_CACHE_DELAY_SECONDS:-3600}
      STATIC_CACHE_MAX_AGE_MINUTES: ${STATIC_CACHE_MAX_AGE_MINUTES:-10080}
      STATIC_CACHE_DELAY_SECONDS: ${STATIC_CACHE_DELAY_SECONDS:-3600}
      STATIC_MULTI_CACHE_MAX_AGE_MINUTES: ${STATIC_MULTI_CACHE_MAX_AGE_MINUTES:-60}
      STATIC_MULTI_CACHE_DELAY_SECONDS: ${STATIC_MULTI_CACHE_DELAY_SECONDS:-900}
      MARKER_CACHE_MAX_AGE_MINUTES: ${MARKER_CACHE_MAX_AGE_MINUTES:-1440}
      MARKER_CACHE_DELAY_SECONDS: ${MARKER_CACHE_DELAY_SECONDS:-3600}
      # Optional: Pyroscope profiling
      PYROSCOPE_SERVER_ADDRESS: ${PYROSCOPE_SERVER_ADDRESS:-}
      PYROSCOPE_APPLICATION_NAME: ${PYROSCOPE_APPLICATION_NAME:-rampardos}
      PYROSCOPE_API_KEY: ${PYROSCOPE_API_KEY:-}
    ports:
      - ${PORT:-9000}:9000
```

```env
$ cat .env
PREVIEW_LATITUDE=50.50
PREVIEW_LONGITUDE=17.17
ADMIN_USERNAME=admin
ADMIN_PASSWORD=changeme
PUID=1000
PGID=1000
```

Convert legacy Leaf templates to Jet via the admin UI helper at
`/admin/convert`. The bundled volume layout works out of the box on a
Swifttileserver Cache/Data tree.

## Configuration

Most operators only need to set admin credentials and the preview
centre. Full reference: `rampardos/internal/config/config.go`.

| Variable | Description | Default |
| --- | --- | --- |
| `PORT` | HTTP server port | `9000` |
| `HOSTNAME` | HTTP bind hostname | `0.0.0.0` |
| `REQUEST_TIMEOUT` | Per-request deadline (Go duration: `10s`, `2m`, …) | `10s` |
| `ADMIN_USERNAME` | Admin basic-auth username (empty disables dashboard) | — |
| `ADMIN_PASSWORD` | Admin basic-auth password | — |
| `PREVIEW_LATITUDE` | Admin /styles preview centre | `52.5200` |
| `PREVIEW_LONGITUDE` | Admin /styles preview centre | `13.4050` |
| `DEFAULT_IMAGE_FORMAT` | `png`, `jpeg`, or `webp` | `png` |
| `OVERRIDE_CLIENT_FORMAT` | Ignore client `format` param, force `DEFAULT_IMAGE_FORMAT` | `false` |
| `PNG_COMPRESSION_LEVEL` | `fast`, `default`, `best`, `none` | `fast` |
| `IMAGE_QUALITY` | JPEG/WebP quality 1–100 | `90` |
| `TILE_CACHE_MAX_AGE_MINUTES` | TTL for external tile disk cache | `10080` (7 days) |
| `STATIC_CACHE_MAX_AGE_MINUTES` | TTL for static map cache | `10080` |
| `STATIC_MULTI_CACHE_MAX_AGE_MINUTES` | TTL for multi-static-map cache | `60` |
| `MARKER_CACHE_MAX_AGE_MINUTES` | TTL for marker image cache | `1440` (1 day) |
| `MARKER_IMAGE_CACHE_SIZE` | Resized marker images cached in RAM | `2000` |
| `TILE_IMAGE_CACHE_SIZE` | Decoded tile images cached in RAM | `500` |
| `COMPOSITE_IMAGE_CACHE_SIZE` | Composited base+staticmap images in RAM | `200` |
| `TILE_URL_<name>` | External XYZ tile URL template (e.g. `TILE_URL_MY_TILES=https://...`) | — |
| `RENDERER_POOL_SIZE` | Renderer worker count per style (0 = NumCPU) | `0` |
| `STYLE_POOL_SIZE` | Max styles kept live at once (0 = unbounded) | `0` |
| `EXPERIMENTAL_G_SAT` | Register a built-in Google Satellite external style | `true` |
| `PPROF_ENABLED` | Mount `/debug/pprof` for live profiling | `false` |

Each cache class also accepts a matching `*_DELAY_SECONDS` (cleaner
sweep interval) and `*_DROP_AFTER_MINUTES` (force-evict regardless of
mtime) variable; see the config source for the full set.

## Building from source

For development. Production users should pull the docker image.

```bash
cd rampardos
go mod tidy
go build -o rampardos ./cmd/server
```

Runtime dependencies (bundled inside the docker image):

- Node + the render worker (`render-worker/render-worker.js`) and its
  maplibre-native FFI shared libraries
- `tippecanoe`'s `tile-join` for combining mbtiles
- `build-glyphs` (from `fontnik`) — only needed if uploading fonts via
  the admin UI

## API endpoints

### Public

| Method | Path | |
| --- | --- | --- |
| GET | `/styles` | List available styles |
| GET | `/tile/{style}/{z}/{x}/{y}/{scale}/{format}` | Get a tile |
| GET / POST | `/staticmap` | Generate a static map (query params or JSON body) |
| GET / POST | `/staticmap/{template}` | Render a static map from a saved template |
| GET | `/staticmap/pregenerated/{id}` | Serve a previously-pregenerated static map |
| GET / POST | `/multistaticmap` | Generate a multi-static map |
| GET / POST | `/multistaticmap/{template}` | Render a multi-static-map from a template |
| GET | `/multistaticmap/pregenerated/{id}` | Serve a pregenerated multi-static map |
| GET | `/metrics` | Prometheus metrics |

### Admin (basic auth)

Gated by `ADMIN_USERNAME` / `ADMIN_PASSWORD`. UI under `/admin/...`,
JSON / WebSocket API under `/admin/api/...`. Full route table:
`rampardos/cmd/server/main.go`.

- `/admin/stats` — cache and request stats
- `/admin/datasets` — upload, activate, combine, reload mbtiles
- `/admin/styles` — install local style ZIPs, register external XYZ URLs
- `/admin/fonts` — upload TTF/OTF fonts (processed via `build-glyphs`)
- `/admin/templates` — manage saved request templates
- `/admin/convert` — Leaf-to-Jet template syntax converter

## Static map parameters

| Parameter | Type | Description |
| --- | --- | --- |
| `style` | string | Style ID (must match an installed local style or registered external) |
| `latitude` | float | Centre latitude |
| `longitude` | float | Centre longitude |
| `zoom` | float | Zoom level 1–22 (fractional allowed for local styles) |
| `width` | int | Width in pixels, max 4096 |
| `height` | int | Height in pixels, max 4096 |
| `scale` | int | Scale factor 1–4 |
| `format` | string | `png`, `jpeg`, or `webp` |
| `markers` | JSON | Array of markers |
| `polygons` | JSON | Array of polygons |
| `circles` | JSON | Array of circles |

## License

MIT — see `LICENSE`.
