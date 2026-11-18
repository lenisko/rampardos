# Renderer pool sizing, phase 1 (baseline metrics) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Measure pool acquire wait at millisecond resolution, worker startup
time and cold-versus-warm render time, so the large install can collect a
baseline before phase 2 changes pool sizing.

**Architecture:** Metrics only; no pool behaviour changes. Two existing
histograms get finer buckets. Three new histograms are observed from
`goWorker.loop` in `gopool_linux.go`: startup (worker start to ready), every
render's worker-side duration, and each worker's first render. The sample
Grafana dashboard gains mean-wait lines and two panels.

**Tech Stack:** Go, `prometheus/client_golang` (`promauto`, `testutil`),
Grafana dashboard JSON.

**Spec:** `docs/superpowers/specs/2026-10-31-renderer-pool-sizing-design.md`
(section "Phase 1"). Phase 2 gets its own plan after the baseline is in.

## Global Constraints

- Branch: `feat/in-process-go-renderer` (PR 19). Commit locally; **push only
  after the user confirms**.
- Go module lives in `rampardos/`; run `go` commands from there.
- The renderer is Linux-only: `gopool_linux.go` changes are verified by the
  integration tests in Docker (command in Task 2), not on macOS.
- Acquire-wait buckets, exactly:
  `10µs, 1ms, 2ms, 5ms, 10ms, 20ms, 35ms, 50ms, 75ms, 100ms, 150ms, 250ms, 500ms, 1s, 2.5s, 5s, 10s`.
- Metric names are prefixed `rampardos_renderer_` and labelled with
  `bucketLabel(...)`-wrapped `style`, `scale`, like their neighbours.
- No change to pool sizing, dispatch, the reaper or the global semaphore.

## Review Focus

1. A worker whose `init()` fails must not record a startup time: failed
   starts would drag down the startup estimate phase 2 builds its threshold
   on. Recording happens only after the `init()` error check (Task 2,
   reviewer check on the code: there is no cheap failing-init fixture).
2. Grown and reloaded workers record startup like construction-time ones.
   They all go through `startWorkerLocked` → `loop`, so the timestamp must be
   taken in `startWorkerLocked`, not in `newGoStylePool` (Task 2 test asserts
   the count rises by `minPoolSize` at construction).
3. A failed first render is not a cold-render sample (it may have returned
   in microseconds on a style error), but it still uses up "first": the
   worker's second render is no longer cold in the same way. Task 2 test
   covers the success case; the rule is stated in Step 3.
4. Dashboard JSON stays loadable: valid JSON, unique panel ids, no
   overlapping grid positions (Task 3 check script).
5. Nothing in the dashboard or docs references removed bucket boundaries
   (`le="0.0001"`, `le="0.005"`); verified absent today, Task 3 re-checks.

---

### Task 1: Buckets and the new histograms

**Files:**
- Modify: `rampardos/internal/services/metrics.go` (struct fields near
  line 101; `rendererPoolAcquireWait` ~line 310; `rendererGlobalAcquireWait`
  ~line 384; recorder methods near `RecordRendererPoolAcquire` ~line 558)
- Test: `rampardos/internal/services/metrics_test.go`

**Interfaces:**
- Produces:
  - `func (m *MetricsManager) RecordRendererWorkerStartup(style, scale string, seconds float64)`
    → `rampardos_renderer_worker_startup_seconds{style,scale}`
  - `func (m *MetricsManager) RecordRendererWorkerRender(style, scale string, seconds float64, first bool)`
    → always observes `rampardos_renderer_worker_render_seconds{style,scale}`;
    when `first`, also observes `rampardos_renderer_worker_first_render_seconds{style,scale}`.
  - Package-level bucket vars `rendererWaitBuckets`, `rendererStartupBuckets`,
    `rendererRenderBuckets` (`[]float64`).

`worker_render_seconds` is not named in the spec. It is the warm-render
reference the spec's first-render comparison needs: `viewport_duration_seconds`
is measured in the handler (encode included, static maps only) and tiles are
in `tile_generate_duration_seconds`, so neither compares like for like with a
worker-side first render. Phase 2's `r` can also read from it.

- [ ] **Step 1: Write the failing test**

Add to `metrics_test.go` a helper that gathers from
`prometheus.DefaultGatherer`, finds the family by name and returns the first
metric's histogram bucket upper bounds (observe once first so the
`HistogramVec` child exists), and:

