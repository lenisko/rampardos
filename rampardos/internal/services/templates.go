package services

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// TemplatesController manages JSON templates
type TemplatesController struct {
	folder string
}

// NewTemplatesController creates a new templates controller
func NewTemplatesController(folder string) *TemplatesController {
	os.MkdirAll(folder, 0755)
	return &TemplatesController{folder: folder}
}

// GetTemplates returns a list of available templates
func (tc *TemplatesController) GetTemplates() ([]string, error) {
	entries, err := os.ReadDir(tc.folder)
	if err != nil {
		return nil, err
	}

	var templates []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.HasSuffix(entry.Name(), ".json") {
			// Skip testdata files
			if strings.HasSuffix(entry.Name(), ".testdata.json") {
				continue
			}
			name := strings.TrimSuffix(entry.Name(), ".json")
			// Skip preview files
			if strings.HasPrefix(name, "preview-") {
				continue
			}
			templates = append(templates, name)
		}
	}

	return templates, nil
}

// GetTemplateContent returns the content of a template
func (tc *TemplatesController) GetTemplateContent(name string) (string, error) {
	sanitized, err := SanitizeName(name)
	if err != nil {
		return "", fmt.Errorf("invalid template name: %w", err)
	}
	path := filepath.Join(tc.folder, sanitized+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// SaveTemplate saves a template
func (tc *TemplatesController) SaveTemplate(name, oldName, content string) error {
	sanitized, err := SanitizeName(name)
	if err != nil {
		return fmt.Errorf("invalid template name: %w", err)
	}
	// Remove old file if name changed
	if oldName != "" && oldName != name {
		oldSanitized, err := SanitizeName(oldName)
		if err == nil {
			oldPath := filepath.Join(tc.folder, oldSanitized+".json")
			os.Remove(oldPath)
		}
	}

	path := filepath.Join(tc.folder, sanitized+".json")

	// Create backup of existing file before saving
	if existingContent, err := os.ReadFile(path); err == nil {
		timestamp := time.Now().Unix()
		backupPath := filepath.Join(tc.folder, fmt.Sprintf("%s.json.%d", sanitized, timestamp))
		os.WriteFile(backupPath, existingContent, 0644)
	}

	return os.WriteFile(path, []byte(content), 0644)
}

// GetTemplateVersions returns a list of backup timestamps for a template
func (tc *TemplatesController) GetTemplateVersions(name string) ([]int64, error) {
	sanitized, err := SanitizeName(name)
	if err != nil {
		return nil, fmt.Errorf("invalid template name: %w", err)
	}

	entries, err := os.ReadDir(tc.folder)
	if err != nil {
		return nil, err
	}

	prefix := sanitized + ".json."
	var versions []int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.HasPrefix(entry.Name(), prefix) {
			timestampStr := strings.TrimPrefix(entry.Name(), prefix)
			var timestamp int64
			if _, err := fmt.Sscanf(timestampStr, "%d", &timestamp); err == nil {
				versions = append(versions, timestamp)
			}
		}
	}

	// Sort descending (newest first)
	sort.Slice(versions, func(i, j int) bool {
		return versions[i] > versions[j]
	})

	return versions, nil
}

// GetTemplateVersionContent returns the content of a specific template version
func (tc *TemplatesController) GetTemplateVersionContent(name string, timestamp int64) (string, error) {
	sanitized, err := SanitizeName(name)
	if err != nil {
		return "", fmt.Errorf("invalid template name: %w", err)
	}
	path := filepath.Join(tc.folder, fmt.Sprintf("%s.json.%d", sanitized, timestamp))
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// DeleteTemplate removes a template and its test data
func (tc *TemplatesController) DeleteTemplate(name string) error {
	sanitized, err := SanitizeName(name)
	if err != nil {
		return fmt.Errorf("invalid template name: %w", err)
	}
	path := filepath.Join(tc.folder, sanitized+".json")
	err = os.Remove(path)
	if err != nil {
		return err
	}
	// Also remove test data if exists
	tc.DeleteTestData(name)
	return nil
}

// GetFolder returns the templates folder path
func (tc *TemplatesController) GetFolder() string {
	return tc.folder
}

// GetTestData returns the test data for a template
func (tc *TemplatesController) GetTestData(name string) (string, error) {
	sanitized, err := SanitizeName(name)
	if err != nil {
		return "", fmt.Errorf("invalid template name: %w", err)
	}
	path := filepath.Join(tc.folder, sanitized+".testdata.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}

// SaveTestData saves test data for a template
func (tc *TemplatesController) SaveTestData(name, content string) error {
	sanitized, err := SanitizeName(name)
	if err != nil {
		return fmt.Errorf("invalid template name: %w", err)
	}
	path := filepath.Join(tc.folder, sanitized+".testdata.json")
	return os.WriteFile(path, []byte(content), 0644)
}

// DeleteTestData removes test data for a template
func (tc *TemplatesController) DeleteTestData(name string) error {
	sanitized, err := SanitizeName(name)
	if err != nil {
		return fmt.Errorf("invalid template name: %w", err)
	}
	path := filepath.Join(tc.folder, sanitized+".testdata.json")
	err = os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
