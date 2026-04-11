package views

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/lenisko/rampardos/internal/services"
)

// TemplatesView renders template pages
type TemplatesView struct {
	templatesController *services.TemplatesController
	renderer            *TemplateRenderer
}

// NewTemplatesView creates a new templates view
func NewTemplatesView(templatesController *services.TemplatesController, renderer *TemplateRenderer) *TemplatesView {
	return &TemplatesView{
		templatesController: templatesController,
		renderer:            renderer,
	}
}

// TemplatesListContext is the template context for templates list page
type TemplatesListContext struct {
	BaseContext
	PageID    string
	PageName  string
	Templates []string
}

// TemplatesEditContext is the template context for edit template page
type TemplatesEditContext struct {
	BaseContext
	PageID          string
	PageName        string
	TemplateName    string
	TemplateContent string
	TestData        string
}

// Render handles GET /admin/templates
func (v *TemplatesView) Render(w http.ResponseWriter, r *http.Request) {
	templates, _ := v.templatesController.GetTemplates()

	ctx := TemplatesListContext{
		BaseContext: NewBaseContext(),
		PageID:      "templates",
		PageName:    "Templates",
		Templates:   templates,
	}

	v.renderer.Render(w, "templates.html", ctx)
}

// RenderAdd handles GET /admin/templates/add
func (v *TemplatesView) RenderAdd(w http.ResponseWriter, r *http.Request) {
	ctx := TemplatesEditContext{
		BaseContext:     NewBaseContext(),
		PageID:          "templates",
		PageName:        "Add Template",
		TemplateName:    "",
		TemplateContent: "{}",
		TestData:        "",
	}

	v.renderer.Render(w, "templates_edit.html", ctx)
}

// RenderEdit handles GET /admin/templates/edit/:name
func (v *TemplatesView) RenderEdit(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	content, err := v.templatesController.GetTemplateContent(name)
	if err != nil {
		content = "{}"
	}
	testData, _ := v.templatesController.GetTestData(name)

	ctx := TemplatesEditContext{
		BaseContext:     NewBaseContext(),
		PageID:          "templates",
		PageName:        "Editing template: " + name,
		TemplateName:    name,
		TemplateContent: content,
		TestData:        testData,
	}

	v.renderer.Render(w, "templates_edit.html", ctx)
}