```go
func TestRendererLatencyBuckets(t *testing.T) {
	m := InitMetrics()
	m.RecordRendererPoolAcquire("bkt", "1", 0.003)
	m.RecordRendererGlobalAcquire(0.003)
	m.DecRendererGlobalInFlight()
	m.RecordRendererWorkerStartup("bkt", "1", 0.2)
	m.RecordRendererWorkerRender("bkt", "1", 0.04, true)

	wait := []float64{0.00001, 0.001, 0.002, 0.005, 0.01, 0.02, 0.035, 0.05, 0.075, 0.1, 0.15, 0.25, 0.5, 1, 2.5, 5, 10}
	startup := []float64{0.025, 0.05, 0.075, 0.1, 0.15, 0.2, 0.3, 0.4, 0.5, 0.75, 1, 2, 5}
	render := []float64{0.005, 0.01, 0.02, 0.035, 0.05, 0.075, 0.1, 0.15, 0.25, 0.5, 1, 2.5, 5}
	for name, want := range map[string][]float64{
		"rampardos_renderer_pool_acquire_wait_seconds":     wait,
		"rampardos_renderer_global_acquire_wait_seconds":   wait,
		"rampardos_renderer_worker_startup_seconds":        startup,
		"rampardos_renderer_worker_render_seconds":         render,
		"rampardos_renderer_worker_first_render_seconds":   render,
	} {
		if got := histogramBounds(t, name); !slices.Equal(got, want) {
			t.Errorf("%s buckets = %v, want %v", name, got, want)
		}
	}
}

func TestRendererWorkerRenderFirstOnlyWhenFirst(t *testing.T) {
	m := InitMetrics()
	all := m.rendererWorkerRender.WithLabelValues("firstonly", "1")
	first := m.rendererWorkerFirstRender.WithLabelValues("firstonly", "1")
	m.RecordRendererWorkerRender("firstonly", "1", 0.04, true)
	m.RecordRendererWorkerRender("firstonly", "1", 0.03, false)
	// assert: sample count of `all` == 2, of `first` == 1
	// (testutil.CollectAndCount counts series, not samples: read
	// SampleCount via (all.(prometheus.Metric)).Write(&dto.Metric{}))
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd rampardos && go test ./internal/services/ -run 'TestRendererLatencyBuckets|TestRendererWorkerRenderFirstOnlyWhenFirst' -v`
Expected: compile failure (`RecordRendererWorkerStartup` undefined).

- [ ] **Step 3: Implement**

Add the three bucket vars, the three `HistogramVec` fields
(`rendererWorkerStartup`, `rendererWorkerRender`, `rendererWorkerFirstRender`)
registered in `newMetricsManager`, the two recorder methods, and switch both
acquire-wait histograms to `rendererWaitBuckets`. Help strings:
- startup: "Time from starting a worker to its ready signal (EGL context, runtime, map, style load), for every worker start: pool creation, growth, replacement and reload. Phase 2 pool sizing grows only when a backlog would outlast this."
- render: "Worker-side duration of each render (renderOne), excluding queueing and encode. The warm-render reference for worker_first_render_seconds."
- first render: "Worker-side duration of each worker's first successful render. Compare with worker_render_seconds to see the cold cost a newly started worker pays."

- [ ] **Step 4: Run to verify it passes**

Run: `cd rampardos && go test ./internal/services/ -v -run 'TestRendererLatencyBuckets|TestRendererWorkerRenderFirstOnlyWhenFirst' && go vet ./...`
Expected: PASS, vet clean.

- [ ] **Step 5: Commit**

```bash
git add rampardos/internal/services/metrics.go rampardos/internal/services/metrics_test.go
git commit -m "feat(metrics): resolve renderer waits to the millisecond and add worker startup and render histograms"
```

### Task 2: Observe startup and render time in the worker

**Files:**
- Modify: `rampardos/internal/services/renderer/gopool_linux.go`
  (`goWorker` struct ~line 918; `startWorkerLocked` ~line 688; `loop` ~line 988)
- Test: `rampardos/internal/services/renderer/integration_metrics_linux_test.go` (create)

**Interfaces:**
- Consumes: `RecordRendererWorkerStartup`, `RecordRendererWorkerRender` (Task 1).
- Produces: `goWorker.started time.Time` (set in `startWorkerLocked`, real
  path only) and `goWorker.rendered bool` (worker goroutine only).

- [ ] **Step 1: Write the failing test**

File header `//go:build renderer_integration`, package `renderer`. Test
`TestWorkerStartupAndRenderMetrics`:
- `services.InitMetrics()`; `cfg := setupIntegrationFixtures(t)`.
- Read sample counts (helper over `prometheus.DefaultGatherer`, matching
  labels `style="bg", scale="1"`) for startup, render and first-render before.
- `NewGoPoolRenderer(cfg)`; `defer r.Close()`; call `runRenderAssertions(t, r)`
  twice (the pool has one worker, so both renders hit it).
- Assert deltas: startup `== 1`, render `== 2`, first render `== 1`; and the
  startup sample sum is `> 0`.

The fixture's pool is created lazily by the first `Render`, with
`StylePoolSize: 1`, so one worker start is expected.

- [ ] **Step 2: Run to verify it fails**

