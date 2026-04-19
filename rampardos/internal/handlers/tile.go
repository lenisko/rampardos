package handlers

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/go-chi/chi/v5"
	"github.com/lenisko/rampardos/internal/fileutil"
	"github.com/lenisko/rampardos/internal/models"
	"github.com/lenisko/rampardos/internal/services"
	"github.com/lenisko/rampardos/internal/services/renderer"
)

// TileHandler handles tile requests
type TileHandler struct {
	renderer         renderer.Renderer
	statsController  *services.StatsController
	stylesController stylesControllerGetExternal
	sfg              singleflight.Group // dedup concurrent tile fetches
}

// NewTileHandler creates a new tile handler
func NewTileHandler(r renderer.Renderer, statsController *services.StatsController, stylesController *services.StylesController) *TileHandler {
	return &TileHandler{
		renderer:         r,
		statsController:  statsController,
		stylesController: stylesController,
	}
}

// TileResult contains the result of tile generation
type TileResult struct {
	Path   string
	Cached bool
}

// Get handles GET /tile/:style/:z/:x/:y/:scale/:format
func (h *TileHandler) Get(w http.ResponseWriter, r *http.Request) {
	style := chi.URLParam(r, "style")
	zStr := chi.URLParam(r, "z")
	xStr := chi.URLParam(r, "x")
	yStr := chi.URLParam(r, "y")
	scaleStr := chi.URLParam(r, "scale")
	formatStr := chi.URLParam(r, "format")

	z, err := strconv.Atoi(zStr)
	if err != nil {
		http.Error(w, "Invalid z parameter", http.StatusBadRequest)
		return
	}

	x, err := strconv.Atoi(xStr)
	if err != nil {
		http.Error(w, "Invalid x parameter", http.StatusBadRequest)
		return
	}

	y, err := strconv.Atoi(yStr)
	if err != nil {
		http.Error(w, "Invalid y parameter", http.StatusBadRequest)
		return
	}

	scale, err := strconv.ParseUint(scaleStr, 10, 8)
	if err != nil || scale < 1 {
		http.Error(w, "Invalid scale parameter", http.StatusBadRequest)
		return
	}

	format := models.ImageFormat(formatStr)
	if !format.IsValid() {
		http.Error(w, "Invalid format parameter", http.StatusBadRequest)
		return
	}

	h.generateTileAndResponse(w, r, style, z, x, y, uint8(scale), format)
}

// GenerateTile generates a tile and returns the result. Concurrent
// requests for the same tile are deduplicated via singleflight: only
// one goroutine downloads/renders, the rest wait for its result.
func (h *TileHandler) GenerateTile(ctx context.Context, style string, z, x, y int, scale uint8, format models.ImageFormat) (*TileResult, error) {
	path := fmt.Sprintf("Cache/Tile/%s-%d-%d-%d-%d.%s", style, z, x, y, scale, format)

	start := time.Now()
	if _, err := os.Stat(path); err == nil {
		h.statsController.TileServed(false, path, style)
		services.GlobalMetrics.RecordTileGenerate(style, services.TileSourceCache, time.Since(start).Seconds())
		return &TileResult{Path: path, Cached: true}, nil
	}

	// singleflight: only one goroutine generates a given tile at a time.
	// Others wait and get the same result (or the same error). The
	// leader times its own work inside the callback; followers
	// piggyback on the leader's result and do not double-count.
	var sfSource string
	var sfStart time.Time
	_, err, _ := h.sfg.Do(path, func() (any, error) {
		// Double-check cache inside singleflight (another request may
		// have populated it between our outer check and acquiring the
		// singleflight slot).
		if _, err := os.Stat(path); err == nil {
			return nil, nil
		}

		sfStart = time.Now()

		if extStyle := h.stylesController.GetExternalStyle(style); extStyle != nil {
			scaleString := ""
			if scale != 1 {
				scaleString = fmt.Sprintf("@%dx", scale)
			}
			tileURL := extStyle.URL
			tileURL = strings.ReplaceAll(tileURL, "{z}", strconv.Itoa(z))
			tileURL = strings.ReplaceAll(tileURL, "{x}", strconv.Itoa(x))
			tileURL = strings.ReplaceAll(tileURL, "{y}", strconv.Itoa(y))
			tileURL = strings.ReplaceAll(tileURL, "{scale}", strconv.FormatUint(uint64(scale), 10))
			tileURL = strings.ReplaceAll(tileURL, "{@scale}", scaleString)
			tileURL = strings.ReplaceAll(tileURL, "{format}", string(format))

			sfSource = services.TileSourceExternal
			return nil, services.DownloadFile(ctx, tileURL, path, "image", 0)
		}

		sfSource = services.TileSourceLocal
		encoded, err := h.renderer.Render(ctx, renderer.Request{
			StyleID: style,
			Z:       z, X: x, Y: y,
			Scale:   scale,
			Format:  format,
		})
		if err != nil {
			return nil, err
		}
		return nil, fileutil.AtomicWriteFile(path, encoded, 0o644)
	})
	if sfSource != "" {
		services.GlobalMetrics.RecordTileGenerate(style, sfSource, time.Since(sfStart).Seconds())
	}

	if err != nil {
		return nil, fmt.Errorf("failed to generate tile %s/%d/%d/%d: %w", style, z, x, y, err)
	}

	h.statsController.TileServed(true, path, style)
	return &TileResult{Path: path, Cached: false}, nil
}

func (h *TileHandler) generateTileAndResponse(w http.ResponseWriter, r *http.Request, style string, z, x, y int, scale uint8, format models.ImageFormat) {
	startTime := time.Now()
	services.GlobalMetrics.IncrementInFlight("tile")
	defer services.GlobalMetrics.DecrementInFlight("tile")

	result, err := h.GenerateTile(r.Context(), style, z, x, y, scale, format)
	if err != nil {
		slog.Error("Failed to generate tile", "error", err)
		services.GlobalMetrics.RecordError("tile", "generation_failed")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	duration := time.Since(startTime).Seconds()
	slog.Debug("Served tile", "cached", result.Cached)
	services.GlobalMetrics.RecordRequest("tile", style, result.Cached, duration)

	serveFile(w, r, result.Path)
}
