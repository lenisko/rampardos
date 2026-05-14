# In-process Go renderer

**Goal:** Replace the Node subprocess worker pool with an in-process Go
binding to maplibre-native, eliminating the IPC layer while keeping the
rest of rampardos unchanged.

**Status:** design — not started.

---

## 1. Why now

The three-way bench in `rampardos-rust-poc/rampardos-render-worker-rs`
(N=1000 renders, 500×200 staticmap at z=15, lavapipe Vulkan) settled at:

| | p50 | p99 | p99.9 | RSS r1000 |
|---|---|---|---|---|
| Node (current) | 3.78 ms | 5.11 ms | 9.38 ms | 258 MB |
| Rust binary worker | 5.19 ms | 6.85 ms | 14.75 ms | 170 MB |
| Go binary worker | 5.61 ms | 6.90 ms | 7.89 ms | 184 MB |

Node still wins on raw p50 because it skips the OpenGL→Vulkan migration
and uses a tighter binding, but Go is within ~50% and has the **best
p99.9** by a wide margin (7.9 ms vs Node's 9.4 ms — and Node's OpenGL
p99.9 was 32 ms in the same run). Memory is also ~30% lower.

The Go win came from upstream FFI commit `f1d0008` (static-mode renderer
+ runtime event queue). Pre-`f1d0008` the Go worker was 40 ms p50; the
shape we're proposing today only became viable last week.

**The win from going in-process is not raw latency** — it's operational:

- One process instead of N + 1.
- No frame-protocol IPC on the hot path (saves ~0.5–1 ms framing,
  uncertain how much of the bench gap that closes).
- `ReloadStyles` becomes `m.SetStyleURL(...)` instead of "kill all
  workers, respawn pool with new style on disk".
- No worker handshake / spawn lifecycle to debug.
- Memory ceiling drops further: each `*Map` shares the `*Runtime`
  rather than each subprocess carrying its own .so + Go heap.

## 2. What stays the same

The renderer abstraction in `rampardos/internal/services/renderer/renderer.go:18`
is already shaped for backend swap:

```go
type Renderer interface {
    Render(ctx, Request) ([]byte, error)
    RenderViewport(ctx, ViewportRequest) ([]byte, error)
    RenderViewportImage(ctx, ViewportRequest) (*image.NRGBA, error)
    ReloadStyles(ctx) error
}
```

Everything above this — `generateBaseStaticMap` viewport-vs-tile-stitch
dispatch (`handlers/static_map.go:382-399`), `CompositeImageCache`, the
`baseSfg`/`sfg` singleflight wrappers, the LRU keys, the expiry-queue
plumbing — sits **above** the interface and doesn't move. The two-level
concurrency model (global `RendererPoolSize` semaphore × per-pool
`StylePoolSize`) doesn't change either; only the worker primitive does.

Only `nodepool.go` + `pool.go` get replaced.

## 2a. Pool keying — `(styleID, scale)`, unchanged from Node

The pool key stays `(styleID, scale)`. Each Session in a pool is
specialized to that key for its lifetime. `Session.SetStyleURL`
**is only called during `ReloadStyles`** — never on the render hot
path.

Why not a single pool of general-purpose Sessions that swap styles
on demand: every `SetStyleURL` invalidates the parsed style, glyph
atlas, sprite atlas, and tile worker caches. Mbgl has to re-parse and
re-build (~30 ms). Hot renders are ~5.6 ms. Folding styles into a
shared pool turns median renders into swap-then-render on cross-style
traffic — a 6× per-render penalty on bimodal workloads (which is what
poracle-style + external-style traffic actually is).

Cache locality is the property that makes the current implementation
fast; Shape-A keying preserves it.

The only argument for general-purpose pools is RSS scaling under many
hot styles. If that ever becomes a real constraint the fix is **LRU
eviction of cold pools**, not changing the keying. Lazy pool creation
+ optional eviction bounds RSS without the per-render swap penalty.

`Session.Resize(w, h, scale)` is used on the hot path for
`(width, height)` variation **inside** a `(styleID, scale)` pool —
that's the natural per-request mutation. Scale-changes-via-Resize is
not used; pools are scale-segregated as today.

## 2b. Concurrency model — load-bearing

