package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"

	"github.com/lenisko/rampardos/internal/config"
	"github.com/lenisko/rampardos/internal/handlers"
	"github.com/lenisko/rampardos/internal/handlers/views"
	custommw "github.com/lenisko/rampardos/internal/middleware"
	"github.com/lenisko/rampardos/internal/models"
	"github.com/lenisko/rampardos/internal/services"
	"github.com/lenisko/rampardos/internal/utils"
)

func main() {
	// Load configuration first
	cfg := config.Load()

	// Initialize runtime settings (includes logging setup)
	services.InitGlobalRuntimeSettings(false)
	if cfg.TileServerURL == "" {
		slog.Error("TILE_SERVER_URL environment not set. Exiting...")
		os.Exit(1)
	}

	slog.Info("Configuration loaded", "timeout", cfg.RequestTimeout, "debug", false)

	// Initialize Pyroscope profiling if configured
	services.InitPyroscope(cfg)

	// Parse external styles from environment
	var externalStyles []models.Style
	for _, env := range os.Environ() {
		if strings.HasPrefix(env, "TILE_URL_") {
			parts := strings.SplitN(env, "=", 2)
			if len(parts) != 2 {
				continue
			}
			key := strings.TrimPrefix(parts[0], "TILE_URL_")
			key = strings.ReplaceAll(key, " ", "")
			key = strings.ToLower(key)

			external := true
			externalStyles = append(externalStyles, models.Style{
				ID:       strings.ReplaceAll(key, "_", "-"),
				Name:     cases.Title(language.English).String(strings.ReplaceAll(key, "_", " ")),
				External: &external,
				URL:      parts[1],
			})
		}
	}

	// Initialize metrics
	services.InitMetrics()

	// Initialize HTTP service
	services.InitHTTPService(cfg)

	// Initialize services
	fileToucher := services.NewFileToucher()
	fileToucher.Start()

	statsController := services.NewStatsController(fileToucher)
	fontsController := services.NewFontsController("TileServer/Fonts", "Temp")
	stylesController := services.NewStylesController(cfg.TileServerURL, externalStyles, "TileServer/Styles", fontsController)

	// Create tileserver reload function (sends SIGHUP to docker container)
	var reloadTileserver func() error
	if cfg.TileServerContainer != "" {
		containerName := cfg.TileServerContainer
		reloadTileserver = func() error {
			cmd := exec.Command("docker", "kill", "-s", "HUP", containerName)
			output, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("docker kill failed: %s - %w", string(output), err)
			}
			return nil
		}
		slog.Info("Tileserver reload enabled", "container", cfg.TileServerContainer)
	}

	datasetsController := services.NewDatasetsController("TileServer/Datasets", reloadTileserver)
	templatesController := services.NewTemplatesController("Templates")

	// Initialize Jet template renderer (loads templates into memory)
	if err := services.InitGlobalJetRenderer("Templates"); err != nil {
		slog.Error("Failed to initialize Jet renderer", "error", err)
		os.Exit(1)
	}

	// Initialize cache index for fast cache lookups
	services.InitGlobalCacheIndex()

	// Set graphics engine mode
	utils.UseLegacyGraphicsEngine = cfg.LegacyGraphicsEngine
	if cfg.LegacyGraphicsEngine {
		slog.Info("Using legacy ImageMagick graphics engine")
	} else {
		slog.Info("Using native Go graphics engine")
	}

	// Initialize cache cleaners
	initCacheCleaners(cfg)

	// Initialize handlers
	tileHandler := handlers.NewTileHandler(cfg.TileServerURL, statsController, stylesController)
	staticMapHandler := handlers.NewStaticMapHandler(cfg.TileServerURL, tileHandler, statsController, stylesController)
	multiStaticMapHandler := handlers.NewMultiStaticMapHandler(staticMapHandler, statsController)
	stylesHandler := handlers.NewStylesHandler(stylesController)
	fontsHandler := handlers.NewFontsHandler(fontsController)
	datasetsHandler := handlers.NewDatasetsHandler(datasetsController)
	templatesHandler := handlers.NewTemplatesHandler(templatesController, staticMapHandler, multiStaticMapHandler)

	// Initialize template renderer
	templateRenderer, err := views.NewTemplateRendererFromDir("internal/templates")
	if err != nil {
		slog.Error("Failed to load templates", "error", err)
		os.Exit(1)
	}

	// Initialize services
	openFreeMapService := services.NewOpenFreeMapService()

	// Initialize view handlers
	statsView := views.NewStatsView(statsController, templateRenderer)
	datasetsView := views.NewDatasetsView(datasetsController, openFreeMapService, datasetsHandler.GetDownloadManager(), templateRenderer)
	fontsView := views.NewFontsView(fontsController, templateRenderer)
	stylesView := views.NewStylesView(stylesController, templateRenderer)
	templatesView := views.NewTemplatesView(templatesController, templateRenderer)
	convertView := views.NewConvertView(templateRenderer)

	// Initialize settings handler
	settingsHandler := handlers.NewSettingsHandler()

	// Initialize convert handler
	convertHandler := handlers.NewConvertHandler()

	// Create router
	r := chi.NewRouter()

	// Middleware
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.RealIP)
	r.Use(custommw.Timeout(cfg.RequestTimeout))
	r.Use(custommw.DebugRequestBody())

	// Public routes
	r.Get("/styles", stylesHandler.Get)
	r.Get("/tile/{style}/{z}/{x}/{y}/{scale}/{format}", tileHandler.Get)

	r.Get("/staticmap", staticMapHandler.Get)
	r.Post("/staticmap", staticMapHandler.Post)
	r.Get("/staticmap/{template}", staticMapHandler.GetTemplate)
	r.Post("/staticmap/{template}", staticMapHandler.PostTemplate)
	r.Get("/staticmap/pregenerated/{id}", staticMapHandler.GetPregenerated)

	r.Get("/multistaticmap", multiStaticMapHandler.Get)
	r.Post("/multistaticmap", multiStaticMapHandler.Post)
	r.Get("/multistaticmap/{template}", multiStaticMapHandler.GetTemplate)
	r.Post("/multistaticmap/{template}", multiStaticMapHandler.PostTemplate)
	r.Get("/multistaticmap/pregenerated/{id}", multiStaticMapHandler.GetPregenerated)

	// Metrics endpoint
	r.Handle("/metrics", promhttp.Handler())

	// Admin routes (protected by Basic Auth)
	r.Route("/admin", func(r chi.Router) {
		r.Use(custommw.AdminAuth())

		// Admin views
		r.Get("/", func(w http.ResponseWriter, req *http.Request) {
			http.Redirect(w, req, "/admin/stats", http.StatusFound)
		})
		r.Get("/stats", statsView.Render)

		r.Get("/datasets", datasetsView.Render)
		r.Get("/datasets/add", datasetsView.RenderAdd)
		r.Get("/datasets/delete/{name}", datasetsView.RenderDelete)

		r.Get("/fonts", fontsView.Render)
		r.Get("/fonts/add", fontsView.RenderAdd)

		r.Get("/styles", stylesView.Render)
		r.Get("/styles/external/add", stylesView.RenderAddExternal)
		r.Get("/styles/local/add", stylesView.RenderAddLocal)
		r.Get("/styles/local/delete/{id}", stylesView.RenderDeleteLocal)

		r.Get("/templates", templatesView.Render)
		r.Get("/templates/add", templatesView.RenderAdd)
		r.Get("/templates/edit/{name}", templatesView.RenderEdit)

		r.Get("/convert", convertView.Render)

		// Admin API
		r.Route("/api", func(r chi.Router) {
			// Datasets
			r.Get("/datasets/add", datasetsHandler.Download)  // WebSocket
			r.Get("/datasets/delete", datasetsHandler.Delete) // WebSocket
			r.Post("/datasets/add", datasetsHandler.Add)
			r.Get("/datasets/downloads", datasetsHandler.GetDownloadStatus)
			r.Post("/datasets/downloads/{name}/clear", datasetsHandler.ClearDownload)
			r.Post("/datasets/downloads/{name}/cancel", datasetsHandler.CancelDownload)
			r.Post("/datasets/combine", datasetsHandler.Combine)
			r.Post("/datasets/{name}/activate", datasetsHandler.SetActive)
			r.Post("/datasets/reload-tileserver", datasetsHandler.ReloadTileserver)

			// Fonts
			r.Post("/fonts/add", fontsHandler.Add)
			r.Delete("/fonts/delete/{name}", fontsHandler.Delete)
			r.Get("/fonts/file/{name}", fontsHandler.GetFile)

			// Styles
			r.Post("/styles/external/add", stylesHandler.AddExternal)
			r.Delete("/styles/external/{id}", stylesHandler.DeleteExternal)
			r.Post("/styles/local/add", stylesHandler.AddLocal)
			r.Delete("/styles/local/{id}", stylesHandler.DeleteLocal)

			// Templates
			r.Post("/templates/preview", templatesHandler.Preview)
			r.Post("/templates/save", templatesHandler.Save)
			r.Post("/templates/testdata", templatesHandler.SaveTestData)
			r.Delete("/templates/delete/{name}", templatesHandler.Delete)

			// Convert
			r.Post("/convert/leaf-to-jet", convertHandler.LeafToJet)

			// Settings
			r.Get("/settings/debug", settingsHandler.GetDebugStatus)
			r.Post("/settings/debug/toggle", settingsHandler.ToggleDebug)
		})
	})

	// Favicon - return 204 No Content
	r.Get("/favicon.ico", func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	// Root redirect
	r.Get("/", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/admin/stats", http.StatusFound)
	})

	// Start server
	addr := cfg.Hostname + ":" + cfg.Port
	slog.Info("Starting server", "address", addr)
	if err := http.ListenAndServe(addr, r); err != nil {
		slog.Error("Server failed", "error", err)
		os.Exit(1)
	}
}

