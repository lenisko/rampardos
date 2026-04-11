package views

import (
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"

	"github.com/lenisko/rampardos/internal/services"
	"github.com/lenisko/rampardos/internal/templates"
)

// TemplateRenderer handles HTML template rendering
// Each page template is parsed separately with base.html to avoid block name conflicts
type TemplateRenderer struct {
	funcMap   template.FuncMap
	templates map[string]*template.Template
}

// NewTemplateRenderer creates a template renderer from embedded templates
func NewTemplateRenderer() (*TemplateRenderer, error) {
	funcMap := template.FuncMap{
		"eq": func(a, b any) bool {
			return a == b
		},
		"upper": strings.ToUpper,
	}

	r := &TemplateRenderer{
		funcMap:   funcMap,
		templates: make(map[string]*template.Template),
	}

	// Read base template from embedded FS
	baseContent, err := fs.ReadFile(templates.FS, "base.html")
	if err != nil {
		return nil, err
	}

	// Parse each page template separately with base
	entries, err := fs.ReadDir(templates.FS, ".")
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == "base.html" || entry.Name() == "embed.go" {
			continue
		}
		if len(entry.Name()) < 5 || entry.Name()[len(entry.Name())-5:] != ".html" {
			continue
		}

		pageContent, err := fs.ReadFile(templates.FS, entry.Name())
		if err != nil {
			return nil, err
		}

		// Parse base first, then page template (page overrides base's empty blocks)
		tmpl, err := template.New("base").Funcs(funcMap).Parse(string(baseContent))
		if err != nil {
			return nil, err
		}

		_, err = tmpl.New(entry.Name()).Parse(string(pageContent))
		if err != nil {
			return nil, err
		}

		r.templates[entry.Name()] = tmpl
	}

	return r, nil
}

// BaseContext contains common fields for all page contexts
type BaseContext struct {
	DebugEnabled bool
}

// NewBaseContext creates a BaseContext with current runtime settings
func NewBaseContext() BaseContext {
	debugEnabled := false
	if services.GlobalRuntimeSettings != nil {
		debugEnabled = services.GlobalRuntimeSettings.IsDebugEnabled()
	}
	return BaseContext{DebugEnabled: debugEnabled}
}

// Render renders a template with the given context
func (r *TemplateRenderer) Render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	tmpl, ok := r.templates[name]
	if !ok {
		slog.Error("Template not found", "name", name)
		http.Error(w, "Template not found", http.StatusInternalServerError)
		return
	}

	if err := tmpl.ExecuteTemplate(w, "base", data); err != nil {
		slog.Error("Failed to render template", "name", name, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}
