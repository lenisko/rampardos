# Claude notes

Non-obvious rules for this codebase. Add entries only where the WHY isn't
visible in the code.

## Renderer: scale and stitching

- One worker pool per `(styleID, scale)` tuple. Scale is baked into the
  maplibre-native `ratio` at pool construction — not a per-request parameter.
  Pools are created lazily by `getOrCreatePool(styleID, scale)`.
- **Two-level concurrency:** `cfg.PoolSize` (RENDERER_POOL_SIZE) is a
  **global** semaphore capping concurrent renders across *all* pools, not a
  per-pool size. `cfg.StylePoolSize` (STYLE_POOL_SIZE, default=PoolSize) is
  the per-pool worker count. Without the global cap, N styles × M scales ×
  PoolSize workers could oversubscribe CPU; the semaphore keeps the ceiling
  at PoolSize regardless of how many pools exist.
- For local styles: default is native viewport render for all zooms
  (`LOCAL_STYLES_USE_VIEWPORT=true` by default). Set to `false` to route
  integer-zoom requests through tile stitching; fractional zoom is
  always viewport-rendered regardless. The dispatcher
  (`generateBaseStaticMap`) picks the branch on
  `isFractional(zoom) || h.localUseViewport`.
- External styles can't use viewport render — they always tile-stitch,
  with fractional zoom approximated to the nearest integer.
  `logExternalViewportApprox` logs this so it's observable.
- Worker receives **logical** `width × height` and outputs
  `width*scale × height*scale` actual pixels. `encodeRGBA` must be called
  with the pixel dimensions or the output has the wrong extent.
- **Viewport zoom convention differs from MapLibre.** Callers send zoom
  in the 256-tile web-map convention (what poracle, Google, OSM, etc.
  use). MapLibre Native interprets zoom in the style source's tileSize
  convention — 512 for vector sources (the default). We subtract
  `log2(tileSize/256)` from the user's zoom before dispatching, so a
  512-tileSize style sees `zoom-1` internally. See `styleZoomOffset`.
  The adjustment is only applied on the viewport path; `TileToViewport`
  already produces a frame in MapLibre's native units (512 px at `z`,
  or 256 px at `z-1` for `?tileSize=256`) for the common 512-tileSize
  case, so tile rendering is already correct.
- **`/tile` output size.** Tiles are 512 logical px by default, which
  256-px clients (Leaflet, OpenLayers) squeeze into a 256 slot, halving
  label size. `?tileSize=256` renders a 256-px frame one MapLibre zoom
  lower: same extent, labels sized for 256. Local styles only — external
  styles are proxied and return 400. At `z=0` the `z-1` frame would be
  clamped to MapLibre's minimum zoom and show part of the world, so
  `Render` downsamples the 512 world tile instead.
- **Tile cache filenames.** 512 tiles keep `style-z-x-y-scale.format`
  so existing caches survive upgrades, and because `static_map.go`
  (redownload) and `image_utils_native.go` (stitch grid) parse that
  shape by splitting on `-`. Other sizes append `-<size>`
  (`tileCachePath`). Static maps only stitch 512 tiles, so the parsers
  never see a suffixed name — keep it that way or teach them the
  suffix.
- Regression hotspot. `77e68a8` reverted a scale>1 viewport bypass;
  `3f345cd` added per-scale pools. Exercise scale=1 **and** scale=2
  whenever you touch viewport/tile math.
