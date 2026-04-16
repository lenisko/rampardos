# Claude notes

Non-obvious rules for this codebase. Add entries only where the WHY isn't
visible in the code.

## Renderer: scale and stitching

- One worker pool per `(styleID, scale)` tuple. Scale is baked into the
  maplibre-native `ratio` at pool construction — not a per-request parameter.
  Pools are created lazily by `getOrCreatePool(styleID, scale)`.
- Integer zoom → tile stitching (cacheable via `Cache/Tile`). Fractional
  zoom → native viewport render (not tile-cacheable). The dispatcher
  (`generateBaseStaticMap`) picks the branch on `isFractional(zoom)`.
- External styles can't use viewport render — they always tile-stitch,
  with fractional zoom approximated to the nearest integer.
  `logExternalViewportApprox` logs this so it's observable.
- Worker receives **logical** `width × height` and outputs
  `width*scale × height*scale` actual pixels. `encodeRGBA` must be called
  with the pixel dimensions or the output has the wrong extent.
- Regression hotspot. `77e68a8` reverted a scale>1 viewport bypass;
  `3f345cd` added per-scale pools. Exercise scale=1 **and** scale=2
  whenever you touch viewport/tile math.

## Cache intent: nocache, TTL, owned

- The expiry queue is **extend-only**: a shorter TTL never shortens an
  existing entry. This is the only reason concurrent nocache / TTL /
  owned requests don't delete each other's files.
- `nocacheBaseTTLFloor` (30s) is the minimum lifetime even for
  `nocache=true`, so a burst of concurrent pregenerate+ttl subscribers
  can still fetch the base.
- `nocache=true` + `pregenerate=true` silently converts to `ttl=30`.
  Returning a URL means the consumer needs time to fetch — immediate
  delete would 404 them.
- `OwnedThreshold` requires a `CacheCleaner` for the target folder. With
  no cleaner, `Unown` is never called and the owned set grows for the
  process lifetime.

## Base vs final path

- `staticMap.BasePath()` is style + viewport only (no drawables);
  `staticMap.Path()` is base + drawables. Many requests share one base.
- No drawables → `path` is an `atomicWriteFile` copy of `basePath`, not
  a re-render. Saves the composition work entirely.
- Two singleflight groups: `baseSfg` dedupes base renders for the same
  `basePath`; the outer `sfg` dedupes final-path generation. A single
  burst of N concurrent requests for the same spawn triggers one base
  render and one final composition.
- `ensureBase` always `os.Stat`s the file. A stale cache-index once
  claimed "cached" after the file was gone, producing a 404 on
  `ServeFile`.

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
