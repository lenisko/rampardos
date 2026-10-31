# Renderer pool sizing from burst history

Date: 2026-10-31. Status: design, not yet planned.

## Why

Each `(style, scale)` pool in `rampardos/internal/services/renderer/gopool_linux.go`
grows the moment a dispatch finds every worker busy, and retires one worker per
`STYLE_POOL_IDLE_SECONDS` (default 600) tick that saw no saturation at all. On a
large production install this held about 15 workers (~1.5 GB native) for very
little work:

| Pool | Mean workers | Traffic |
|---|---|---|
| carto-voyager x2 (`/tile`, web map) | 3.40 | 0.3 tiles/s, ~87 ms each |
| carto-positron x2 (`/tile`, web map) | 2.45 | 0.05 tiles/s, ~70 ms each |
| osm-bright x1 (poracle weather and others) | 1.82 | hourly spike |
| osm-bright x2 (poracle monster) | up to 3 | ~2/s at ~32 ms, ~5–6/s for a minute on the hour |

Two traffic shapes cause it:

- **Pan bursts on `/tile` styles**: about a dozen tiles at once, every few
  minutes through the day, sparse overnight. Every pan grows the pool to its
  ceiling, and the next pan arrives inside the idle tick, so the pool never
  shrinks.
- **The hourly poracle spike**: weather changes, and the monster
  re-notifications that follow, triple the static-map rate for a minute or two.

Growing on any saturation overshoots (a dozen blocked dispatches grow to the
ceiling within milliseconds, and new workers often finish starting after the
burst has drained), and shrinking on a quiet tick churns (grow, retire, grow
again), paying a cold style load and a `malloc_trim` each time.

The fix must not swing to the other extreme. Sizing on long-run average load
would put every pool at one worker, and bursty pools would then queue behind it
on every burst. Bursty pools should keep the 2 or 3 workers their bursts need;
pools without bursts should sit at 1.

## Goals

- Size each pool by how much concurrency its **recent bursts actually needed**,
  remembered over hours, not by average load and not by any momentary
  saturation.
- Grow as soon as a burst's backlog would take longer to drain than it takes to
  start a worker, so the startup cost is paid once to add concurrency instead
  of the burst queueing behind the workers it has. Do not grow when the backlog
  clears before a new worker could be ready.
- Hold a size through the gaps between recurring bursts (the hourly spike, the
  daytime panning) so pools stop churning.
- Let a pool fall back once its bursts stop (tile pools overnight).
- Measure before and after on the same production install.

## Non-goals

- Changing the global render semaphore (`RENDERER_POOL_SIZE`) or its semantics.
- Per-style configuration. Sizing is learned per pool, so no per-style knobs.
- Scaling a pool to zero workers. `STYLE_POOL_MIN` keeps its meaning and its
  floor of 1.
- Persisting sizing history across restarts.

## Anti-goals

- A bursty pool settling at one worker because its average load is low.
- Reworking dispatch. The shared unbuffered `cmds` channel, `done`,
  `errPoolClosed` retries, the `waiting` counter and the slot-release callback
  were just settled by two review rounds; this design keeps them as they are.
- Latency regressions hidden by memory savings: p95/p99 acquire wait must not
  get meaningfully worse than the baseline.

## Phase 1: baseline (on `feat/in-process-go-renderer`, PR 19)

The current acquire-wait histograms use buckets of
`10µs, 100µs, 1ms, 5ms, 10ms, 50ms, 100ms, 500ms, 1s, 5s, 10s`. The range that
matters, about 5–500 ms, has four boundaries, which is too coarse to compare
designs.

1. Re-bucket `rampardos_renderer_pool_acquire_wait_seconds` and
   `rampardos_renderer_global_acquire_wait_seconds` to:
   `10µs, 1ms, 2ms, 5ms, 10ms, 20ms, 35ms, 50ms, 75ms, 100ms, 150ms, 250ms, 500ms, 1s, 2.5s, 5s, 10s`.
   The 10 µs bucket keeps "did not wait at all" visible.
2. Add `rampardos_renderer_worker_startup_seconds{style,scale}`: a histogram of
   the time from starting a worker to its ready signal (`w.startupErrs <- nil`
   in `goWorker.loop`), for every worker start (pool creation and growth).
   Buckets `25ms … 5s` with fine steps between 50 ms and 1 s.
