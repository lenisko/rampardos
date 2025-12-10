package services

import (
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// MetricsManager handles all Prometheus metrics
type MetricsManager struct {
	startTime time.Time

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
	uptimeSeconds       prometheus.Gauge
	memoryResidentBytes prometheus.Gauge
	memoryVirtualBytes  prometheus.Gauge

	// Queue metrics
	fileToucherQueueSize prometheus.Gauge
	fileRemoverQueueSize *prometheus.GaugeVec

	// Task metrics
	activeTasks    *prometheus.GaugeVec
	tasksStarted   *prometheus.CounterVec
	tasksCompleted *prometheus.CounterVec

	// Template metrics
	templateRendersTotal *prometheus.CounterVec
}

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
		}, []string{"type", "cached"}),

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

		memoryResidentBytes: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "rampardos_memory_resident_bytes",
			Help: "Resident memory size in bytes",
		}),

		memoryVirtualBytes: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "rampardos_memory_virtual_bytes",
			Help: "Virtual memory size in bytes",
		}),

		fileToucherQueueSize: promauto.NewGauge(prometheus.GaugeOpts{
			Name: "rampardos_filetoucher_queue_size",
			Help: "Size of the file toucher queue",
		}),

		fileRemoverQueueSize: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "rampardos_fileremover_queue_size",
			Help: "Size of the file remover queue (files pending removal)",
		}, []string{"folder"}),

		activeTasks: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "rampardos_active_tasks",
			Help: "Number of active tasks",
		}, []string{"type"}),

		tasksStarted: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "rampardos_tasks_started_total",
			Help: "Total tasks started",
		}, []string{"type"}),

		tasksCompleted: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "rampardos_tasks_completed_total",
			Help: "Total tasks completed",
		}, []string{"type"}),

		templateRendersTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Name: "rampardos_template_renders_total",
			Help: "Total template renders",
		}, []string{"template", "method", "type"}),
	}

	// Start runtime metrics updater
	go m.updateRuntimeMetrics()

	return m
}

func (m *MetricsManager) updateRuntimeMetrics() {
	ticker := time.NewTicker(15 * time.Second)
	for range ticker.C {
		m.uptimeSeconds.Set(time.Since(m.startTime).Seconds())

		var memStats runtime.MemStats
		runtime.ReadMemStats(&memStats)
		m.memoryResidentBytes.Set(float64(memStats.Alloc))
		m.memoryVirtualBytes.Set(float64(memStats.Sys))
	}
}

// RecordRequest records a request with type, cache status, and duration
func (m *MetricsManager) RecordRequest(reqType, style string, cached bool, duration float64) {
	cachedStr := "false"
	if cached {
		cachedStr = "true"
	}

	m.requestsTotal.WithLabelValues(reqType, style, cachedStr).Inc()

	if cached {
		m.cacheHitsTotal.WithLabelValues(reqType).Inc()
	} else {
		m.cacheMissTotal.WithLabelValues(reqType).Inc()
	}

	m.requestDuration.WithLabelValues(reqType, cachedStr).Observe(duration)
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
	m.requestsTotal.WithLabelValues("tile", style, cachedStr).Inc()
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
	m.requestsTotal.WithLabelValues("staticmap", style, cachedStr).Inc()
	if cached {
		m.cacheHitsTotal.WithLabelValues("staticmap").Inc()
	} else {
		m.cacheMissTotal.WithLabelValues("staticmap").Inc()
	}
}

// RecordMarkerRequest records a marker request
func (m *MetricsManager) RecordMarkerRequest(domain string, cached bool) {
	cachedStr := "false"
	if cached {
		cachedStr = "true"
	}
	m.requestsTotal.WithLabelValues("marker", domain, cachedStr).Inc()
	if cached {
		m.cacheHitsTotal.WithLabelValues("marker").Inc()
	} else {
		m.cacheMissTotal.WithLabelValues("marker").Inc()
	}
}

// RecordHTTPClientRequest records an HTTP client request
func (m *MetricsManager) RecordHTTPClientRequest(host string) {
	m.httpClientRequests.WithLabelValues(host).Inc()
}

// RecordHTTPClientError records an HTTP client error
func (m *MetricsManager) RecordHTTPClientError(host string) {
	m.httpClientErrors.WithLabelValues(host).Inc()
}

// RecordHTTPClientDuration records HTTP client request duration
func (m *MetricsManager) RecordHTTPClientDuration(host string, duration float64) {
	m.httpClientDuration.WithLabelValues(host).Observe(duration)
}

// RecordError records an error
func (m *MetricsManager) RecordError(errType, reason string) {
	m.errorsTotal.WithLabelValues(errType, reason).Inc()
}

// RecordHTTPError records an HTTP error response
func (m *MetricsManager) RecordHTTPError(handler string, statusCode int) {
	m.errorsTotal.WithLabelValues(handler, fmt.Sprintf("http_%d", statusCode)).Inc()
}

// RecordTemplateError records a template rendering error
func (m *MetricsManager) RecordTemplateError(templateName, reason string) {
	m.errorsTotal.WithLabelValues("template_"+templateName, reason).Inc()
}

// RecordValidationError records a validation error
func (m *MetricsManager) RecordValidationError(handler, field string) {
	m.errorsTotal.WithLabelValues(handler, "validation_"+field).Inc()
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
	m.templateRendersTotal.WithLabelValues(templateName, method, reqType).Inc()
}
