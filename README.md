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

## Requirements

- Go 1.25+
- ~~ImageMagick (for image processing)~~
- tippecanoe (for mbtiles operations (admin side))
- fontnik/build-glyphs (for font processing (admin side))

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
