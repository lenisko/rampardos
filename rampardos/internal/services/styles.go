package services

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/lenisko/rampardos/internal/models"
)

// StylesController manages map styles
type StylesController struct {
	folder          string
	fontsController *FontsController

	mu             sync.RWMutex
	externalStyles map[string]models.Style
}

// NewStylesController creates a new styles controller
func NewStylesController(externalStyles []models.Style, folder string, fontsController *FontsController) *StylesController {
	os.MkdirAll(folder, 0755)
	os.MkdirAll(filepath.Join(folder, "External"), 0755)

	sc := &StylesController{
		folder:          folder,
		fontsController: fontsController,
		externalStyles:  make(map[string]models.Style),
	}

	// Load external styles from env and file
	for _, style := range externalStyles {
		sc.externalStyles[style.ID] = style
	}

	// Load saved external styles
	for _, style := range sc.loadExternalStyles() {
		sc.externalStyles[style.ID] = style
	}

	return sc
}

func (sc *StylesController) loadExternalStyles() []models.Style {
	path := filepath.Join(sc.folder, "External", "styles.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var styles []models.Style
	if err := json.Unmarshal(data, &styles); err != nil {
		slog.Warn("Failed to parse external styles", "error", err)
		return nil
	}

	return styles
}

func (sc *StylesController) saveExternalStyles() error {
	sc.mu.RLock()
	styles := make([]models.Style, 0, len(sc.externalStyles))
	for _, style := range sc.externalStyles {
		styles = append(styles, style)
	}
	sc.mu.RUnlock()

	data, err := json.MarshalIndent(styles, "", "  ")
	if err != nil {
		return err
	}

	path := filepath.Join(sc.folder, "External", "styles.json")
	return os.WriteFile(path, data, 0644)
}

// GetStyles returns all available styles
func (sc *StylesController) GetStyles(ctx context.Context) ([]models.Style, error) {
	localStyles, err := sc.GetLocalStyles()
	if err != nil {
		return nil, err
	}

	sc.mu.RLock()
	externalStyles := make([]models.Style, 0, len(sc.externalStyles))
	for _, style := range sc.externalStyles {
		externalStyles = append(externalStyles, style)
	}
	sc.mu.RUnlock()

	allStyles := append(localStyles, externalStyles...)

	// Remove URLs from response
	result := make([]models.Style, len(allStyles))
	for i, style := range allStyles {
		result[i] = style.RemovingURL()
	}

	return result, nil
}

// GetStylesWithAnalysis returns all styles with font/icon analysis for local styles
func (sc *StylesController) GetStylesWithAnalysis(ctx context.Context) ([]models.Style, error) {
	styles, err := sc.GetStyles(ctx)
	if err != nil {
		return nil, err
	}

	// Analyze local styles only
	for i, style := range styles {
		if !style.IsExternal() {
			analysis := sc.analyzeStyle(style.ID)
			styles[i].Analysis = analysis
		}
	}

	return styles, nil
}

// analyzeStyle checks for missing fonts and icons in a style
func (sc *StylesController) analyzeStyle(id string) *models.StyleAnalysis {
	usage := sc.analyzeUsage(id)
	availableIcons := sc.analyzeAvailableIcons(id)

	fonts, err := sc.fontsController.GetFonts()
	if err != nil {
		return &models.StyleAnalysis{
			MissingFonts: []string{"error loading fonts"},
			MissingIcons: []string{"error loading fonts"},
		}
	}

	// Find missing fonts
	fontSet := make(map[string]bool)
	for _, f := range fonts {
		fontSet[f] = true
	}
	var missingFonts []string
	for _, f := range usage.fonts {
		if !fontSet[f] {
			missingFonts = append(missingFonts, f)
		}
	}

	// Find missing icons
	iconSet := make(map[string]bool)
	for _, i := range availableIcons {
		iconSet[i] = true
	}
	var missingIcons []string
	for _, i := range usage.icons {
		if !iconSet[i] {
			missingIcons = append(missingIcons, i)
		}
	}

	return &models.StyleAnalysis{
		MissingFonts: missingFonts,
		MissingIcons: missingIcons,
	}
}

type styleUsage struct {
	fonts []string
	icons []string
}

// analyzeUsage parses style.json to find used fonts and icons
func (sc *StylesController) analyzeUsage(id string) styleUsage {
	stylePath := filepath.Join(sc.folder, id+".json")
	data, err := os.ReadFile(stylePath)
	if err != nil {
		return styleUsage{}
	}

	var styleJSON map[string]any
	if err := json.Unmarshal(data, &styleJSON); err != nil {
		return styleUsage{}
	}

	layers, ok := styleJSON["layers"].([]any)
	if !ok {
		return styleUsage{}
	}

	fontSet := make(map[string]bool)
	iconSet := make(map[string]bool)

	for _, layerAny := range layers {
		layer, ok := layerAny.(map[string]any)
		if !ok {
			continue
		}

		// Check layout for fonts and icons
		if layout, ok := layer["layout"].(map[string]any); ok {
			if textFonts, ok := layout["text-font"].([]any); ok && len(textFonts) > 0 {
				if font, ok := textFonts[0].(string); ok && font != "" {
					fontSet[font] = true
				}
			}
			if iconImage, ok := layout["icon-image"].(string); ok && iconImage != "" {
				iconSet[iconImage] = true
			}
		}

		// Check paint for pattern icons
		if paint, ok := layer["paint"].(map[string]any); ok {
			for _, key := range []string{"background-pattern", "fill-pattern", "line-pattern", "fill-extrusion-pattern"} {
				if pattern, ok := paint[key].(string); ok && pattern != "" {
					iconSet[pattern] = true
				}
			}
		}
	}

	fonts := make([]string, 0, len(fontSet))
	for f := range fontSet {
		fonts = append(fonts, f)
	}
	icons := make([]string, 0, len(iconSet))
	for i := range iconSet {
		icons = append(icons, i)
	}

	return styleUsage{fonts: fonts, icons: icons}
}

// analyzeAvailableIcons reads sprite.json to get available icons
func (sc *StylesController) analyzeAvailableIcons(id string) []string {
	spritePath := filepath.Join(sc.folder, id, "sprite.json")
	data, err := os.ReadFile(spritePath)
	if err != nil {
		return nil
	}

	var spriteJSON map[string]any
	if err := json.Unmarshal(data, &spriteJSON); err != nil {
		return nil
	}

	icons := make([]string, 0, len(spriteJSON))
	for key := range spriteJSON {
		icons = append(icons, key)
	}
	return icons
}

// GetExternalStyle returns an external style by name
func (sc *StylesController) GetExternalStyle(name string) *models.Style {
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	style, ok := sc.externalStyles[name]
	if !ok {
		return nil
	}
	return &style
}

// AddExternalStyle adds a new external style
func (sc *StylesController) AddExternalStyle(style models.Style) error {
	if !style.IsExternal() || style.URL == "" {
		return fmt.Errorf("URL is required for external styles")
	}

	sc.mu.Lock()
	sc.externalStyles[style.ID] = style
	sc.mu.Unlock()

	return sc.saveExternalStyles()
}

// DeleteExternalStyle removes an external style
func (sc *StylesController) DeleteExternalStyle(id string) error {
	sc.mu.Lock()
	delete(sc.externalStyles, id)
	sc.mu.Unlock()

	return sc.saveExternalStyles()
}

// AddLocalStyle adds a local style from a ZIP file
// ZIP should contain: style.json, sprite.json, sprite.png, sprite@2x.json, sprite@2x.png
func (sc *StylesController) AddLocalStyle(id, name string, zipData []byte) error {
	sanitizedID, err := SanitizeName(id)
	if err != nil {
		return fmt.Errorf("invalid style ID: %w", err)
	}
	styleDir := filepath.Join(sc.folder, sanitizedID)

	// Check if style already exists
	if _, err := os.Stat(styleDir); err == nil {
		return fmt.Errorf("style %s already exists", id)
	}

	// Create style directory
	if err := os.MkdirAll(styleDir, 0755); err != nil {
		return fmt.Errorf("failed to create style directory: %w", err)
	}

	// Extract ZIP
	if err := extractZip(zipData, styleDir); err != nil {
		os.RemoveAll(styleDir)
		return fmt.Errorf("failed to extract ZIP: %w", err)
	}

	// Verify required files exist
	requiredFiles := []string{"style.json"}
	for _, file := range requiredFiles {
		if _, err := os.Stat(filepath.Join(styleDir, file)); os.IsNotExist(err) {
			os.RemoveAll(styleDir)
			return fmt.Errorf("ZIP missing required file: %s", file)
		}
	}

	// Update style.json with correct paths
	if err := sc.updateStyleJSON(styleDir, id); err != nil {
		slog.Warn("Failed to update style.json paths", "error", err)
	}

	slog.Info("Added local style", "id", id, "name", name)
	return nil
}

// updateStyleJSON updates the style.json with correct sprite and glyphs paths
func (sc *StylesController) updateStyleJSON(styleDir, _ string) error {
	stylePath := filepath.Join(styleDir, "style.json")
	data, err := os.ReadFile(stylePath)
	if err != nil {
		return err
	}

	var styleJSON map[string]any
	if err := json.Unmarshal(data, &styleJSON); err != nil {
		return err
	}

	// Update sprite path if sprites exist
	if _, err := os.Stat(filepath.Join(styleDir, "sprite.png")); err == nil {
		styleJSON["sprite"] = "{styleUrl}/sprite"
	}

	// Update glyphs path
	styleJSON["glyphs"] = "{fontUrl}/{fontstack}/{range}.pbf"

	updated, err := json.MarshalIndent(styleJSON, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(stylePath, updated, 0644)
}

// DeleteLocalStyle removes a local style
func (sc *StylesController) DeleteLocalStyle(id string) error {
	sanitizedID, err := SanitizeName(id)
	if err != nil {
		return fmt.Errorf("invalid style ID: %w", err)
	}

	// Don't allow deleting External folder
	if sanitizedID == "External" {
		return fmt.Errorf("cannot delete External styles folder")
	}

	styleDir := filepath.Join(sc.folder, sanitizedID)
	styleJSON := filepath.Join(sc.folder, sanitizedID+".json")

	// Check if directory exists
	dirInfo, dirErr := os.Stat(styleDir)
	dirExists := dirErr == nil && dirInfo.IsDir()

	// Check if JSON file exists
	_, jsonErr := os.Stat(styleJSON)
	jsonExists := jsonErr == nil

	if !dirExists && !jsonExists {
		return fmt.Errorf("style %s not found", id)
	}

	// Delete directory if exists
	if dirExists {
		if err := os.RemoveAll(styleDir); err != nil {
			return fmt.Errorf("failed to delete style directory: %w", err)
		}
	}

	// Delete JSON file if exists
	if jsonExists {
		if err := os.Remove(styleJSON); err != nil {
			return fmt.Errorf("failed to delete style JSON: %w", err)
		}
	}

	slog.Info("Deleted local style", "id", id)
	return nil
}

// GetLocalStyleIDs returns the IDs of all local styles.
func (sc *StylesController) GetLocalStyleIDs() ([]string, error) {
	styles, err := sc.GetLocalStyles()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(styles))
	for _, s := range styles {
		ids = append(ids, s.ID)
	}
	return ids, nil
}

// GetLocalStyles returns only local styles (directories in styles folder)
func (sc *StylesController) GetLocalStyles() ([]models.Style, error) {
	entries, err := os.ReadDir(sc.folder)
	if err != nil {
		return nil, err
	}

	var styles []models.Style
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "External" {
			continue
		}

		// Check if style.json exists
		stylePath := filepath.Join(sc.folder, entry.Name(), "style.json")
		if _, err := os.Stat(stylePath); os.IsNotExist(err) {
			continue
		}

		styles = append(styles, models.Style{
			ID:   entry.Name(),
			Name: entry.Name(),
		})
	}

	return styles, nil
}
