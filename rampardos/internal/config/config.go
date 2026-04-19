package config

import (
	"os"
	"strconv"
	"time"
)

// Config holds application configuration
type Config struct {
	Port           string
	Hostname       string
	RequestTimeout time.Duration

	// Renderer settings
	RendererBackend        string        // "node-pool"
	RendererNodeBinary     string        // path to node
	RendererWorkerScript   string        // path to render-worker.js
	RendererPoolSize       int           // concurrent workers per style
	RendererRenderTimeout  time.Duration // per-render deadline
	RendererWorkerLifetime int           // renders per worker before recycle
	RendererStartupTimeout time.Duration // max handshake wait

	// HTTP client settings
	HTTPMaxConns  int
	HTTPTimeout   time.Duration // General HTTP timeout (0 = unlimited)

	// Cache settings
	TileCacheMaxAge      *uint32
	TileCacheDelay       *uint32
	TileCacheDropAfter   *uint32
	StaticCacheMaxAge    *uint32
	StaticCacheDelay     *uint32
	StaticCacheDropAfter *uint32
	MultiStaticMaxAge    *uint32
	MultiStaticDelay     *uint32
	MultiStaticDropAfter *uint32
	MarkerCacheMaxAge    *uint32
	MarkerCacheDelay     *uint32
	MarkerCacheDropAfter *uint32
	RegenCacheMaxAge     *uint32
	RegenCacheDelay      *uint32
	RegenCacheDropAfter  *uint32

	// Image processing settings
	DefaultImageFormat   string // "png", "jpeg", or "webp" (default: png)
	OverrideClientFormat bool   // If true, ignore client format and use DefaultImageFormat
	PNGCompressionLevel  string // "fast", "default", "best", or "none" (default: fast — flate level 9 is ~4-6x slower for ~25% size saving on map tiles)
	ImageQuality         int    // JPEG/WebP quality 1-100 (default: 90)
	MarkerImageCacheSize int    // Max resized marker images to cache (default: 2000). Entry size varies by marker dimensions; a typical 40×40 NRGBA ≈ 6.4 KB; 2000 ≈ 12 MB.
	TileImageCacheSize   int    // Max decoded tile images to cache in memory (default: 500, 0 disables). Each entry is ~256 KB (256x256 NRGBA); 500 ≈ 128 MB.
	CompositeImageCacheSize int // Max base+staticmap images cached in memory (default: 200, 0 disables). Each entry ~640 KB for 400×400 NRGBA; 200 ≈ 128 MB.

	// Experimental features
	ExperimentalGSat bool // Enable Google Satellite external style

	// LocalStylesUseViewport bypasses tile stitching for local-style
	// integer-zoom static maps, sending a single RenderViewport call
	// to the Node renderer instead. Avoids the tile caching layer
	// entirely for local styles — valuable when the tile working set
	// exceeds the RAM LRU and most stitches decode-and-discard.
	// External styles always tile-stitch (upstream providers can't
	// render an arbitrary viewport).
	LocalStylesUseViewport bool

	// Diagnostics
	PprofEnabled bool // Mount net/http/pprof under /debug/pprof. Off by default — endpoints expose profiling data and pprof.Profile holds a goroutine for 30s per call.

	// Pyroscope settings
	PyroscopeServerAddress        string
	PyroscopeApplicationName      string
	PyroscopeMutexProfileFraction int
	PyroscopeBlockProfileRate     int
	PyroscopeLogger               bool
	PyroscopeApiKey               string
	PyroscopeBasicAuthUser        string
	PyroscopeBasicAuthPassword    string
}

