package services

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	prommodel "github.com/prometheus/client_model/go"
)

// bucketLabel returns the input if it matches a strict identifier pattern and is short,
// otherwise "other" — to prevent unbounded Prometheus label cardinality from user input.
var labelSafeRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func bucketLabel(s string) string {
	if len(s) > 64 || !labelSafeRe.MatchString(s) {
		return "other"
	}
	return s
}

// MetricsManager handles all Prometheus metrics
type MetricsManager struct {
	startTime time.Time

	// Daily error counts by category (in-memory, last 7 days)
	dailyErrors   map[string]map[string]uint64 // key: category -> YYYY-MM-DD -> count
	dailyErrorsMu sync.RWMutex

	// Request metrics
	requestsTotal    *prometheus.CounterVec
	cacheHitsTotal   *prometheus.CounterVec
	cacheMissTotal   *prometheus.CounterVec
	requestDuration  *prometheus.HistogramVec
	requestsInFlight *prometheus.GaugeVec

	// HTTP client metrics
	httpClientRequests *prometheus.CounterVec
	httpClientErrors   *prometheus.CounterVec
	httpClientDuration *prometheus.HistogramVec

	// Error metrics
	errorsTotal *prometheus.CounterVec

	// Runtime metrics
	uptimeSeconds  prometheus.Gauge
	memoryRSSBytes prometheus.Gauge
	memoryVSSBytes prometheus.Gauge

	// Queue metrics
	fileToucherQueueSize prometheus.Gauge
	fileRemoverQueueSize *prometheus.GaugeVec

	// Template metrics
	templateRendersTotal *prometheus.CounterVec

	// Cache size metrics
	cacheSizeBytes *prometheus.GaugeVec

	// In-memory image cache metrics (marker LRU, tile LRU). Distinct
	// from the on-disk {cache_hits,misses}_total{type=...} counters:
	// these measure whether a decoded image was reused from memory.
	imageCacheHits   *prometheus.CounterVec
	imageCacheMisses *prometheus.CounterVec

	// Tile generation time split by source: disk cache hit, local
	// render via maplibre-native, or external download. Covers every
	// GenerateTile call including internal ones from the static-map
	// stitcher (which bypass the /tile HTTP handler and therefore
	// don't land in rampardos_request_duration_seconds{type="tile"}).
	tileGenerateDuration *prometheus.HistogramVec

	// Tile decode time split by source: RAM LRU hit (memcpy + lock)
	// vs disk read + image.Decode. Measures the step between
	// "tile bytes are available" and "decoded *image.NRGBA in hand"
	// so we can see whether further format work (alternate on-disk
	// encoding) would meaningfully reduce per-stitch CPU.
	tileDecodeDuration *prometheus.HistogramVec

	// Viewport render time for the arbitrary-size maplibre-native
	// path. Covers all local-style staticmaps (any zoom); distinct
	// from tile_generate_duration which only times per-tile work for
	// external styles.
	rendererViewportDuration *prometheus.HistogramVec

	// Renderer pool saturation tripwires. Every local staticmap base
	// is a live renderer call — there is no disk-cache buffer — so
	// visibility into whether the render pool is the bottleneck matters.
	rendererPoolAcquireWait    *prometheus.HistogramVec // time callers waited for an idle worker in (style, scale) pool
	rendererPoolIdleWorkers    *prometheus.GaugeVec     // snapshot of idle workers, updated per acquire
	rendererPoolWorkers        *prometheus.GaugeVec     // resident workers per (style, scale); varies as pools grow/shrink
	rendererPoolWorkersMax     *prometheus.GaugeVec     // high-water resident workers per pool since start
	rendererPoolGrow           *prometheus.CounterVec   // workers added on demand
	rendererPoolShrink         *prometheus.CounterVec   // workers retired when idle
	rendererWorkerReplacements *prometheus.CounterVec   // reason=error|lifetime

	// Go-renderer per-render breakdown. Decomposes the total per-render
	// wall time (rendererViewportDuration) so we can see where the time
	// actually goes: service-loop turns, notification-park time, time to
	// the first event/frame result, and total ServiceDriverWork time.
	// Metric names keep their historical pump_* prefix (from the
	// pre-executor host-pumping binding) so dashboards survive; the
	// semantics are the service-loop analogues.
	rendererPumpIterations      *prometheus.HistogramVec // service-loop turns per render
	rendererPumpSleep           *prometheus.HistogramVec // total seconds parked awaiting runtime notifications per render
	rendererPumpTimeToFirstEv   *prometheus.HistogramVec // seconds from loop start to first event or frame result
	rendererPumpRenderUpdate    *prometheus.HistogramVec // total seconds in sess.ServiceDriverWork() per render
	rendererPumpRenderUpdateCnt *prometheus.HistogramVec // driver work items serviced per render (>1 = incremental draws)
	rendererReadback            *prometheus.HistogramVec // seconds for the full readback operation (start→service→take) per render

	// Global concurrency semaphore (RENDERER_POOL_SIZE). Caps
	// concurrent renders across all pools; complements the per-pool
	// saturation metrics above.
	rendererGlobalCapacity    prometheus.Gauge
	rendererGlobalInFlight    prometheus.Gauge
	rendererGlobalInFlightMax prometheus.Gauge // high-water concurrent renders since start

	// inFlight mirrors rendererGlobalInFlight so the high-water mark can be
	// computed without reading back from Prometheus.
	inFlight                  int
	peakInFlight              float64
	inFlightMu                sync.Mutex
	rendererGlobalAcquireWait prometheus.Histogram

	// Dataset size metrics
	datasetSizeBytes *prometheus.GaugeVec
}

