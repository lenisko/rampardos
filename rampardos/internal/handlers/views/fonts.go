package views

import (
	"net/http"

	"github.com/lenisko/rampardos/internal/services"
)

// FontsView renders font pages
type FontsView struct {
	fontsController *services.FontsController
	templates       *TemplateRenderer
}

// NewFontsView creates a new fonts view
func NewFontsView(fontsController *services.FontsController, templates *TemplateRenderer) *FontsView {
	return &FontsView{
		fontsController: fontsController,
		templates:       templates,
	}
}

// FontsContext is the template context for fonts page
type FontsContext struct {
	BaseContext
	PageID   string
	PageName string
	Fonts    []string
}

// FontsAddContext is the template context for add font page
type FontsAddContext struct {
	BaseContext
	PageID   string
	PageName string
}

// Render handles GET /admin/fonts
func (v *FontsView) Render(w http.ResponseWriter, r *http.Request) {
	fonts, _ := v.fontsController.GetFonts()

	ctx := FontsContext{
		BaseContext: NewBaseContext(),
		PageID:      "fonts",
		PageName:    "Fonts",
		Fonts:       fonts,
	}

	v.templates.Render(w, "fonts.html", ctx)
}

// RenderAdd handles GET /admin/fonts/add
func (v *FontsView) RenderAdd(w http.ResponseWriter, r *http.Request) {
	ctx := FontsAddContext{
		BaseContext: NewBaseContext(),
		PageID:      "fonts",
		PageName:    "Add Font",
	}

	v.templates.Render(w, "fonts_add.html", ctx)
}