// Load loads configuration from environment variables
func Load() *Config {
	cfg := &Config{
		Port:           getEnv("PORT", "9000"),
		Hostname:       getEnv("HOSTNAME", "0.0.0.0"),
		RequestTimeout: getEnvDuration("REQUEST_TIMEOUT", 10*time.Second),

		RendererBackend:        getEnv("RENDERER_BACKEND", "node-pool"),
		RendererNodeBinary:     getEnv("RENDERER_NODE_BINARY", "node"),
		RendererWorkerScript:   getEnv("RENDERER_WORKER_SCRIPT", "/app/render-worker/render-worker.js"),
		RendererPoolSize:       getEnvInt("RENDERER_POOL_SIZE", 0),
		RendererRenderTimeout:  getEnvSeconds("RENDERER_TIMEOUT_SECONDS", 15),
		RendererWorkerLifetime: getEnvInt("RENDERER_WORKER_LIFETIME", 500),
		RendererStartupTimeout: getEnvSeconds("RENDERER_STARTUP_TIMEOUT_SECONDS", 10),

		HTTPMaxConns: getEnvInt("HTTP_MAX_CONNS", 100),
		HTTPTimeout:  getEnvSeconds("HTTP_TIMEOUT_SECONDS", 15), // 0 = unlimited

		TileCacheMaxAge:      getEnvUint32("TILE_CACHE_MAX_AGE_MINUTES", 10080),
		TileCacheDelay:       getEnvUint32("TILE_CACHE_DELAY_SECONDS", 3600),
		TileCacheDropAfter:   getEnvUint32("TILE_CACHE_DROP_AFTER_MINUTES", 129600),
		StaticCacheMaxAge:    getEnvUint32("STATIC_CACHE_MAX_AGE_MINUTES", 10080),
		StaticCacheDelay:     getEnvUint32("STATIC_CACHE_DELAY_SECONDS", 3600),
		StaticCacheDropAfter: getEnvUint32("STATIC_CACHE_DROP_AFTER_MINUTES", 129600),
		MultiStaticMaxAge:    getEnvUint32("STATIC_MULTI_CACHE_MAX_AGE_MINUTES", 60),
		MultiStaticDelay:     getEnvUint32("STATIC_MULTI_CACHE_DELAY_SECONDS", 900),
		MultiStaticDropAfter: getEnvUint32("STATIC_MULTI_CACHE_DROP_AFTER_MINUTES", 129600),
		MarkerCacheMaxAge:    getEnvUint32("MARKER_CACHE_MAX_AGE_MINUTES", 1440),
		MarkerCacheDelay:     getEnvUint32("MARKER_CACHE_DELAY_SECONDS", 3600),
		MarkerCacheDropAfter: getEnvUint32("MARKER_CACHE_DROP_AFTER_MINUTES", 129600),
		RegenCacheMaxAge:     getEnvUint32("REGENERATABLE_CACHE_MAX_AGE_MINUTES", 10080),
		RegenCacheDelay:      getEnvUint32("REGENERATABLE_CACHE_DELAY_SECONDS", 3600),
		RegenCacheDropAfter:  getEnvUint32("REGENERATABLE_CACHE_DROP_AFTER_MINUTES", 129600),

		DefaultImageFormat:   getEnv("DEFAULT_IMAGE_FORMAT", "png"),
		OverrideClientFormat: getEnvBool("OVERRIDE_CLIENT_FORMAT", false),
		PNGCompressionLevel:  getEnv("PNG_COMPRESSION_LEVEL", "fast"),
		ImageQuality:         getEnvInt("IMAGE_QUALITY", 90),
		MarkerImageCacheSize: getEnvInt("MARKER_IMAGE_CACHE_SIZE", 2000),
		TileImageCacheSize:   getEnvInt("TILE_IMAGE_CACHE_SIZE", 500),
		CompositeImageCacheSize: getEnvInt("COMPOSITE_IMAGE_CACHE_SIZE", 200),

		ExperimentalGSat: getEnvBool("EXPERIMENTAL_G_SAT", true),

		LocalStylesUseViewport: getEnvBool("LOCAL_STYLES_USE_VIEWPORT", true),

		PprofEnabled: getEnvBool("PPROF_ENABLED", false),

		PyroscopeServerAddress:        getEnv("PYROSCOPE_SERVER_ADDRESS", ""),
		PyroscopeApplicationName:      getEnv("PYROSCOPE_APPLICATION_NAME", "tileserver-cache"),
		PyroscopeMutexProfileFraction: getEnvInt("PYROSCOPE_MUTEX_PROFILE_FRACTION", 5),
		PyroscopeBlockProfileRate:     getEnvInt("PYROSCOPE_BLOCK_PROFILE_RATE", 5),
		PyroscopeLogger:               getEnvBool("PYROSCOPE_LOGGER", false),
		PyroscopeApiKey:               getEnv("PYROSCOPE_API_KEY", ""),
		PyroscopeBasicAuthUser:        getEnv("PYROSCOPE_BASIC_AUTH_USER", ""),
		PyroscopeBasicAuthPassword:    getEnv("PYROSCOPE_BASIC_AUTH_PASSWORD", ""),
	}

	return cfg
}

func getEnv(key, defaultValue string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultValue
}

func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	val := os.Getenv(key)
	if val == "" {
		return defaultValue
	}

	// Try parsing as duration string (e.g., "30s", "1m")
	if d, err := time.ParseDuration(val); err == nil {
		return d
	}

	// Try parsing as seconds integer
	if secs, err := strconv.Atoi(val); err == nil {
		return time.Duration(secs) * time.Second
	}

	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	val := os.Getenv(key)
	if val == "" {
		return defaultValue
	}
	return val == "true" || val == "1" || val == "yes"
}

func getEnvUint32(key string, defaultValue ...uint32) *uint32 {
	val := os.Getenv(key)
	if val == "" {
		if len(defaultValue) > 0 {
			return &defaultValue[0]
		}
		return nil
	}
	i, err := strconv.ParseUint(val, 10, 32)
	if err != nil {
		return nil
	}
	u := uint32(i)
	return &u
}

func getEnvInt(key string, defaultValue int) int {
	val := os.Getenv(key)
	if val == "" {
		return defaultValue
	}
	i, err := strconv.Atoi(val)
	if err != nil {
		return defaultValue
	}
	return i
}

func getEnvSeconds(key string, defaultValue int) time.Duration {
	val := os.Getenv(key)
	if val == "" {
		return time.Duration(defaultValue) * time.Second
	}
	secs, err := strconv.Atoi(val)
	if err != nil {
		return time.Duration(defaultValue) * time.Second
	}
	return time.Duration(secs) * time.Second
}
