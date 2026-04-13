#!/usr/bin/env node
/*
 * rampardos render worker.
 *
 * One Node process per worker slot. Each worker is pinned to a single
 * style at startup and handles many renders of that style over stdin.
 *
 * Protocol (all frames are length-prefixed; all multi-byte integers
 * are big-endian):
 *
 *   Request  (Go -> worker):
 *     'R' (1 byte)  + uint32 length + JSON payload
 *
 *     JSON payload fields:
 *       {zoom, center: [lng, lat], width, height, bearing, pitch}
 *
 *   Response (worker -> Go):
 *     'K' (1 byte)  + uint32 length + raw RGBA bytes     — success
 *     'E' (1 byte)  + uint32 length + UTF-8 error string — failure
 *
 *   Handshake (once, before any request):
 *     'H' (1 byte)  + uint32 length + JSON {pid, style}
 *
 * CLI args:
 *   --style-id ID            (required) style identifier for logging
 *   --style-path PATH        (required) absolute path to resolved style.json
 *   --mbtiles PATH           (required) absolute path to .mbtiles file
 *   --styles-dir DIR         (required) absolute path to styles root (sprite files)
 *   --fonts-dir DIR          (required) absolute path to fonts root (glyph PBFs)
 *
 * Exits cleanly on stdin EOF. No signal handling — the Go orchestrator
 * owns the process lifecycle and sends SIGKILL on timeout or recycle.
 */

const fs = require("fs");
const zlib = require("zlib");
const Database = require("better-sqlite3");
const mbgl = require("@maplibre/maplibre-gl-native");

const args = require("node:util").parseArgs({
  options: {
    "style-id":   { type: "string" },
    "style-path": { type: "string" },
    "mbtiles":    { type: "string" },
    "styles-dir": { type: "string" },
    "fonts-dir":  { type: "string" },
  },
}).values;

for (const required of ["style-id", "style-path", "mbtiles", "styles-dir", "fonts-dir"]) {
  if (!args[required]) {
    process.stderr.write(`render-worker: missing --${required}\n`);
    process.exit(2);
  }
}

// writeFrame writes a single framed message to stdout. Stdout is
// assumed to be a pipe that the Go orchestrator drains continuously;
// if the orchestrator blocks, Node buffers writes in userland (which
// preserves ordering but can accumulate memory). Task 11's worker pool
// guarantees continuous draining.
function writeFrame(type, payload) {
  const header = Buffer.alloc(5);
  header.writeUInt8(type.charCodeAt(0), 0);
  header.writeUInt32BE(payload.length, 1);
  process.stdout.write(header);
  process.stdout.write(payload);
}

function writeError(msg) {
  writeFrame("E", Buffer.from(msg, "utf8"));
}

// Resolve a sprite URL like file:///abs/path/to/sprite.json back to
// an absolute filesystem path.
function fileURLToPath(url) {
  if (!url.startsWith("file://")) {
    throw new Error(`not a file URL: ${url}`);
  }
  // maplibre-native percent-encodes spaces and special characters in
  // URLs before passing them to the request callback. Decode so the
  // path matches the actual filesystem (e.g. "Noto%20Sans%20Bold"
  // becomes "Noto Sans Bold").
  return decodeURIComponent(url.substring("file://".length));
}

// Module-scope state populated by the startup try/catch below. Helpers
// further down close over these via lexical scope.
let db;
let tileQuery;
let map;
let styleJSON;

// Vector tile reader. mbtiles stores tiles with TMS y-coordinates,
// which is the 2^z - 1 - y flip of XYZ y.
function readTile(z, x, y) {
  const tmsY = (1 << z) - 1 - y;
  const row = tileQuery.get(z, x, tmsY);
  // Missing tile: maplibre-native correctly handles this as an empty
  // tile render. This is expected behavior, especially at zoom levels
  // above the mbtiles' maxzoom (overzoom case) — it should NOT produce
  // a worker error frame.
  if (!row) return null;
  const data = row.tile_data;
  // Vector tiles in mbtiles are typically gzip-compressed. The Node
  // bindings for maplibre-native do NOT decompress automatically —
  // the request callback must return uncompressed protobuf bytes.
  // Detect gzip magic bytes (0x1f 0x8b) and decompress if needed.
  if (data && data.length >= 2 && data[0] === 0x1f && data[1] === 0x8b) {
    return zlib.gunzipSync(data);
  }
  return data;
}