3. Add `rampardos_renderer_worker_first_render_seconds{style,scale}`: the
   duration of each worker's first render. A new worker's first render may be
   slower than a warm one (glyphs, sprites, its own state); this shows whether
   that cold cost belongs in the growth threshold.
4. Update the sample dashboard's acquire-wait panels to show p50/p95/p99 at
   these resolutions, and add a startup-time panel.

Deploy on the large install and collect at least a full day (all pools through
a daytime, an overnight and the hourly spikes). That is the baseline.

## Phase 2: burst-history sizing (new branch off PR 19)

Branch `feat/pool-burst-sizing` from the PR 19 head that carries phase 1, so
rollback is "deploy PR 19 again".

### Startup time

Each pool keeps a running estimate of its worker startup time, from the same
measurements phase 1 records. A pool always has at least one sample: its first
worker is timed at pool creation. Startup differs by style (dark-matter loads
slower than carto-positron), so the estimate is per pool.

If phase 1 shows the first render on a new worker is meaningfully slower than
a warm one, the threshold is startup time plus that excess. Otherwise it is
the startup time alone. (Builder decides from the baseline data.)

### Backlog estimate

Each pool keeps a running average of its render time `r` (a warm worker's
render, as `viewport_duration_seconds` measures it). When a request arrives at
a pool with `m` workers, "requests ahead" `k` is the number already rendering
or queued there. Its estimated wait is

    wait(k, m) = 0                          if k < m
               = ceil((k - m + 1) / m) * r  otherwise

This is the time until the arriving request starts. If it exceeds the startup
threshold `S`, a worker started now would be ready while that request (and
anything behind it) is still queued, so growing beats queueing. If it does not,
the queue clears before a new worker could take anything, and growing only
costs memory.

The estimate ignores how far the in-flight renders have got, so it can be off
by up to one render time. That error is small next to the decision it feeds.

Examples, with `S = 300 ms`:

| Burst | `k`, `m`, `r` | Wait | Decision |
|---|---|---|---|
| Tile pan, 12th tile of 12 | 11, 1, 70 ms | 770 ms | grow on arrival; second worker ready at ~300 ms; burst ends ~580 ms instead of ~840 ms |
| Same pan, after growing | 11, 2, 70 ms | 420 ms | grow again once the second worker is ready, if the backlog still exceeds `S` |
| osm-bright hourly spike | 3, 1, 32 ms | 96 ms | no growth; the queue drains before a worker could start |

### Grow

On each arrival, the pool computes `wait(k, m)` for the arriving request. If it
exceeds `S`, the pool is below `STYLE_POOL_SIZE`, and no worker in the pool is
already starting, the pool starts one worker immediately. It does not wait for
the request to have waited `S` first: in a burst, most of the requests that
could use the new worker have arrived by then.

A request that has actually been waiting longer than `S` also grows the pool,
under the same conditions. This is a backstop for an `r` that underestimates
(a slow style, a cold tile cache). (My call.)

When a starting worker becomes ready, the pool re-evaluates its current queue
with the new `m`, and starts another worker if the wait at the back of the
queue still exceeds `S`. A long burst climbs one start at a time. (My call.)

Only requests already dispatching to the pool count: they hold a global render
slot, so a pool never sees more than `RENDERER_POOL_SIZE` of them. Waits on the
global cap are a different problem a bigger pool cannot fix, and stay visible
in `global_acquire_wait_seconds`. (My call.)

This replaces today's grow-on-saturation in `dispatch`. The rule that a
retiring worker is replaced while requests wait (`deregister`) stays.

### Burst history

The pool keeps a ring of twelve 15-minute slots covering the last 3 hours. For
each slot it records the largest requests-ahead `k` seen at any arrival.

A slot **needed** size `m` if `wait(k, m - 1)` for its largest `k` exceeds `S`:
with one worker fewer, its worst burst would have queued longer than a worker
takes to start. This is the same test the grow rule applies live, so a size the
pool grows to during a burst is the size history holds it at afterwards. The
payoff is that the next burst finds the worker already running and pays no
startup at all.

`r` and `S` are read at evaluation time, not stored per slot.

### Target and shrink

