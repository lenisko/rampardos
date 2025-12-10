package views

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/lenisko/rampardos/internal/models"
	"github.com/lenisko/rampardos/internal/services"
)

// StylesView renders style pages
type StylesView struct {
	stylesController *services.StylesController
	templates        *TemplateRenderer
	previewLatitude  float64
	previewLongitude float64
}

// NewStylesView creates a new styles view
func NewStylesView(stylesController *services.StylesController, templates *TemplateRenderer) *StylesView {
	return &StylesView{
		stylesController: stylesController,
		templates:        templates,
		previewLatitude:  52.5200, // Default Berlin
		previewLongitude: 13.4050,
	}
}

// StylesContext is the template context for styles page
type StylesContext struct {
	BaseContext
	PageID           string
	PageName         string
	Styles           []models.Style
	PreviewLatitude  float64
	PreviewLongitude float64
	Time             int64
}

// StylesAddExternalContext is the template context for add external style page
type StylesAddExternalContext struct {
	BaseContext
	PageID   string
	PageName string
}

// StylesAddLocalContext is the template context for add local style page
type StylesAddLocalContext struct {
	BaseContext
	PageID   string
	PageName string
}

// StylesDeleteLocalContext is the template context for delete local style page
type StylesDeleteLocalContext struct {
	BaseContext
	PageID   string
	PageName string
	ID       string
}

// Render handles GET /admin/styles
func (v *StylesView) Render(w http.ResponseWriter, r *http.Request) {
	styles, _ := v.stylesController.GetStylesWithAnalysis(r.Context())

	ctx := StylesContext{
		BaseContext:      NewBaseContext(),
		PageID:           "styles",
		PageName:         "Styles",
		Styles:           styles,
		PreviewLatitude:  v.previewLatitude,
		PreviewLongitude: v.previewLongitude,
		Time:             time.Now().Unix(),
	}

	v.templates.Render(w, "styles.html", ctx)
}

// RenderAddExternal handles GET /admin/styles/external/add
func (v *StylesView) RenderAddExternal(w http.ResponseWriter, r *http.Request) {
	ctx := StylesAddExternalContext{
		BaseContext: NewBaseContext(),
		PageID:      "styles",
		PageName:    "Add External Style",
	}

	v.templates.Render(w, "styles_add_external.html", ctx)
}

// RenderAddLocal handles GET /admin/styles/local/add
func (v *StylesView) RenderAddLocal(w http.ResponseWriter, r *http.Request) {
	ctx := StylesAddLocalContext{
		BaseContext: NewBaseContext(),
		PageID:      "styles",
		PageName:    "Add Local Style",
	}

	v.templates.Render(w, "styles_add_local.html", ctx)
}

// RenderDeleteLocal handles GET /admin/styles/local/delete/:id
func (v *StylesView) RenderDeleteLocal(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	ctx := StylesDeleteLocalContext{
		BaseContext: NewBaseContext(),
		PageID:      "styles",
		PageName:    "Delete Local Style",
		ID:          id,
	}

	v.templates.Render(w, "styles_delete_local.html", ctx)
}
