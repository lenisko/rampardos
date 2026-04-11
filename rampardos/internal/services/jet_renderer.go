package services

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/CloudyKit/jet/v6"
)

// bufferPool is a sync.Pool for reusing bytes.Buffer instances
var bufferPool = sync.Pool{
	New: func() any {
		return new(bytes.Buffer)
	},
}

// JetRenderer manages Jet templates with in-memory caching
type JetRenderer struct {
	loader *MemoryLoader
	set    *jet.Set
	mu     sync.RWMutex
	folder string
}

// MemoryLoader implements jet.Loader for in-memory templates
type MemoryLoader struct {
	templates map[string]string
	mu        sync.RWMutex
}

// NewMemoryLoader creates a new memory loader
func NewMemoryLoader() *MemoryLoader {
	return &MemoryLoader{
		templates: make(map[string]string),
	}
}

// Open implements jet.Loader - returns io.ReadCloser
func (l *MemoryLoader) Open(name string) (io.ReadCloser, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	// Normalize name - remove leading slash if present
	name = strings.TrimPrefix(name, "/")

	content, ok := l.templates[name]
	if !ok {
		return nil, fmt.Errorf("template not found: %s", name)
	}

	return io.NopCloser(strings.NewReader(content)), nil
}

// Exists implements jet.Loader
func (l *MemoryLoader) Exists(name string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	// Normalize name - remove leading slash if present
	name = strings.TrimPrefix(name, "/")
	_, ok := l.templates[name]
	return ok
}

// Set stores a template in memory
func (l *MemoryLoader) Set(name, content string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.templates[name] = content
}

// Delete removes a template from memory
func (l *MemoryLoader) Delete(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.templates, name)
}

// GlobalJetRenderer is the global Jet renderer instance
var GlobalJetRenderer *JetRenderer

// NewJetRenderer creates a new Jet renderer
func NewJetRenderer(templatesFolder string) *JetRenderer {
	loader := NewMemoryLoader()

	set := jet.NewSet(
		loader,
		jet.InDevelopmentMode(), // Always reparse templates
	)

	// Add custom functions
	set.AddGlobal("index", func(arr any, idx int) any {
		switch v := arr.(type) {
		case []any:
			if idx >= 0 && idx < len(v) {
				return v[idx]
			}
		case []string:
			if idx >= 0 && idx < len(v) {
				return v[idx]
			}
		case []float64:
			if idx >= 0 && idx < len(v) {
				return v[idx]
			}
		case []int:
			if idx >= 0 && idx < len(v) {
				return v[idx]
			}
		}
		return nil
	})

	// json parses a JSON string into a Go value (for iterating over arrays stored as strings)
	set.AddGlobal("json", func(s string) any {
		var result any
		if err := json.Unmarshal([]byte(s), &result); err != nil {
			return nil
		}
		return result
	})

	jr := &JetRenderer{
		loader: loader,
		set:    set,
		folder: templatesFolder,
	}

	return jr
}

// LoadTemplatesFromDisk loads all templates from the templates folder
func (jr *JetRenderer) LoadTemplatesFromDisk() error {
	jr.mu.Lock()
	defer jr.mu.Unlock()

	entries, err := os.ReadDir(jr.folder)
	if err != nil {
		if os.IsNotExist(err) {
			slog.Info("Templates folder does not exist, skipping", "folder", jr.folder)
			return nil
		}
		return fmt.Errorf("failed to read templates folder: %w", err)
	}

	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		// Skip testdata files
		if strings.HasSuffix(entry.Name(), ".testdata.json") {
			continue
		}
		// Skip preview files
		if strings.HasPrefix(entry.Name(), "preview-") {
			continue
		}

		name := strings.TrimSuffix(entry.Name(), ".json")
		path := filepath.Join(jr.folder, entry.Name())

		content, err := os.ReadFile(path)
		if err != nil {
			slog.Warn("Failed to read template", "name", name, "error", err)
			continue
		}

		jr.loader.Set(name, string(content))
		count++
		slog.Debug("Loaded template", "name", name)
	}

	slog.Info("Loaded templates from disk", "count", count, "folder", jr.folder)
	return nil
}

// SetTemplate stores/updates a template in memory
func (jr *JetRenderer) SetTemplate(name, content string) {
	jr.mu.Lock()
	defer jr.mu.Unlock()

	jr.loader.Set(name, content)
	slog.Debug("Template updated in memory", "name", name)
}

// DeleteTemplate removes a template from memory
func (jr *JetRenderer) DeleteTemplate(name string) {
	jr.mu.Lock()
	defer jr.mu.Unlock()

	jr.loader.Delete(name)
	slog.Debug("Template deleted from memory", "name", name)
}

// Render renders a template with the given context
func (jr *JetRenderer) Render(templateName string, context map[string]any) (string, error) {
	jr.mu.RLock()
	defer jr.mu.RUnlock()

	t, err := jr.set.GetTemplate(templateName)
	if err != nil {
		return "", fmt.Errorf("failed to get template %s: %w", templateName, err)
	}

	vars := make(jet.VarMap)
	for k, v := range context {
		vars.Set(k, v)
	}

	// Get buffer from pool
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufferPool.Put(buf)

	if err := t.Execute(buf, vars, nil); err != nil {
		return "", fmt.Errorf("failed to execute template %s: %w", templateName, err)
	}

	return buf.String(), nil
}

// RenderString renders a template string directly (for preview)
func (jr *JetRenderer) RenderString(templateContent string, context map[string]any) (string, error) {
	// Create a temporary template
	t, err := jr.set.Parse("_preview_", templateContent)
	if err != nil {
		return "", fmt.Errorf("failed to parse template: %w", err)
	}

	vars := make(jet.VarMap)
	for k, v := range context {
		vars.Set(k, v)
	}

	// Get buffer from pool
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufferPool.Put(buf)

	if err := t.Execute(buf, vars, nil); err != nil {
		return "", fmt.Errorf("failed to execute template: %w", err)
	}

	return buf.String(), nil
}

// InitGlobalJetRenderer initializes the global Jet renderer
func InitGlobalJetRenderer(templatesFolder string) error {
	GlobalJetRenderer = NewJetRenderer(templatesFolder)
	return GlobalJetRenderer.LoadTemplatesFromDisk()
}