// Low-cardinality label values used across metric recordings.
// Keeping them as constants prevents typos silently splitting a
// counter into multiple dimensions.
const (
	TileSourceCache    = "cache"
	TileSourceLocal    = "local"
	TileSourceExternal = "external"

	ImageCacheTile      = "tile"
	ImageCacheMarker    = "marker"
	ImageCacheComposite = "composite"

	TileDecodeSourceRAMLRU = "ram_lru"
	TileDecodeSourceDisk   = "disk"

	WorkerReplacementError    = "error"
	WorkerReplacementLifetime = "lifetime"
)

var (
	GlobalMetrics *MetricsManager
	metricsOnce   sync.Once
)

// InitMetrics initializes the global metrics manager
func InitMetrics() *MetricsManager {
	metricsOnce.Do(func() {
		GlobalMetrics = newMetricsManager()
	})
	return GlobalMetrics
}

func newMetricsManager() *MetricsManager {
	m := &MetricsManager{
		startTime: time.Now(),
		dailyErrors: map[string]map[string]uint64{
			"http":       make(map[string]uint64),
			"template":   make(map[string]uint64),
			"validation": make(map[string]uint64),
			"generic":    make(map[string]uint64),
		},

		requestsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "rampardos_requests_total",
			Help: "Total number of requests",
		}, []string{"type", "style", "cached"}),

		cacheHitsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "rampardos_cache_hits_total",
			Help: "Total number of cache hits",
		}, []string{"type"}),

		cacheMissTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "rampardos_cache_misses_total",
			Help: "Total number of cache misses",
		}, []string{"type"}),

		requestDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "rampardos_request_duration_seconds",
			Help:    "Request duration in seconds",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0, 30.0},
		}, []string{"type", "style", "cached"}),

		requestsInFlight: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "rampardos_requests_in_flight",
			Help: "Number of requests currently being processed",
		}, []string{"type"}),

		httpClientRequests: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "rampardos_http_client_requests_total",
			Help: "Total HTTP client requests",
		}, []string{"host"}),

		httpClientErrors: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "rampardos_http_client_errors_total",
			Help: "Total HTTP client errors",
		}, []string{"host"}),

		httpClientDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "rampardos_http_client_duration_seconds",
			Help:    "HTTP client request duration",
			Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0},
		}, []string{"host"}),

		errorsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "rampardos_errors_total",
			Help: "Total errors",
		}, []string{"type", "reason"}),

		uptimeSeconds: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "rampardos_uptime_seconds",
			Help: "Process uptime in seconds",
		}),

		memoryRSSBytes: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "rampardos_memory_rss_bytes",
			Help: "Resident Set Size in bytes",
		}),

		memoryVSSBytes: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "rampardos_memory_vss_bytes",
			Help: "Virtual Set Size in bytes",
		}),

		fileToucherQueueSize: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "rampardos_filetoucher_queue_size",
			Help: "Size of the file toucher queue",
		}),

		fileRemoverQueueSize: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "rampardos_fileremover_queue_size",
			Help: "Size of the file remover queue (files pending removal)",
		}, []string{"folder"}),

		templateRendersTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "rampardos_template_renders_total",
			Help: "Total template renders",
		}, []string{"template", "method", "type"}),

		cacheSizeBytes: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "rampardos_cache_size_bytes",
			Help: "Size of cache directories in bytes",
		}, []string{"folder"}),

		imageCacheHits: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "rampardos_image_cache_hits_total",
			Help: "Hits on the in-memory decoded-image LRUs. cache=tile covers the base-map stitch path; cache=marker covers resized marker overlays.",
		}, []string{"cache"}),

		imageCacheMisses: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "rampardos_image_cache_misses_total",
			Help: "Misses on the in-memory decoded-image LRUs. A miss forces a file read + image.Decode.",
		}, []string{"cache"}),

		tileGenerateDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "rampardos_tile_generate_duration_seconds",
			Help:    "Time spent producing a tile. source=cache is a disk hit; source=local is a maplibre-native render; source=external is an upstream download. Includes stitcher-initiated calls that skip the /tile HTTP handler.",
			Buckets: []float64{0.0005, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0},
		}, []string{"style", "source"}),

		tileDecodeDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "rampardos_tile_decode_duration_seconds",
			Help:    "Time to produce a decoded *image.NRGBA for a tile. source=ram_lru is a memory cache hit (lock + memcpy); source=disk is a file read + image.Decode on miss.",
			Buckets: []float64{0.00001, 0.00005, 0.0001, 0.0005, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25},
		}, []string{"source"}),

		rendererViewportDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "rampardos_renderer_viewport_duration_seconds",
			Help:    "Time spent inside renderer.RenderViewport, covering all local-style staticmap bases (integer and fractional zoom). Includes maplibre-native render + encode + IPC; excludes the caller's disk write.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0},
		}, []string{"style", "scale"}),

		rendererPoolAcquireWait: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "rampardos_renderer_pool_acquire_wait_seconds",
			Help:    "Time a dispatch call spent waiting for an idle worker in its (style, scale) pool. Sustained high percentiles indicate the pool is saturated; under healthy load this should be dominated by the sub-ms bucket.",
			Buckets: []float64{0.00001, 0.0001, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1.0, 5.0, 10.0},
		}, []string{"style", "scale"}),

		rendererPoolIdleWorkers: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "rampardos_renderer_pool_idle_workers",
			Help: "Idle workers snapshotted at the moment a dispatch acquires one from the (style, scale) pool. 0 means the pool was fully busy when this dispatch entered.",
		}, []string{"style", "scale"}),

		rendererPoolWorkers: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "rampardos_renderer_pool_workers",
			Help: "Resident workers in each (style, scale) pool. Pools start at STYLE_POOL_MIN, grow towards STYLE_POOL_SIZE when a dispatch finds every worker busy, and retire one worker per idle interval back down to the floor. Each worker is a full mbgl map + runtime + EGL context, so this gauge tracks the renderer's memory footprint.",
		}, []string{"style", "scale"}),

		rendererPoolWorkersMax: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "rampardos_renderer_pool_workers_max",
			Help: "Highest resident worker count each (style, scale) pool has reached since process start. The pool_workers gauge only shows the value at scrape time, so a pool that grew during a burst and decayed before the next scrape leaves no trace there; this is the number to size STYLE_POOL_SIZE against.",
		}, []string{"style", "scale"}),

		rendererPoolGrow: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "rampardos_renderer_pool_grow_total",
			Help: "Workers added because a dispatch found every worker in the pool busy. Compare with pool_shrink_total: comparable rates on a short interval mean the pool is oscillating and STYLE_POOL_IDLE_SECONDS is too low.",
		}, []string{"style", "scale"}),

		rendererPoolShrink: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "rampardos_renderer_pool_shrink_total",
			Help: "Workers retired after an idle interval, each releasing an mbgl map, runtime and EGL context.",
		}, []string{"style", "scale"}),

		rendererWorkerReplacements: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "rampardos_renderer_worker_replacements_total",
			Help: "Worker processes killed and respawned. reason=error counts abnormal dispatch failures; reason=lifetime counts routine recycling after workerLifetime renders.",
		}, []string{"style", "scale", "reason"}),

		rendererPumpIterations: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "rampardos_renderer_pump_iterations_per_render",
			Help:    "Number of service-loop turns the Go renderer executed per render (ServiceDriverWork + event/frame drain + operation poll). High counts with low pump_sleep mean overhead-bound; high counts with high pump_sleep mean mbgl is slow to produce work.",
			Buckets: []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 5000},
		}, []string{"style", "scale"}),

		rendererPumpSleep: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "rampardos_renderer_pump_sleep_seconds_per_render",
			Help:    "Total seconds the Go renderer spent parked awaiting runtime notifications per render. The service loop only parks when no driver work was serviced; this is genuine wait on mbgl progress (tile IO, parse/layout), bounded by a 100ms defensive cap per park.",
			Buckets: []float64{0.0001, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25},
		}, []string{"style", "scale"}),

		rendererPumpTimeToFirstEv: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "rampardos_renderer_pump_time_to_first_event_seconds",
			Help:    "Seconds from the service loop's start to the first runtime event or frame result drained. Measures mbgl's warm-up cost: how long the executor takes to start producing render updates (driven by tile/glyph/sprite fetch latency).",
			Buckets: []float64{0.0001, 0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0},
		}, []string{"style", "scale"}),

		rendererPumpRenderUpdate: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "rampardos_renderer_pump_render_update_seconds_per_render",
			Help:    "Total seconds spent inside sess.ServiceDriverWork() per render. This executes the queued graphics work (draws, readbacks) on the worker's EGL thread; the remainder of total render time is mbgl warm-up + loop overhead.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5},
		}, []string{"style", "scale"}),

		rendererPumpRenderUpdateCnt: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "rampardos_renderer_pump_render_update_count_per_render",
			Help:    "Number of driver work items serviced per render. High values mean mbgl scheduled multiple incremental draws before the still image completed (e.g. as tiles arrived progressively).",
			Buckets: []float64{1, 2, 3, 5, 10, 20, 50},
		}, []string{"style", "scale"}),

		rendererReadback: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "rampardos_renderer_readback_seconds_per_render",
			Help:    "Seconds for the full readback operation per render (ReadPremultipliedRGBA8Start, service to completion, Take). The glReadPixels-equivalent plus the binding's copy-out. On Mesa llvmpipe this should be low single-digit ms for 512×512×4; large values indicate driver-side stall or wrong attachment type.",
			Buckets: []float64{0.0001, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25},
		}, []string{"style", "scale"}),

		rendererGlobalCapacity: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "rampardos_renderer_global_capacity",
			Help: "Maximum concurrent renders across all (style, scale) pools (RENDERER_POOL_SIZE). Static; set once at renderer init.",
		}),

		rendererGlobalInFlightMax: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "rampardos_renderer_global_in_flight_max",
			Help: "Highest number of concurrent renders observed since process start. Compare with rampardos_renderer_global_capacity: if this stays well below the cap, the configured concurrency (and the worker pools sized for it) is never needed.",
		}),

		rendererGlobalInFlight: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "rampardos_renderer_global_in_flight",
			Help: "Active renders holding the global concurrency semaphore. Scrape together with rampardos_renderer_global_capacity for utilisation.",
		}),

		rendererGlobalAcquireWait: promauto.NewHistogram(prometheus.HistogramOpts{
			Name:    "rampardos_renderer_global_acquire_wait_seconds",
			Help:    "Time a dispatch spent waiting for the global concurrency semaphore before even attempting the per-pool acquire. Non-zero percentiles indicate the global cap is the limiting factor, not a specific pool.",
			Buckets: []float64{0.00001, 0.0001, 0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1.0, 5.0, 10.0},
		}),

		datasetSizeBytes: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "rampardos_dataset_size_bytes",
			Help: "Size of dataset files in bytes",
		}, []string{"name"}),
	}

	// Start runtime metrics updater
	go m.updateRuntimeMetrics()

	// Start daily error cleanup
	go m.cleanupOldDailyErrors()

	return m
}

