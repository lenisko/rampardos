package services

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	prommodel "github.com/prometheus/client_model/go"
)

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

	// Task metrics
	activeTasks    *prometheus.GaugeVec
	tasksStarted   *prometheus.CounterVec
	tasksCompleted *prometheus.CounterVec

	// Template metrics
	templateRendersTotal *prometheus.CounterVec

	// Cache size metrics
	cacheSizeBytes *prometheus.GaugeVec

	// Dataset size metrics
	datasetSizeBytes *prometheus.GaugeVec

	// Tileserver metrics
	tileserverRestarts prometheus.Counter
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

		cacheSizeBytes: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "rampardos_cache_size_bytes",
			Help: "Size of cache directories in bytes",
		}, []string{"folder"}),

		datasetSizeBytes: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Name: "rampardos_dataset_size_bytes",
			Help: "Size of dataset files in bytes",
		}, []string{"name"}),

		tileserverRestarts: promauto.NewCounter(prometheus.CounterOpts{
			Name: "rampardos_tileserver_restarts_total",
			Help: "Total number of tileserver restarts triggered by health check failures",
		}),
	}

	// Start runtime metrics updater
	go m.updateRuntimeMetrics()

	// Start daily error cleanup
	go m.cleanupOldDailyErrors()

	return m
}

func (m *MetricsManager) updateRuntimeMetrics() {
	ticker := time.NewTicker(15 * time.Second)
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
	m.errorsTotal.WithLabelValues("template_"+templateName, reason).Inc()
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
	m.templateRendersTotal.WithLabelValues(templateName, method, reqType).Inc()
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

// IncTileserverRestarts increments the tileserver restart counter
func (m *MetricsManager) IncTileserverRestarts() {
	m.tileserverRestarts.Inc()
}

// GetTileserverRestarts returns the total number of tileserver restarts
func (m *MetricsManager) GetTileserverRestarts() uint64 {
	var dto prommodel.Metric
	if err := m.tileserverRestarts.Write(&dto); err != nil {
		return 0
	}
	if dto.GetCounter() != nil {
		return uint64(dto.GetCounter().GetValue())
	}
	return 0
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
		var pm prometheus.Metric = metric
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
