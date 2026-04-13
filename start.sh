#!/usr/bin/env bash
#
# Start rampardos locally with sensible defaults.
# All settings can be overridden via environment variables or flags.
#
# Usage:
#   ./start.sh                          # defaults
#   ./start.sh --port 8080              # custom port
#   ./start.sh --pool-size 4            # custom worker pool size
#   PORT=8080 ./start.sh                # env var override
#
# Environment variables (take precedence over flags):
#   PORT, HOSTNAME, RENDERER_POOL_SIZE, RENDERER_TIMEOUT_SECONDS,
#   RENDERER_WORKER_LIFETIME, RENDERER_WORKER_SCRIPT,
#   ADMIN_USERNAME, ADMIN_PASSWORD
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
BINARY="$SCRIPT_DIR/bin/rampardos"
WORKER_SCRIPT="$SCRIPT_DIR/rampardos-render-worker/render-worker.js"

# Defaults (overridden by env vars, then by flags)
: "${PORT:=9000}"
: "${HOSTNAME:=0.0.0.0}"
: "${RENDERER_POOL_SIZE:=2}"
: "${RENDERER_TIMEOUT_SECONDS:=15}"
: "${RENDERER_WORKER_LIFETIME:=500}"
: "${RENDERER_WORKER_SCRIPT:=$WORKER_SCRIPT}"
: "${ADMIN_USERNAME:=}"
: "${ADMIN_PASSWORD:=}"

# Parse command-line flags (override env vars)
while [[ $# -gt 0 ]]; do
    case "$1" in
        --port)             PORT="$2"; shift 2 ;;
        --host)             HOSTNAME="$2"; shift 2 ;;
        --pool-size)        RENDERER_POOL_SIZE="$2"; shift 2 ;;
        --timeout)          RENDERER_TIMEOUT_SECONDS="$2"; shift 2 ;;
        --worker-lifetime)  RENDERER_WORKER_LIFETIME="$2"; shift 2 ;;
        --worker-script)    RENDERER_WORKER_SCRIPT="$2"; shift 2 ;;
        --admin-user)       ADMIN_USERNAME="$2"; shift 2 ;;
        --admin-pass)       ADMIN_PASSWORD="$2"; shift 2 ;;
        --help|-h)
            echo "Usage: $0 [options]"
            echo ""
            echo "Options:"
            echo "  --port N              HTTP port (default: 9000)"
            echo "  --host ADDR           Bind address (default: 0.0.0.0)"
            echo "  --pool-size N         Renderer workers per style (default: 2)"
            echo "  --timeout N           Render timeout in seconds (default: 15)"
            echo "  --worker-lifetime N   Renders per worker before recycle (default: 500)"
            echo "  --worker-script PATH  Path to render-worker.js"
            echo "  --admin-user USER     Admin UI username"
            echo "  --admin-pass PASS     Admin UI password"
            echo ""
            echo "All options can also be set via environment variables."
            echo "See README.md for the full list."
            exit 0
            ;;
        *)
            echo "Unknown option: $1 (try --help)"
            exit 1
            ;;
    esac
done

# Check prerequisites
if [ ! -f "$BINARY" ]; then
    echo "Binary not found at $BINARY — run 'make build' first."
    exit 1
fi

if [ ! -f "$RENDERER_WORKER_SCRIPT" ]; then
    echo "Worker script not found at $RENDERER_WORKER_SCRIPT"
    echo "Run 'make npm-install' or check --worker-script path."
    exit 1
fi

if [ ! -d "$SCRIPT_DIR/rampardos-render-worker/node_modules/@maplibre/maplibre-gl-native" ]; then
    echo "Node dependencies not installed — running npm install..."
    (cd "$SCRIPT_DIR/rampardos-render-worker" && npm install)
fi

# Ensure runtime directories exist
mkdir -p Cache/Tile Cache/Static Cache/StaticMulti Cache/Marker Cache/Regeneratable
mkdir -p TileServer/Fonts TileServer/Styles TileServer/Datasets
mkdir -p Templates Markers Temp

# Export all settings
export PORT HOSTNAME
export RENDERER_POOL_SIZE RENDERER_TIMEOUT_SECONDS RENDERER_WORKER_LIFETIME
export RENDERER_WORKER_SCRIPT
export ADMIN_USERNAME ADMIN_PASSWORD

echo "Starting rampardos on $HOSTNAME:$PORT (pool_size=$RENDERER_POOL_SIZE)"
exec "$BINARY"