func (m *MetricsManager) updateRuntimeMetrics() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		m.uptimeSeconds.Set(time.Since(m.startTime).Seconds())

		// Try to read from /proc/self/smaps_rollup for accurate memory metrics
		if memInfo := readSmapsRollup(); memInfo != nil {
			m.memoryRSSBytes.Set(float64(memInfo.RSS))
			m.memoryVSSBytes.Set(float64(memInfo.VSS))
		} else {
			// Fallback to Go runtime stats
			var memStats runtime.MemStats
			runtime.ReadMemStats(&memStats)
			m.memoryRSSBytes.Set(float64(memStats.Alloc))
			m.memoryVSSBytes.Set(float64(memStats.Sys))
		}

	}
}

// MemoryInfo holds memory statistics
type MemoryInfo struct {
	RSS uint64 // Resident Set Size
	VSS uint64 // Virtual Set Size
}

// readSmapsRollup reads memory info from /proc/self/smaps_rollup (Linux only)
func readSmapsRollup() *MemoryInfo {
	f, err := os.Open("/proc/self/smaps_rollup")
	if err != nil {
		return nil
	}
	defer f.Close()

	info := &MemoryInfo{}
	scanner := bufio.NewScanner(f)

	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		key := strings.TrimSuffix(fields[0], ":")
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		// Values in smaps are in kB
		value *= 1024

		if key == "Rss" {
			info.RSS = value
			break
		}
	}

	// Read VSS from /proc/self/status (VmSize)
	info.VSS = readVSSFromStatus()

	return info
}