func initCacheCleaners(cfg *config.Config) {
	// Tile cache
	if cfg.TileCacheMaxAge != nil && cfg.TileCacheDelay != nil {
		cleaner := services.NewCacheCleaner("Cache/Tile", cfg.TileCacheMaxAge, cfg.TileCacheDelay)
		cleaner.Start()
	}

	// Static map cache
	if cfg.StaticCacheMaxAge != nil && cfg.StaticCacheDelay != nil {
		cleaner := services.NewCacheCleaner("Cache/Static", cfg.StaticCacheMaxAge, cfg.StaticCacheDelay)
		cleaner.Start()
	}

	// Multi static map cache
	if cfg.MultiStaticMaxAge != nil && cfg.MultiStaticDelay != nil {
		cleaner := services.NewCacheCleaner("Cache/StaticMulti", cfg.MultiStaticMaxAge, cfg.MultiStaticDelay)
		cleaner.Start()
	}

	// Marker cache
	if cfg.MarkerCacheMaxAge != nil && cfg.MarkerCacheDelay != nil {
		cleaner := services.NewCacheCleaner("Cache/Marker", cfg.MarkerCacheMaxAge, cfg.MarkerCacheDelay)
		cleaner.Start()
	}

	// Regeneratable cache
	if cfg.RegenCacheMaxAge != nil && cfg.RegenCacheDelay != nil {
		cleaner := services.NewCacheCleaner("Cache/Regeneratable", cfg.RegenCacheMaxAge, cfg.RegenCacheDelay)
		cleaner.Start()
	}
}
