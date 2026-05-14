# rampardos-render-worker

Node worker script that the rampardos Go process spawns as child
processes to rasterise vector mbtiles via
`@maplibre/maplibre-gl-native`.

## Runtime contract

One process per worker slot. Each worker is pinned to a single style
at startup (via `--style-id` / `--style-path`). It handles many
renders of that style over its stdin/stdout pipe until it's killed or
recycled by the Go orchestrator.

## Protocol

Length-prefixed frames, big-endian uint32. See `render-worker.js`
header comment for full details.

## Local verification

To exercise the worker end-to-end on a dev machine without the Go
orchestrator:

```sh
cd rampardos-render-worker
npm install

# You need a real style.json and mbtiles file for this test. Point
# to whatever your dev rig has.
node render-worker.js \
  --style-id dev \
  --style-path /absolute/path/to/style.json \
  --mbtiles /absolute/path/to/Combined.mbtiles \
  --styles-dir /absolute/path/to/Styles \
  --fonts-dir /absolute/path/to/Fonts
```

The worker will print a handshake frame on stdout and then wait for
request frames. To craft a request frame by hand:

```sh
node -e '
const fs = require("fs");
const req = JSON.stringify({zoom: 14, center: [-0.1278, 51.5074], width: 512, height: 512});
const hdr = Buffer.alloc(5);
hdr.writeUInt8("R".charCodeAt(0), 0);
hdr.writeUInt32BE(req.length, 1);
process.stdout.write(hdr);
process.stdout.write(req);
' | node render-worker.js --style-id dev ... > out.bin
```

**Note:** `out.bin` will contain a handshake `'H'` frame followed by
the response `'K'`/`'E'` frame concatenated. To decode, consume the
first 5 bytes (type + uint32 BE length), skip `length` bytes, then
read the next frame. For end-to-end verification use the Go
integration test at
`rampardos/internal/services/renderer/integration_test.go` instead.

For automated testing, see `rampardos/internal/services/renderer/integration_test.go`.