// readVSSFromStatus reads VmSize from /proc/self/status
func readVSSFromStatus() uint64 {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "VmSize:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				value, err := strconv.ParseUint(fields[1], 10, 64)
				if err == nil {
					return value * 1024 // kB to bytes
				}
			}
			break
		}
	}
	return 0
}

// RecordRequest records a request with type, cache status, and duration
func (m *MetricsManager) RecordRequest(reqType, style string, cached bool, duration float64) {
	cachedStr := "false"
	if cached {
		cachedStr = "true"
	}

	m.requestsTotal.WithLabelValues(reqType, bucketLabel(style), cachedStr).Inc()

	if cached {
		m.cacheHitsTotal.WithLabelValues(reqType).Inc()
	} else {
		m.cacheMissTotal.WithLabelValues(reqType).Inc()
	}

	m.requestDuration.WithLabelValues(reqType, bucketLabel(style), cachedStr).Observe(duration)
}

// IncrementInFlight increments the in-flight counter for a request type
func (m *MetricsManager) IncrementInFlight(reqType string) {
	m.requestsInFlight.WithLabelValues(reqType).Inc()
}

// DecrementInFlight decrements the in-flight counter for a request type
func (m *MetricsManager) DecrementInFlight(reqType string) {
	m.requestsInFlight.WithLabelValues(reqType).Dec()
}

