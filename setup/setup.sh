#!/usr/bin/env bash
#
# Bootstrap host-side config directories for rampardos.
#
# Intended for users running rampardos under Docker but who need to
# populate the volume-mounted TileServer/ tree on the host before the
# first start. Installs the klokantech-basic style and the openmaptiles
# v2.0 fonts; creates an empty Datasets/List/ for the admin UI to fill.
# Idempotent — re-running is safe; existing styles and fonts are left
# in place.
#
# Usage (recommended):
#   curl -fsSL https://raw.githubusercontent.com/lenisko/rampardos/master/setup/setup.sh | bash
#
# Or download first, inspect, then run:
#   curl -fsSL https://raw.githubusercontent.com/lenisko/rampardos/master/setup/setup.sh -o setup.sh
#   bash setup.sh
#
# Overrides (environment variables):
#   TARGET_DIR        Where to create TileServer/ (default: $PWD)
#   RAMPARDOS_REPO    GitHub repo path for raw downloads (default: lenisko/rampardos)
#   RAMPARDOS_REF     Branch or tag for raw downloads (default: master)
#
set -euo pipefail

TARGET_DIR="${TARGET_DIR:-$PWD}"
RAMPARDOS_REPO="${RAMPARDOS_REPO:-lenisko/rampardos}"
RAMPARDOS_REF="${RAMPARDOS_REF:-master}"

STYLES_DIR="$TARGET_DIR/TileServer/Styles"
FONTS_DIR="$TARGET_DIR/TileServer/Fonts"
DATASETS_DIR="$TARGET_DIR/TileServer/Datasets"
DATASETS_LIST_DIR="$DATASETS_DIR/List"

STYLE_NAME="klokantech-basic"
STYLE_ZIP_URL="https://raw.githubusercontent.com/$RAMPARDOS_REPO/$RAMPARDOS_REF/setup/styles/$STYLE_NAME.zip"
FONTS_ZIP_URL="https://github.com/openmaptiles/fonts/releases/download/v2.0/v2.0.zip"

# --- helpers ----------------------------------------------------------------

log() { printf '==> %s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

require_tool() {
	command -v "$1" >/dev/null 2>&1 || die "missing dependency: $1 (please install and re-run)"
}

# --- preflight --------------------------------------------------------------

require_tool curl
require_tool unzip

log "Target: $TARGET_DIR"
mkdir -p "$STYLES_DIR" "$FONTS_DIR" "$DATASETS_LIST_DIR"

# --- style ------------------------------------------------------------------

if [ -d "$STYLES_DIR/$STYLE_NAME" ]; then
	log "Style '$STYLE_NAME' already present, skipping"
else
	log "Installing style '$STYLE_NAME'"
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	curl -fsSL "$STYLE_ZIP_URL" -o "$tmp/style.zip"
	unzip -q "$tmp/style.zip" -d "$STYLES_DIR"
	rm -rf "$tmp"
	trap - EXIT
fi

# --- fonts ------------------------------------------------------------------

if [ -n "$(ls -A "$FONTS_DIR" 2>/dev/null)" ]; then
	log "Fonts directory non-empty, skipping fonts install"
else
	log "Installing openmaptiles fonts v2.0 (~80MB, this may take a minute)"
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	curl -fL --progress-bar "$FONTS_ZIP_URL" -o "$tmp/fonts.zip"
	unzip -q "$tmp/fonts.zip" -d "$FONTS_DIR"
	rm -rf "$tmp"
	trap - EXIT
fi

# --- next steps -------------------------------------------------------------

cat <<EOF

Done. Layout under $TARGET_DIR/TileServer:
  Styles/$STYLE_NAME/
  Fonts/...
  Datasets/List/    (empty — add an mbtiles via the admin UI)

Next:
  1. Set ADMIN_USERNAME / ADMIN_PASSWORD in your .env
  2. Start the container (docker compose up -d, or your preferred runner)
  3. Open http://<host>:9000/admin/datasets to upload or paste an
     mbtiles URL, then activate it
EOF