Every method on a `Session` dispatches through that Session's
`Runtime`'s single OS thread. Two renders against the same Session
serialize. Two renders against two Sessions run on two threads in
parallel.

Caching is per-`Map` for the things that take RSS (parsed style, glyph
atlas, sprite atlas, render context). The per-Runtime ambient cache
configures the SQLite tile cache for HTTP fetches; we don't fetch
HTTP — every URL is `mbtiles://` or `file://` — so the ambient cache
is irrelevant. There is no shared cache layer that we'd benefit from
by colocating Sessions.

**Conclusion: pool members are independent `*maplibre.Session` values,
one per worker slot, never sharing Runtimes.** Total Session count is
`Σ pools (StylePoolSize)`, capped at runtime by the global
`RendererPoolSize` semaphore.

## 3. Options

### Option A — keep subprocess, swap Node → Go binary

Change `cmd: "node"` to `cmd: "render-worker-go"` and adjust the spawn
factory. The Go binary already speaks the same `H/R/K/E` frame protocol
(that's why the bench harness can swap workers transparently).

Pros: smallest diff. Bench-validated. Crash isolation preserved.

Cons: Doesn't actually solve the operational pain — still N+1 processes,
still IPC framing, still respawn dance. Latency is slightly worse than
Node today (5.6 ms vs 3.8 ms p50). **Do not pick this.**

### Option B — in-process, pool of `*maplibre.Session` (recommended)

The Go binding ships a `Session` bundle (`session.go`) — `Runtime + Map +
TextureSession + style` behind one handle. Construction is one call
(`NewSession(ctx, opts)`) which blocks until `STYLE_LOADED`; teardown is
one call (`Close()`) which destroys in the right order. Each Session
owns one OS thread via its Runtime; **N Sessions = N parallel
renderers, one thread each**. Calls against a single Session serialize
through that Session's dispatcher, so the pool primitive is "one
Session per worker slot, never share."

```go
type GoPoolRenderer struct {
    cfg     Config
    mu      sync.RWMutex
    pools   map[poolKey]*goStylePool
    sem     chan struct{}      // global RendererPoolSize cap
}

type goStylePool struct {
    sessions chan *maplibre.Session  // StylePoolSize slots
    style    string
    scale    uint8
    bufPool  sync.Pool               // []byte for RenderInto reuse
}
```

The dispatch loop:

```go
func (r *GoPoolRenderer) RenderViewport(ctx context.Context, req ViewportRequest) ([]byte, error) {
    r.sem <- struct{}{}; defer func() { <-r.sem }()
    pool, err := r.getOrCreatePool(req.StyleID, req.Scale)
    if err != nil { return nil, err }

    select {
    case s := <-pool.sessions:
        defer func() { pool.sessions <- s }()
        return renderOne(ctx, s, req, pool.bufPool)
    case <-ctx.Done():
        return nil, ctx.Err()
    }
}
```

`renderOne` is small: `s.Resize(...)` if dimensions changed →
`s.JumpTo(camera)` → `s.RenderInto(ctx, buf)` → encode. Context flows
through every binding call; cancellation is honoured at the dispatcher
boundary.

The canonical structure ships in
`maplibre-native-go/examples/pool/main.go` — fan-out NewSession,
parallel `SetStyleAll` for in-place style swap, ordered teardown.
Lift the shape; rampardos adds the (style, scale) keying, the
two-level concurrency cap, and the singleflight/LRU layers above.

Pros:
- One process. No framing cost. Smallest RSS of any option.
- Reuses every existing layer above the renderer interface.
- Static-mode renderer: each `RenderInto` is one mbgl-internal pass —
  no event-queue dance in our hot path.
- In-place style swap (`Session.SetStyleURL/JSON/Style` reuses the
  Map and renders ~30 ms vs ~80–100 ms for full pool rebuild).
- Context cancellation is first-class — caller-disconnect kills
  in-flight renders cleanly.

Cons:
- **Crash isolation is gone.** A C++ segfault in mbgl takes rampardos
  down. Mitigations in §5. (Note: the binding's dispatcher is now
  panic-safe — Go-side panics in callbacks become errors, not process
  exit. C++ signals still kill us.)
- Tighter coupling to a single FFI version — bumping
  `maplibre-native-ffi` requires rebuilding the .so and matching the
  Go binding's pinned commit (currently `f1d0008`).

### Option C — per-style subprocess, in-process for hot styles

Hybrid: hot styles in-process; long-tail or known-fragile styles spawn a
subprocess. This is overengineering for our actual blast radius. Skip.

**Recommendation: B.** Crash isolation is the only real concern and it's
addressable without a hybrid model.

## 4. About `*Map` recycling

Open question: do we need the equivalent of `WorkerLifetime`?

The Node worker recycles every 500 renders to bound C++ heap growth. The
Rust worker doesn't recycle and the bench showed flat RSS from r100
through r1000. The Go worker showed the same (185 MB flat across the run).

**Recommendation: don't ship recycling. Add a Grafana RSS panel keyed
by process and watch it.** If we see growth in production, we can add
recycling later — the hook is a counter inside `goStylePool.dispatch`
that, every K renders, removes the `*Map` from the channel, calls
`m.Close()`, and replaces it with a freshly built one. ~20 lines.

The bench is a simulated workload but a realistic one (same style,
same fixtures, same viewport drift as poracle traffic). If RSS were
going to balloon, it would have shown up at r1000.

Skip recycling on day one. Monitor. Revisit if the panel says we need it.

## 5. Crash isolation

mbgl is C++. The honest answer is "if it segfaults, the process dies."
Three ways to handle this:

1. **Run rampardos under systemd / k8s with restart-on-fail.** This is
   what we do anyway. The window of dropped requests during a 200 ms
   restart cycle is acceptable for staticmaps (poracle retries; tile
   clients retry).
2. **Wrap each render in `defer recover()`.** Catches Go-side panics
   from the binding (e.g., dispatcher contract violations). Doesn't
   catch C++ exceptions or signals — those still kill the process.
   Still worth doing for diagnostics: log the panic, return 500.
3. **Don't trust untrusted styles.** External styles (the ones that
   tile-stitch through the legacy path — see CLAUDE.md "External styles
   can't use viewport render") have always been a risk for arbitrary
   crashes. We already restrict viewport rendering to local styles; that
   restriction continues.

**Recommendation: rely on supervisor restart + `defer recover()` per
request. No special handling for C++ crashes beyond what the supervisor
already gives us.** The Go binding has been smoke-tested; the failure
modes that bypass Go-level recovery are rare enough that "restart"
is the right answer.

## 6. Style reload

Currently `ReloadStyles` rebuilds the entire pool: stops in-flight
renders, loads styles from disk, spawns new workers, swaps the map
atomically (per CLAUDE.md "loadPool runs outside the write lock so
in-flight renders keep going").

In-process is dramatically simpler: per-Session, call
`s.SetStyleURL(ctx, "file://...")` which reuses the Map and blocks
until `STYLE_LOADED`. No pool rebuild. Iterate the pool's sessions
and run the swaps in parallel — see `examples/pool` `SetStyleAll`
for the shape.

The atomic-swap dance against a concurrent `getOrCreatePool`
(merge-not-overwrite in the existing Node implementation) does **not**
carry over: `ReloadStyles` no longer creates new pool entries, just
mutates existing Sessions' styles in place. New `getOrCreatePool` calls
during a reload either land on an old-style Session (about to be
swapped, fine) or trigger a fresh pool whose construction reads the
current on-disk style (also fine).

The only race to handle: a render in flight on a Session as
`SetStyleURL` is called against it. Take the Session out of the
channel before swapping (acquire as if rendering), swap, release back.
That serializes naturally on the channel.

The tempfile + rename invariant in CLAUDE.md (`atomicWritePrepared`)
still applies for the source-of-truth style file — Sessions may reread
it during reload.

## 7. Migration sequence

1. **Add the Go binding as a dependency.**
   `go get github.com/jfberry/maplibre-native-go@<sha>`. Pin in `go.mod`.
   FFI build is a separate concern — `Makefile` target documents
   `MLN_FFI_REV=f1d00086e0da85617edc1ce5281b4c5f4e5938e1` and points at
   `scripts/build-mln-ffi.sh` (port from
   `rampardos-rust-poc/rampardos-render-worker-rs/scripts/build-mln-ffi-linux.sh`).

2. **Implement `GoPoolRenderer` satisfying the existing `Renderer`
   interface.** New file:
   `rampardos/internal/services/renderer/gopool.go`. Pool primitive is
   `*maplibre.Session`. Mirror `nodepool.go`'s outer structure
   (`getOrCreatePool`, channel-semaphore dispatch) but the inner worker
   becomes a single `NewSession` + per-render `Resize/JumpTo/RenderInto`
   sequence. Skip JSON marshal entirely — pass the request struct
   straight to the Session methods. Estimated ~200 lines (smaller than
   Node version because Session subsumes the lifecycle plumbing).

   Install the binding's process-global log callback (via the new
   process-global API) at process start so mbgl logs flow into
   rampardos's logger rather than stderr.

3. **Reuse `integration_test.go`** by parameterizing it on a `Renderer`
   factory. The existing `TestIntegrationRealWorker` becomes
   `TestIntegrationNode` + `TestIntegrationGo`, sharing assertions.

4. **Wire a feature flag.** `RENDERER_BACKEND=node|go` in `config.go`
   (default `node`). `main.go` picks `NewGoPoolRenderer` or
   `NewNodePoolRenderer` accordingly. Both implement `Renderer`, so
   nothing else changes.

5. **Stage rollout.** Ship behind the flag with `node` default. Toggle
   on a non-customer-facing instance. Watch p50 / p99.9 / RSS for a
   week. Compare error counts. Flip the default.

6. **Delete `nodepool.go` + `pool.go` + the Node worker source.**
   Once the Go path has been default for a release cycle and we're
   confident.

## 8. Bench harness reuse

The three-way bench in
`rampardos-rust-poc/rampardos-render-worker-rs/scripts/backend-bench.sh`
keeps validating the Go subprocess worker against Node + Rust on
identical fixtures. **Don't retire it.** Once rampardos goes
in-process, the harness still answers the question "did the upstream
FFI bump break us?" — point its `GO_REPO_DIR` at whatever maplibre-native-go
revision rampardos is pinned to and re-run before merging the
dependency bump.

## 9. Future work (not blockers)

- **LRU eviction of cold pools.** If many-style deployments grow
  RSS unbounded, evict pools whose Sessions haven't rendered in N
  minutes. Pool drain is `for s := range pool.sessions { s.Close() }`
  after taking the pool out of the map. Add only if Grafana shows it's
  needed.
- **Pool warmup at process start.** Pre-create pools for known-hot
  styles. Cuts first-request latency for cold-start traffic. Not
  needed if lazy-creation latency (~50–100 ms first request) is
  acceptable.

## 10. Out of scope

- **Process-per-style isolation.** Even if a single rogue style could
  crash the renderer, restart-on-fail is sufficient.
- **Hot FFI swap without restart.** The .so is loaded at process start
  and stays for the process lifetime. FFI bumps require a deploy.
- **Tile-stitch path.** Tile-stitch uses individual tile renders and
  Go composes them; that path doesn't go through the renderer interface
  (it goes through the tile cache), so it's untouched. External styles
  continue to use it as today.
- **Metrics renaming.** `worker_lifetime_renders`, `worker_recycle_total`
  etc. become irrelevant on the Go path. Leave them as zero-emitting
  counters until the Node code is deleted in step 6.

## 11. Risks summary

| Risk | Likelihood | Mitigation |
|---|---|---|
| C++ segfault crashes rampardos | Low (Rust+Go both stable in bench) | Supervisor restart. Binding's dispatcher is panic-safe so Go-side panics from callbacks are now caught (commit `92424bf`); only C++ signals can take rampardos down. Compare crash rate to Node baseline during stage. |
| RSS grows unbounded without recycling | Low (bench flat to r1000) | Grafana RSS panel; ship recycling behind a flag if needed. |
| FFI version skew between binding + .so | Medium | `Makefile` pins both; CI builds them together; don't ship a binding bump without a matching .so build. |
| Reload races | Medium | Same shape as Node `ReloadStyles` — merge-not-overwrite semantics in `pools` map. Carry over the lock structure. |
| Performance regression vs Node p50 | Possible (bench shows 5.6 ms vs Node 3.8 ms) | Measure during stage. Likely acceptable given p99.9 + RSS wins. If not, the in-process path saves the framing cost (~0.5–1 ms) which the subprocess bench couldn't avoid; real number TBD. |