// RecordTileRequest records a tile request
func (m *MetricsManager) RecordTileRequest(style string, cached bool) {
	cachedStr := "false"
	if cached {
		cachedStr = "true"
	}
	m.requestsTotal.WithLabelValues("tile", bucketLabel(style), cachedStr).Inc()
	if cached {
		m.cacheHitsTotal.WithLabelValues("tile").Inc()
	} else {
		m.cacheMissTotal.WithLabelValues("tile").Inc()
	}
}

// RecordStaticMapRequest records a static map request
func (m *MetricsManager) RecordStaticMapRequest(style string, cached bool) {
	cachedStr := "false"
	if cached {
		cachedStr = "true"
	}
	m.requestsTotal.WithLabelValues("staticmap", bucketLabel(style), cachedStr).Inc()
	if cached {
		m.cacheHitsTotal.WithLabelValues("staticmap").Inc()
	} else {
		m.cacheMissTotal.WithLabelValues("staticmap").Inc()
	}
}

func (m *MetricsManager) RecordTileGenerate(style, source string, duration float64) {
	m.tileGenerateDuration.WithLabelValues(bucketLabel(style), bucketLabel(source)).Observe(duration)
}

func (m *MetricsManager) RecordTileDecode(source string, duration float64) {
	m.tileDecodeDuration.WithLabelValues(bucketLabel(source)).Observe(duration)
}

func (m *MetricsManager) RecordRendererViewport(style, scale string, duration float64) {
	m.rendererViewportDuration.WithLabelValues(bucketLabel(style), bucketLabel(scale)).Observe(duration)
}

func (m *MetricsManager) RecordRendererPoolAcquire(style, scale string, waitSeconds float64, idleAfter int) {
	m.rendererPoolAcquireWait.WithLabelValues(bucketLabel(style), bucketLabel(scale)).Observe(waitSeconds)
	m.rendererPoolIdleWorkers.WithLabelValues(bucketLabel(style), bucketLabel(scale)).Set(float64(idleAfter))
}

// SetRendererPoolWorkers publishes a pool's current worker count and
// advances its high-water mark. highWater is tracked by the caller (the
// pool owns the lock that makes it consistent with the count).
func (m *MetricsManager) SetRendererPoolWorkers(style, scale string, workers, highWater int) {
	m.rendererPoolWorkers.WithLabelValues(bucketLabel(style), bucketLabel(scale)).Set(float64(workers))
	m.rendererPoolWorkersMax.WithLabelValues(bucketLabel(style), bucketLabel(scale)).Set(float64(highWater))
}

