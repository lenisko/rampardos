package views

import (
	"net/http"
)

// ConvertView renders the convert page
type ConvertView struct {
	renderer *TemplateRenderer
}

// NewConvertView creates a new convert view
func NewConvertView(renderer *TemplateRenderer) *ConvertView {
	return &ConvertView{
		renderer: renderer,
	}
}

// ConvertContext is the template context for convert page
type ConvertContext struct {
	BaseContext
	PageID   string
	PageName string
}

// Render handles GET /admin/convert
func (v *ConvertView) Render(w http.ResponseWriter, r *http.Request) {
	ctx := ConvertContext{
		BaseContext: NewBaseContext(),
		PageID:      "convert",
		PageName:    "Convert",
	}

	v.renderer.Render(w, "convert.html", ctx)
}