Every few minutes (builder's choice, e.g. once a minute), the pool computes its
target: the smallest size `m` (at least `STYLE_POOL_MIN`, at most
`STYLE_POOL_SIZE`) such that fewer than 2 slots in the window needed more than
`m`. A size has to be needed in at least 2 separate slots to be held: the
hourly spike needs it in 3 of 12 slots and daytime panning in many, while a
one-off burst in one slot does not hold it.

If the target is above the current size (a worker retired itself after a
glyph failure, or a reload started the pool small), the pool grows one worker
toward it. (My call.)

If the target is below the current size, and the newest worker has been up for
at least 15 minutes, the pool retires one worker. One per evaluation, so a pool
steps down gradually. Any idle worker may be retired; dispatch needs no
ordering of workers.

A one-off burst still grows the pool immediately through the grow rule; the
15-minute minimum stops it growing and shrinking inside the same burst, and
afterwards it is retired at the next evaluation.

The saturation flag and the per-`idleTTL` reaper tick are removed.

### Reload

`ReloadStyles` rebuilds pools. The replacement pool inherits the old pool's
burst history and startup estimate. It is built at the floor, as today, and
grows to its inherited target in the background after the swap, so a reload's
failure behaviour and memory peak stay as they are while the pool's pattern is
kept. (My call.)

### Settings

| Setting | Meaning | Default |
|---|---|---|
| `STYLE_POOL_MIN` | Floor, unchanged | 1 |
| `STYLE_POOL_SIZE` | Ceiling, unchanged | `RENDERER_POOL_SIZE` |
| `STYLE_POOL_WINDOW_SECONDS` | Burst-history window | 10800 (3 h) |
| `STYLE_POOL_BURST_SLOTS` | Slots in the window that must need a size to hold it | 2 |
| `STYLE_POOL_IDLE_SECONDS` | Now the minimum time a grown worker is kept before it can be retired | 900 |

The slot length is fixed at the window divided by 12.

### Metrics

- `rampardos_renderer_pool_target_workers{style,scale}`: the computed target.
- `rampardos_renderer_pool_grow_total` and `..._shrink_total` stay; churn is
  judged from them.
- Phase 1's startup, first-render and refined wait histograms.

## Success criteria (phase 2 vs the phase 1 baseline, same install)

- carto-positron x2 averages close to 1 worker (baseline 2.45); total workers
  and native memory fall.
- p95 and p99 pool acquire wait stay within the startup threshold of the
  baseline's.
- Grow and shrink counts fall well below the baseline's.
- osm-bright pools hold their size through the day rather than cycling around
  each hourly spike.
- Render duration p99 is unchanged.

## Tests

- Unit tests on the sizing maths: estimated wait, target from a slot ring,
  the 2-slot rule, window expiry.
- Pool tests with the existing inert test workers (`newTestPool`): a burst
  whose backlog exceeds `S` grows on arrival, one that doesn't grows nothing,
  the backstop grows after an actual wait past `S`, at most one starting worker, the 15-minute hold, one
  retirement per evaluation, the floor and ceiling, reload inheritance.
- A replay test of the production shapes against a fake clock: scattered pan
  bursts on a tile pool, an hourly spike on a static-map pool, a quiet night.
  Expected sizes: tile pool at its burst size during the "day" and at 1 after 3
  quiet hours; the static-map pool holding its size across hourly spikes.

## Calls made in this document (not discussed)

- Startup is measured to the worker's ready signal; whether to add the cold
  first-render excess is decided from phase 1 data.
- The wait estimate formula and the "requests ahead" statistic.
- Growth is triggered on arrival by the backlog estimate, with an actual wait
  past `S` as a backstop.
- Evaluation every few minutes; the exact interval is the builder's.
- `STYLE_POOL_IDLE_SECONDS` is repurposed as the minimum hold, with its default
  moving from 600 to 900, rather than adding a new setting.
- On reload, the replacement pool inherits history, is built at the floor and
  grows to its target after the swap.
- A starting worker becoming ready re-evaluates the backlog, so a pool can grow
  several steps in one burst, one start at a time.
- The evaluation also grows a pool that is below its target.
- Only requests holding a global slot count toward pool sizing.
- Phase 1 bucket boundaries.

## Left to the builder

- Data structures for the slot ring and running averages.
- How the backstop timer integrates with the existing blocking `select` in
  `dispatch`.
- The running average for `r` (EWMA or windowed).
- The evaluation interval.
- Clock injection for tests.