- **Executor-binding still invariants** (`goWorker.awaitStill`, the
  core-worker driver): a static-mode map renders only on demand. The
  await keeps exactly one frame demand armed with
  `IF_NEEDED | WAIT_FOR_UPDATE` and re-arms it on every result. A
  demand that finds nothing new waits inside the binding and reruns on
  the map's next update, so the host never subscribes to update events
  and never sees NoUpdate churn; a newer demand with the same flags
  supersedes a waiting one instead of queueing (FFI dropped the
  explicit CoalescingBoundary for this rule in #760). When the still's
  Future completes, a session `Barrier` is issued and awaited: it ends
  the demand left waiting (else the next render's camera update wakes
  it and its frame lands before that render's flush) and orders the
  readback after the completing frame. TargetNotReady results fail the
  render rather than re-arming into a spin. The park selects on the
  FrameWake channel, the map-failure channel, the still, and a 50ms
  timer that is missed-wake insurance only.
- **Map failures arrive by push.** The runtime event handler
  (`handleEvents`) runs on the runtime's native thread: it may only
  filter and do a non-blocking send onto `mapErr` — no blocking, no
  `Await`, no runtime calls that wait. Events are dropped until a
  handler is set, so it is set before `CreateMap`. `awaitStill` does
  not flush `mapErr` at render start: a failure from a style reload
  (`SetStyleUrl` is not awaited) arrives between renders and must fail
  the next one, as the old drained event queue did. The map's event
  mask carries only the two failure events.
  `RenderFrameFinished` events do not exist in static mode (mbgl gates
  them to Continuous) — don't build logic on them. A session `Resize`
  completes once the map and driver apply the extent and the map
  publishes its snapshot. The worker
  starts it, runs the still, then checks its completion — that
  ordering stays correct either way. `DrainFrameResults` returns a nil batch and
  no error when the queue is empty (the binding maps NOT_READY to
  absence) — check for nil before using the batch. There is no cancel for a pending still and
  the map refuses a second one — an abandoned still must be settled by
  driving the same await (bounded), else the worker is poisoned.
  Blocking `Future.Await` is safe under the core-worker driver; the
  caller-graphics-thread driver self-deadlocks on it — do not
  reintroduce that driver without restoring a host service loop.

## Tile provider (shared renderer resource provider)

- One process-wide `TileStore` (SQLite pool + byte-bounded LRU of
  decompressed tiles) serves every worker via the binding's resource
  provider; `RENDERER_TILE_PROVIDER=off` is the kill switch back to the
  native mbtiles source, and any store-open failure degrades the same
  way. `RENDERER_TILE_CACHE_MB` bounds the LRU (default 64).
- The custom `rampardos://tile/` scheme is what routes tiles to the
  provider: mbgl only consults providers for network-path URLs, so
  styleprep inlines a TileJSON with that scheme instead of an
  `mbtiles://` url. file:// glyphs/sprites never reach the provider.
- The provider must return DECOMPRESSED tile bytes (mbgl's network path
  assumes HTTP already undid Content-Encoding) and must flip XYZ→TMS
  rows for the mbtiles query. maxzoom in the inline TileJSON is what
  makes overzoom work.
- Dataset activate/combine retargets the mbtiles symlink; ReloadStyles
  reopens the store, drops the tile LRU, and rebuilds the inline
  TileJSON before re-preparing styles.

## Cache intent: nocache, TTL, owned

- `nocache=true` bypasses the composite LRU **read** (forces a fresh
  render) but still writes the result back and still dedupes concurrent
  siblings via singleflight. It does not touch the tile or marker
  caches — those have their own freshness rules.
- `nocache=true` + `pregenerate=true` with no explicit `ttl` defaults
  `ttl=30` so the returned URL lives long enough for the consumer to
  fetch the file. `nocache` itself doesn't affect the disk lifetime.
- The expiry queue is **extend-only**: a shorter TTL never shortens an
  existing entry. This is the only reason concurrent TTL / owned
  requests don't delete each other's files.
- `OwnedThreshold` requires a `CacheCleaner` for the target folder. With
  no cleaner, `Unown` is never called and the owned set grows for the
  process lifetime.

## Base vs final path (image-first, in-memory)

- The hot path's primary currency is `image.Image`, not bytes.
  `ensureBase` returns `image.Image`; `generateStaticMap` and the
  public `GenerateStaticMap` return `image.Image`; the multi
  composer consumes `[]image.Image` from component calls. Encoding
  happens once per HTTP response at the boundary via
  `http.ServeContent(…, bytes.NewReader(encoded))`. No disk
  round-trip on the critical path for local-style requests.
- `CompositeImageCache` (`services.GlobalCompositeImageCache`) is
  the cross-request burst-sharing mechanism. Keyed by path. Holds
  base renders, final staticmaps and multistaticmap composites as
  `image.Image`. **Bounded by decoded-pixel bytes**
  (`COMPOSITE_IMAGE_CACHE_MB`, default 256), not entry count: a
  scale=2 multi composite is tens of times a scale=1 staticmap, and
  the old count bound (200) reached ~1 GB of heap in prod (#34).
  `COMPOSITE_IMAGE_CACHE_SIZE` (default 1000) is only a secondary
  cap. An image bigger than the whole budget is not cached.
- Count a lookup once: the fast-path `Get` records hit/miss; the
  double-checked lookup inside singleflight must use `Recheck`.
  `rampardos_composite_cache_lookups_total{kind}` and
  `..._kind_bytes{kind}` split base / final / multi, to show which
  kind earns its memory.
- The pregenerated fetch-back (`/staticmap/pregenerated/{id}`) is
  served from the disk file and never consults this cache.
- Shared bases (N users, same viewport, different drawables) and
  shared components (multi requests reusing panel viewports) both
  hit this cache without triggering any PNG round-trip. The
  encode-on-serve cost is the only encode in the non-pregenerate
  hot path.
- `baseSfg` dedupes concurrent base renders; the outer `sfg`
  dedupes final-path generation. Followers share the leader's
  image pointer via the sfg return value.
- `staticMap.BasePath()` is the LRU key for base images;
  `staticMap.Path()` for final staticmaps. Both stable across
  process lifetime.
- **Disk writes happen only for `?pregenerate=true`.** Neither
  local-viewport nor external-style tile stitching persists the
  stitched base anymore — both produce an `image.Image` that goes
  into the LRU. Individual tile files (`Cache/Tile/*.png`) are
  still disk-backed for cross-restart tile-download reuse; it's
  the stitched basePath that's gone.
- **Enqueue invariant:** `services.GlobalExpiryQueue.Add` (via
  `enqueueWithBase`) is only called from
  `handlePregenerateResponseBytes`, right next to the
  `AtomicWriteFile`. Every disk file has exactly one matching
  expiry registration. Handler code above does not call
  `enqueueWithBase` — adding one for a file that was never written
  was a latent footgun in the pre-bytes-first pipeline.
- **No disk fallback for long-tail reuse.** Requests without a TTL
  no longer benefit from a 7-day disk cache on duplicate responses.
  Reuse is LRU-bounded (~minutes at 200 slots). Accepted because
  poracle's burst-share window fits inside the LRU; long-window
  regeneration cost is ~50 ms single / ~150 ms multi.

## Singleflight cancellation

- `sfg.Do` callbacks use `context.WithoutCancel(ctx)` for the shared
  generation. A leader's client disconnect must not abort the render
  for followers that are still subscribed. Any new `sfg.Do` site must
  follow this pattern.

## ReloadStyles

- `loadPool` runs outside the write lock so in-flight renders keep
  going. At swap time, pools that a concurrent `getOrCreatePool`
  inserted into `npr.pools` are merged into the new map — never
  `npr.pools = newPools` wholesale.
- `style.prepared.json` must be written via `atomicWritePrepared`
  (tempfile + rename). Workers spawned by one `loadPool` read this
  file; plain `os.WriteFile`'s truncate window exposes a zero-length
  file to them.