Run (Docker, from the repo root; same as `/tmp/churn/rtest.sh`):
```bash
docker build -q --target rampardos-test -t rampardos-test:p1 . >/dev/null
docker run --rm -e EGL_PLATFORM=surfaceless -e LIBGL_ALWAYS_SOFTWARE=1 -e MESA_GL_VERSION_OVERRIDE=3.3 rampardos-test:p1 sh -c "apt-get update -qq >/dev/null && apt-get install -y -qq --no-install-recommends libegl1 libegl-mesa0 libgl1-mesa-dri libgles2 sqlite3 >/dev/null 2>&1; PKG_CONFIG_PATH=/ffi/install/share/pkgconfig CGO_ENABLED=1 go test -count=1 -tags renderer_integration -run TestWorkerStartupAndRenderMetrics -v ./internal/services/renderer/"
```
Expected: FAIL, startup delta 0.

- [ ] **Step 3: Implement**

- `startWorkerLocked` (real path): set `w.started = time.Now()` before `go w.loop()`.
- `loop`: after the `init()` error branch has returned, record
  `time.Since(w.started)` via `RecordRendererWorkerStartup(p.cfg.styleID, p.cfg.scaleLabel, …)`
  before `w.startupErrs <- nil`, guarded by `services.GlobalMetrics != nil`.
- `loop`, render case: time `w.renderOne(...)`. If `err == nil`, record with
  `first = !w.rendered`. Set `w.rendered = true` after the first render
  whatever its outcome (Review Focus 3).

- [ ] **Step 4: Run to verify it passes**

Run the Step 2 command, then the whole package with race:
`... go test -count=1 -race -tags renderer_integration ./internal/services/renderer/`
Expected: PASS; total test count = previous 78 + 1. Also, on macOS,
`cd rampardos && go build ./... && go vet ./...` still passes (stub unaffected).

- [ ] **Step 5: Commit**

```bash
git add rampardos/internal/services/renderer/gopool_linux.go rampardos/internal/services/renderer/integration_metrics_linux_test.go
git commit -m "feat(renderer): record worker startup and cold versus warm render time"
```

### Task 3: Dashboard

**Files:**
- Modify: `grafana-dashboard.json`

- [ ] **Step 1: Mean wait lines**

In panels 202 ("Pool Acquire Wait") and 207 ("Global Semaphore Acquire
Wait") add refId `D`, legend `mean {{style}} x{{scale}}` (202) / `mean`
(207):
`sum(rate(<metric>_sum{instance=~"$instance"}[$__rate_interval])) by (style, scale) / sum(rate(<metric>_count{instance=~"$instance"}[$__rate_interval])) by (style, scale)`
(207 drops the `by`). The user asked for average latency explicitly.

- [ ] **Step 2: Two panels in "Renderer Health"**

Shift every panel with `gridPos.y >= 119` down by 8. Add at `y: 119`, `h: 8`,
`w: 12`, styled like panel 202 (seconds unit, same fieldConfig):
- id 254, x 0, "Worker Startup Time (start to ready)": p50 and p95 of
  `worker_startup_seconds` by `(le, style, scale)`.
- id 255, x 12, "First Render vs Warm Render": p50 of
  `worker_first_render_seconds` (legend `first {{style}} x{{scale}}`) and of
  `worker_render_seconds` (legend `warm {{style}} x{{scale}}`), both by
  `(le, style, scale)`. Use `rate(...[1h])` rather than `$__rate_interval`
  for the first-render series: it is sparse (one sample per worker start).

- [ ] **Step 3: Check**

Run:
```bash
python3 - <<'EOF'
import json
d=json.load(open('grafana-dashboard.json'))
ps=d['panels']; ids=[p['id'] for p in ps]
assert len(ids)==len(set(ids)), 'duplicate ids'
cells=set()
for p in ps:
    g=p['gridPos']
    for y in range(g['y'],g['y']+g['h']):
        for x in range(g['x'],g['x']+g['w']):
            assert (x,y) not in cells, ('overlap', p['id']); cells.add((x,y))
s=open('grafana-dashboard.json').read()
assert 'le=\\"0.0001\\"' not in s and 'le=\\"0.005\\"' not in s
print('ok', len(ps))
EOF
```
Expected: `ok 62`.

- [ ] **Step 4: Commit**

```bash
git add grafana-dashboard.json
git commit -m "feat(dashboard): show mean acquire wait, worker startup and cold renders"
```

### Task 4: Ship for the baseline

- [ ] **Step 1:** Run the full suites: `cd rampardos && go test -race ./...`
  (macOS) and the Docker integration package run from Task 2 Step 4. Expected:
  all pass.
- [ ] **Step 2:** Ask the user to confirm the push of the three commits to
  `upstream feat/in-process-go-renderer`. After the push, add the three new
  histograms to the PR description's *Metrics* section (`gh pr edit 19
  --body-file`), also with confirmation.
- [ ] **Step 3:** Tell the user what to collect from the large install after
  at least a full day: per pool, p50/p95/p99 and mean pool acquire wait,
  startup p50/p95, first versus warm render p50, workers, grow and shrink
  counts. That is the phase 2 baseline.
