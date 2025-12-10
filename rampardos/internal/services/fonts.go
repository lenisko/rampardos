package services

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	buildGlyphsCommand = "/usr/local/bin/build-glyphs"
)

// FontsController manages fonts
type FontsController struct {
	folder     string
	tempFolder string
}

// NewFontsController creates a new fonts controller
func NewFontsController(folder, tempFolder string) *FontsController {
	os.MkdirAll(folder, 0755)
	os.RemoveAll(tempFolder)
	os.MkdirAll(tempFolder, 0755)

	return &FontsController{
		folder:     folder,
		tempFolder: tempFolder,
	}
}

// GetFonts returns a list of available fonts
func (fc *FontsController) GetFonts() ([]string, error) {
	entries, err := os.ReadDir(fc.folder)
	if err != nil {
		return nil, err
	}

	var fonts []string
	for _, entry := range entries {
		if entry.IsDir() {
			fonts = append(fonts, entry.Name())
		}
	}

	return fonts, nil
}

// AddFont adds a new font from a file
func (fc *FontsController) AddFont(data []byte, filename string) error {
	// Extract name from filename
	ext := filepath.Ext(filename)
	baseName := strings.TrimSuffix(filename, ext)
	name := toCamelCase(baseName)

	// Write temp file
	tempFile := filepath.Join(fc.tempFolder, fmt.Sprintf("%d%s", os.Getpid(), ext))
	if err := os.WriteFile(tempFile, data, 0644); err != nil {
		return fmt.Errorf("failed to write temp file: %w", err)
	}
	defer os.Remove(tempFile)

	// Build glyphs
	if err := fc.buildGlyphs(tempFile, name); err != nil {
		return err
	}

	// Save original font file for preview
	fontPath := filepath.Join(fc.folder, name, "font"+ext)
	if err := os.WriteFile(fontPath, data, 0644); err != nil {
		slog.Warn("Failed to save original font file", "error", err)
	}

	return nil
}

// GetFontFile returns the path to the original font file if it exists
func (fc *FontsController) GetFontFile(name string) (string, error) {
	sanitized, err := SanitizeName(name)
	if err != nil {
		return "", fmt.Errorf("invalid font name: %w", err)
	}
	fontDir := filepath.Join(fc.folder, sanitized)
	entries, err := os.ReadDir(fontDir)
	if err != nil {
		return "", err
	}

	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "font.") {
			return filepath.Join(fontDir, entry.Name()), nil
		}
	}

	return "", fmt.Errorf("font file not found")
}

// DeleteFont removes a font
func (fc *FontsController) DeleteFont(name string) error {
	sanitized, err := SanitizeName(name)
	if err != nil {
		return fmt.Errorf("invalid font name: %w", err)
	}
	path := filepath.Join(fc.folder, sanitized)
	return os.RemoveAll(path)
}

func (fc *FontsController) buildGlyphs(file, name string) error {
	path := filepath.Join(fc.folder, name)

	// Remove existing
	os.RemoveAll(path)

	// Create directory
	if err := os.MkdirAll(path, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// Run build-glyphs
	cmd := exec.Command(buildGlyphsCommand, file, path)
	output, err := cmd.CombinedOutput()
	if err != nil {
		os.RemoveAll(path)
		slog.Error("Failed to build glyphs", "error", err, "output", string(output))
		return fmt.Errorf("failed to create glyphs: %s", string(output))
	}

	return nil
}

// toCamelCase converts a string to CamelCase
func toCamelCase(s string) string {
	// Split by common separators
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == '-' || r == '_'
	})

	var result strings.Builder
	for _, part := range parts {
		if len(part) > 0 {
			result.WriteString(strings.ToUpper(string(part[0])))
			if len(part) > 1 {
				result.WriteString(part[1:])
			}
		}
	}

	return result.String()
}