// RecordRendererPoolGrow / Shrink count elasticity events.
func (m *MetricsManager) RecordRendererPoolGrow(style, scale string) {
	m.rendererPoolGrow.WithLabelValues(bucketLabel(style), bucketLabel(scale)).Inc()
}

func (m *MetricsManager) RecordRendererPoolShrink(style, scale string) {
	m.rendererPoolShrink.WithLabelValues(bucketLabel(style), bucketLabel(scale)).Inc()
}

func (m *MetricsManager) RecordRendererWorkerReplacement(style, scale, reason string) {
	m.rendererWorkerReplacements.WithLabelValues(bucketLabel(style), bucketLabel(scale), reason).Inc()
}

// RecordRendererPumpBreakdown emits the per-render diagnostic
// histograms for the Go renderer's pump loop. Called once per render
// from GoPoolRenderer's pumpUntilStillFinished. Decomposes total
// render time so we can see what's actually contributing to it.
func (m *MetricsManager) RecordRendererPumpBreakdown(
	style, scale string,
	iterations int,
	sleepSeconds float64,
	timeToFirstEventSeconds float64,
	renderUpdateSeconds float64,
	renderUpdateCount int,
) {
	m.rendererPumpIterations.WithLabelValues(bucketLabel(style), bucketLabel(scale)).Observe(float64(iterations))
	m.rendererPumpSleep.WithLabelValues(bucketLabel(style), bucketLabel(scale)).Observe(sleepSeconds)
	m.rendererPumpTimeToFirstEv.WithLabelValues(bucketLabel(style), bucketLabel(scale)).Observe(timeToFirstEventSeconds)
	m.rendererPumpRenderUpdate.WithLabelValues(bucketLabel(style), bucketLabel(scale)).Observe(renderUpdateSeconds)
	m.rendererPumpRenderUpdateCnt.WithLabelValues(bucketLabel(style), bucketLabel(scale)).Observe(float64(renderUpdateCount))
}

// RecordRendererReadback emits the per-render readback histogram.
// Called once per render from renderOne, right around the
// sess.ReadPremultipliedRGBA8Into() call.
func (m *MetricsManager) RecordRendererReadback(style, scale string, seconds float64) {
	m.rendererReadback.WithLabelValues(bucketLabel(style), bucketLabel(scale)).Observe(seconds)
}

// SetRendererGlobalCapacity is called once at renderer init to expose
// the global concurrency cap (RENDERER_POOL_SIZE).
func (m *MetricsManager) SetRendererGlobalCapacity(capacity int) {
	m.rendererGlobalCapacity.Set(float64(capacity))
}

// RecordRendererGlobalAcquire is called once per successful semaphore
// acquire, passing how long the caller waited. Paired with
// DecRendererGlobalInFlight on release.
func (m *MetricsManager) RecordRendererGlobalAcquire(waitSeconds float64) {
	m.rendererGlobalAcquireWait.Observe(waitSeconds)
	m.rendererGlobalInFlight.Inc()

	m.inFlightMu.Lock()
	m.inFlight++
	if n := m.inFlight; float64(n) > m.peakInFlight {
		m.peakInFlight = float64(n)
		m.rendererGlobalInFlightMax.Set(m.peakInFlight)
	}
	m.inFlightMu.Unlock()
}

func (m *MetricsManager) DecRendererGlobalInFlight() {
	m.rendererGlobalInFlight.Dec()
	m.inFlightMu.Lock()
	if m.inFlight > 0 {
		m.inFlight--
	}
	m.inFlightMu.Unlock()
}

func (m *MetricsManager) RecordImageCacheHit(name string) {
	m.imageCacheHits.WithLabelValues(name).Inc()
}

func (m *MetricsManager) RecordImageCacheMiss(name string) {
	m.imageCacheMisses.WithLabelValues(name).Inc()
}

// RecordMarkerRequest records a marker request
func (m *MetricsManager) RecordMarkerRequest(domain string, cached bool) {
	cachedStr := "false"
	if cached {
		cachedStr = "true"
	}
	m.requestsTotal.WithLabelValues("marker", bucketLabel(domain), cachedStr).Inc()
	if cached {
		m.cacheHitsTotal.WithLabelValues("marker").Inc()
	} else {
		m.cacheMissTotal.WithLabelValues("marker").Inc()
	}
}

