package models

// StyleAnalysis contains analysis results for a style
type StyleAnalysis struct {
	MissingFonts []string `json:"missingFonts"`
	MissingIcons []string `json:"missingIcons"`
}

// Style represents a map style
type Style struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	External *bool          `json:"external,omitempty"`
	URL      string         `json:"url,omitempty"`
	Analysis *StyleAnalysis `json:"analysis,omitempty"`
}

// RemovingURL returns a copy without the URL field
func (s Style) RemovingURL() Style {
	return Style{
		ID:       s.ID,
		Name:     s.Name,
		External: s.External,
	}
}

// IsExternal returns true if this is an external style
func (s Style) IsExternal() bool {
	return s.External != nil && *s.External
}
