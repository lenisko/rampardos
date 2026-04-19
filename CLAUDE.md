# Claude notes

Non-obvious rules for this codebase. Add entries only where the WHY isn't
visible in the code.

## Renderer: scale and stitching

- One worker pool per `(styleID, scale)` tuple. Scale is baked into the
  maplibre-native `ratio` at pool construction — not a per-request parameter.
  Pools are created lazily by `getOrCreatePool(styleID, scale)`.
- For local styles: integer zoom → tile stitching (unless
  `LOCAL_STYLES_USE_VIEWPORT=true`, which routes it through native
  viewport render). Fractional zoom → always native viewport render.
  The dispatcher (`generateBaseStaticMap`) picks the branch on
  `isFractional(zoom) || h.localUseViewport`.
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
  base renders and final staticmap outputs as `image.Image`. Size
  via `COMPOSITE_IMAGE_CACHE_SIZE` (default 200).
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