// RecordHTTPClientRequest records an HTTP client request
func (m *MetricsManager) RecordHTTPClientRequest(host string) {
	m.httpClientRequests.WithLabelValues(bucketLabel(host)).Inc()
}

// RecordHTTPClientError records an HTTP client error
func (m *MetricsManager) RecordHTTPClientError(host string) {
	m.httpClientErrors.WithLabelValues(bucketLabel(host)).Inc()
}

// RecordHTTPClientDuration records HTTP client request duration
func (m *MetricsManager) RecordHTTPClientDuration(host string, duration float64) {
	m.httpClientDuration.WithLabelValues(bucketLabel(host)).Observe(duration)
}

// RecordError records an error
func (m *MetricsManager) RecordError(errType, reason string) {
	m.errorsTotal.WithLabelValues(errType, reason).Inc()
	m.incrementDailyError("generic")
}

// incrementDailyError increments the daily error counter for a category
func (m *MetricsManager) incrementDailyError(category string) {
	today := time.Now().Format("2006-01-02")
	m.dailyErrorsMu.Lock()
	if m.dailyErrors[category] == nil {
		m.dailyErrors[category] = make(map[string]uint64)
	}
	m.dailyErrors[category][today]++
	m.dailyErrorsMu.Unlock()
}

// DailyErrorStat holds daily error count
type DailyErrorStat struct {
	Date  string
	Count uint64
}

// DailyErrorsByCategory holds error counts by category for last 7 days
type DailyErrorsByCategory struct {
	HTTP       []DailyErrorStat
	Template   []DailyErrorStat
	Validation []DailyErrorStat
	Generic    []DailyErrorStat
}

// GetDailyErrorsByCategory returns error counts for the last 14 days by category
func (m *MetricsManager) GetDailyErrorsByCategory() DailyErrorsByCategory {
	m.dailyErrorsMu.RLock()
	defer m.dailyErrorsMu.RUnlock()

	return DailyErrorsByCategory{
		HTTP:       m.getDailyStatsForCategory("http"),
		Template:   m.getDailyStatsForCategory("template"),
		Validation: m.getDailyStatsForCategory("validation"),
		Generic:    m.getDailyStatsForCategory("generic"),
	}
}

// cleanupOldDailyErrors removes daily error entries older than 14 days
func (m *MetricsManager) cleanupOldDailyErrors() {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().AddDate(0, 0, -14).Format("2006-01-02")
		m.dailyErrorsMu.Lock()
		for category, dates := range m.dailyErrors {
			for date := range dates {
				if date < cutoff {
					delete(m.dailyErrors[category], date)
				}
			}
		}
		m.dailyErrorsMu.Unlock()
	}
}

// getDailyStatsForCategory returns stats for a specific category (must hold read lock)
func (m *MetricsManager) getDailyStatsForCategory(category string) []DailyErrorStat {
	stats := make([]DailyErrorStat, 14)
	now := time.Now()
	categoryData := m.dailyErrors[category]

	for i := 13; i >= 0; i-- {
		date := now.AddDate(0, 0, -i)
		dateStr := date.Format("2006-01-02")
		count := uint64(0)
		if categoryData != nil {
			count = categoryData[dateStr]
		}
		stats[13-i] = DailyErrorStat{
			Date:  date.Format("02.01"),
			Count: count,
		}
	}
	return stats
}

// RecordHTTPError records an HTTP error response
func (m *MetricsManager) RecordHTTPError(handler string, statusCode int) {
	m.errorsTotal.WithLabelValues(handler, fmt.Sprintf("http_%d", statusCode)).Inc()
	m.incrementDailyError("http")
}

// RecordTemplateError records a template rendering error
func (m *MetricsManager) RecordTemplateError(templateName, reason string) {
	m.errorsTotal.WithLabelValues("template_"+bucketLabel(templateName), reason).Inc()
	m.incrementDailyError("template")
}

// RecordValidationError records a validation error
func (m *MetricsManager) RecordValidationError(handler, field string) {
	m.errorsTotal.WithLabelValues(handler, "validation_"+field).Inc()
	m.incrementDailyError("validation")
}

// SetFileToucherQueueSize sets the file toucher queue size
func (m *MetricsManager) SetFileToucherQueueSize(size int) {
	m.fileToucherQueueSize.Set(float64(size))
}

