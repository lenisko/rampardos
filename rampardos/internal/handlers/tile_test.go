package handlers

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/lenisko/rampardos/internal/models"
	"github.com/lenisko/rampardos/internal/services"
	"github.com/lenisko/rampardos/internal/services/renderer"
)

func init() {
	// Ensure GlobalMetrics is initialised so StatsController.TileServed
	// doesn't nil-deref during unit tests.
	if services.GlobalMetrics == nil {
		services.InitMetrics()
	}
}

// newTestStatsController returns a StatsController backed by a real
// (but un-started) FileToucher so that TileServed never nil-derefs.
func newTestStatsController() *services.StatsController {
	return services.NewStatsController(services.NewFileToucher())
}

func TestTileHandlerGenerateTileUsesRendererForLocalStyle(t *testing.T) {
	fake := &renderer.Fake{
		Canned: []byte{0x89, 'P', 'N', 'G', 0, 0, 0, 0},
	}
	// Remove any leftover cached tile from a previous run.
	os.Remove("Cache/Tile/local-14-8188-5448-1.png")

	h := &TileHandler{
		renderer:         fake,
		statsController:  newTestStatsController(),
		stylesController: stubStylesController{ext: nil},
	}

	result, err := h.GenerateTile(context.Background(), "local", 14, 8188, 5448, 1, models.ImageFormatPNG)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("result was nil")
	}
	if len(fake.Calls) != 1 {
		t.Fatalf("expected 1 renderer call, got %d", len(fake.Calls))
	}
	call := fake.Calls[0]
	if call.Kind != "Render" {
		t.Errorf("call kind: got %q, want Render", call.Kind)
	}
	if call.Request.StyleID != "local" || call.Request.Z != 14 || call.Request.X != 8188 || call.Request.Y != 5448 {
		t.Errorf("call request: %+v", call.Request)
	}
}

func TestTileHandlerGenerateTileReturnsRenderErrors(t *testing.T) {
	fake := &renderer.Fake{
		RenderFn: func(ctx context.Context, req renderer.Request) ([]byte, error) {
			return nil, errors.New("render failed")
		},
	}
	// Remove any leftover cached tile from a previous run.
	os.Remove("Cache/Tile/local-0-0-0-1.png")

	h := &TileHandler{
		renderer:         fake,
		statsController:  newTestStatsController(),
		stylesController: stubStylesController{ext: nil},
	}

	_, err := h.GenerateTile(context.Background(), "local", 0, 0, 0, 1, models.ImageFormatPNG)
	if err == nil {
		t.Errorf("expected render failure error, got nil")
	}
}