// parseMbtilesTileURL assumes the URL prefix matches byte-for-byte
// what rampardos/internal/services/renderer/styleprep.go writes into
// the vector source URL. If that rewrite format ever changes (e.g.
// percent-encoding, path normalisation), both this parser and
// styleprep.go must be updated together.
//
// Parse an mbtiles source URL like "mbtiles:///abs/path/to/Combined.mbtiles/z/x/y.pbf".
// maplibre-native constructs these when it needs a tile from a vector
// source whose url is "mbtiles://<path>".
function parseMbtilesTileURL(url) {
  // mbtiles://<path>/<z>/<x>/<y>.pbf — we know our own mbtiles path,
  // strip it, and parse the trailing z/x/y.
  const prefix = "mbtiles://" + args["mbtiles"] + "/";
  if (!url.startsWith(prefix)) {
    throw new Error(`unexpected mbtiles url: ${url}`);
  }
  const tail = url.substring(prefix.length);
  const match = tail.match(/^(\d+)\/(\d+)\/(\d+)(?:\.\w+)?$/);
  if (!match) {
    throw new Error(`unparseable mbtiles tile url: ${url}`);
  }
  return { z: Number(match[1]), x: Number(match[2]), y: Number(match[3]) };
}

try {
  // Open the mbtiles file once, keep it open for the worker's lifetime.
  // SQLite in read-only mode allows many concurrent readers across
  // multiple worker processes.
  db = new Database(args["mbtiles"], { readonly: true, fileMustExist: true });
  tileQuery = db.prepare(
    "SELECT tile_data FROM tiles WHERE zoom_level = ? AND tile_column = ? AND tile_row = ?"
  );

  map = new mbgl.Map({
    ratio: 1,
    request: (req, callback) => {
      try {
        const url = req.url;
        if (url.startsWith("mbtiles://")) {
          const mbtilesPrefix = "mbtiles://" + args["mbtiles"];
          // maplibre-native first requests the source URL itself (no
          // trailing /z/x/y) to discover TileJSON metadata. Return the
          // mbtiles metadata as a TileJSON-like response.
          if (url === mbtilesPrefix || url === mbtilesPrefix + "/") {
            const metaRows = db.prepare("SELECT name, value FROM metadata").all();
            const meta = {};
            for (const row of metaRows) meta[row.name] = row.value;
            const tileJSON = {
              tilejson: "2.0.0",
              tiles: [mbtilesPrefix + "/{z}/{x}/{y}.pbf"],
              minzoom: parseInt(meta.minzoom || "0", 10),
              maxzoom: parseInt(meta.maxzoom || "14", 10),
              name: meta.name || "",
            };
            callback(null, { data: Buffer.from(JSON.stringify(tileJSON)) });
            return;
          }
          const { z, x, y } = parseMbtilesTileURL(url);
          const data = readTile(z, x, y);
          if (!data) {
            callback(null, {}); // missing tile = empty tile, standard mbgl behaviour
            return;
          }
          callback(null, { data });
          return;
        }
        if (url.startsWith("file://")) {
          const filePath = fileURLToPath(url);
          const data = fs.readFileSync(filePath);
          callback(null, { data });
          return;
        }
        callback(new Error(`unsupported url scheme: ${url}`));
      } catch (err) {
        callback(err);
      }
    },
  });

  styleJSON = JSON.parse(fs.readFileSync(args["style-path"], "utf8"));
  map.load(styleJSON);
} catch (err) {
  try {
    writeError(`startup: ${err.message}\n${err.stack || ""}`);
  } catch (_) { /* ignore if stdout is broken */ }
  process.stderr.write(`render-worker startup failed: ${err.message}\n`);
  process.exit(1);
}

// Emit the handshake frame so the Go orchestrator knows we're ready.
writeFrame("H", Buffer.from(JSON.stringify({
  pid: process.pid,
  style: args["style-id"],
})));

// Stream parser for length-prefixed request frames on stdin.
let buffer = Buffer.alloc(0);

process.stdin.on("data", (chunk) => {
  buffer = Buffer.concat([buffer, chunk]);
  processPending();
});

process.stdin.on("end", () => {
  try {
    map.release();
  } catch (err) {
    process.stderr.write(`render-worker: map.release() failed: ${err.message}\n`);
  }
  process.exit(0);
});

function processPending() {
  while (buffer.length >= 5) {
    const type = String.fromCharCode(buffer[0]);
    const len = buffer.readUInt32BE(1);
    if (buffer.length < 5 + len) return;

    const payload = buffer.subarray(5, 5 + len);
    buffer = buffer.subarray(5 + len);

    if (type === "R") {
      handleRequest(payload);
    } else {
      writeError(`unexpected frame type: ${type}`);
    }
  }
}

function handleRequest(payload) {
  let req;
  try {
    req = JSON.parse(payload.toString("utf8"));
  } catch (err) {
    writeError(`bad request JSON: ${err.message}`);
    return;
  }

  const renderOpts = {
    zoom: req.zoom,
    center: req.center,
    width: req.width,
    height: req.height,
    bearing: req.bearing ?? 0,
    pitch: req.pitch ?? 0,
  };

  let done = false;
  map.render(renderOpts, (err, rgba) => {
    if (done) return;
    done = true;
    if (err) {
      writeError(err.message || String(err));
      return;
    }
    // rgba is the raw pixel buffer; maplibre-native hands us a Uint8Array.
    writeFrame("K", Buffer.from(rgba.buffer, rgba.byteOffset, rgba.byteLength));
  });
}