// SetFileRemoverQueueSize sets the file remover queue size for a folder
func (m *MetricsManager) SetFileRemoverQueueSize(folder string, size int) {
	m.fileRemoverQueueSize.WithLabelValues(folder).Set(float64(size))
}

// RecordTemplateRender records a template render
func (m *MetricsManager) RecordTemplateRender(templateName, method, reqType string) {
	m.templateRendersTotal.WithLabelValues(bucketLabel(templateName), method, reqType).Inc()
}

// SetCacheSize sets the cache size for a folder
func (m *MetricsManager) SetCacheSize(folder string, sizeBytes uint64) {
	m.cacheSizeBytes.WithLabelValues(folder).Set(float64(sizeBytes))
}

// SetDatasetSize sets the size for a dataset
func (m *MetricsManager) SetDatasetSize(name string, sizeBytes uint64) {
	m.datasetSizeBytes.WithLabelValues(name).Set(float64(sizeBytes))
}

// DeleteDatasetSize removes a dataset from the metrics
func (m *MetricsManager) DeleteDatasetSize(name string) {
	m.datasetSizeBytes.DeleteLabelValues(name)
}

// DatasetSizeStat holds dataset size statistics
type DatasetSizeStat struct {
	Name string
	Size uint64
}

// GetDatasetSizes returns dataset size statistics
func (m *MetricsManager) GetDatasetSizes() map[string]uint64 {
	sizes := make(map[string]uint64)
	ch := make(chan prometheus.Metric, 100)
	go func() {
		m.datasetSizeBytes.Collect(ch)
		close(ch)
	}()

	for metric := range ch {
		var dto prommodel.Metric
		if err := metric.Write(&dto); err != nil {
			continue
		}
		var name string
		for _, label := range dto.GetLabel() {
			if label.GetName() == "name" {
				name = label.GetValue()
			}
		}
		if dto.GetGauge() != nil {
			sizes[name] = uint64(dto.GetGauge().GetValue())
		}
	}
	return sizes
}

// CacheSizeStat holds cache size statistics
type CacheSizeStat struct {
	Folder string
	Size   uint64
}

// GetCacheSizes returns cache size statistics
func (m *MetricsManager) GetCacheSizes() []CacheSizeStat {
	var stats []CacheSizeStat
	ch := make(chan prometheus.Metric, 100)
	go func() {
		m.cacheSizeBytes.Collect(ch)
		close(ch)
	}()

	for metric := range ch {
		var dto prommodel.Metric
		if err := metric.Write(&dto); err != nil {
			continue
		}
		var folder string
		for _, label := range dto.GetLabel() {
			if label.GetName() == "folder" {
				folder = label.GetValue()
			}
		}
		if dto.GetGauge() != nil {
			stats = append(stats, CacheSizeStat{
				Folder: folder,
				Size:   uint64(dto.GetGauge().GetValue()),
			})
		}
	}
	return stats
}

// GetMemoryInfo returns current memory statistics
func (m *MetricsManager) GetMemoryInfo() *MemoryInfo {
	if info := readSmapsRollup(); info != nil {
		return info
	}
	// Fallback to Go runtime stats
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	return &MemoryInfo{
		RSS: memStats.Alloc,
		VSS: memStats.Sys,
	}
}

// GetUptime returns the process uptime
func (m *MetricsManager) GetUptime() time.Duration {
	return time.Since(m.startTime)
}

// TemplateRenderStat holds template render statistics
type TemplateRenderStat struct {
	Template string
	Method   string
	Type     string
	Count    uint64
}

// GetTemplateRenderStats returns template render statistics
func (m *MetricsManager) GetTemplateRenderStats() []TemplateRenderStat {
	var stats []TemplateRenderStat
	ch := make(chan prometheus.Metric, 100)
	go func() {
		m.templateRendersTotal.Collect(ch)
		close(ch)
	}()

	for metric := range ch {
		pm := metric
		var dto prommodel.Metric
		if err := pm.Write(&dto); err != nil {
			continue
		}
		var template, method, reqType string
		for _, label := range dto.GetLabel() {
			switch label.GetName() {
			case "template":
				template = label.GetValue()
			case "method":
				method = label.GetValue()
			case "type":
				reqType = label.GetValue()
			}
		}
		if dto.GetCounter() != nil {
			stats = append(stats, TemplateRenderStat{
				Template: template,
				Method:   method,
				Type:     reqType,
				Count:    uint64(dto.GetCounter().GetValue()),
			})
		}
	}
	return stats
}
