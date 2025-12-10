package utils

import (
	"encoding/json"
	"testing"
)

func TestLeafRenderer_SimpleVariable(t *testing.T) {
	ctx := map[string]any{
		"name": "John",
		"age":  30.0,
	}

	template := "Hello #(name), you are #(age) years old"
	result, err := RenderLeafTemplate(template, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := "Hello John, you are 30 years old"
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestLeafRenderer_NestedProperty(t *testing.T) {
	ctx := map[string]any{
		"user": map[string]any{
			"name": "John",
			"age":  25.0,
		},
	}

	template := "User: #(user.name)"
	result, err := RenderLeafTemplate(template, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := "User: John"
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestLeafRenderer_IfStatement(t *testing.T) {
	ctx := map[string]any{
		"show": true,
	}

	template := "#if(show):visible#else:hidden#endif"
	result, err := RenderLeafTemplate(template, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result != "visible" {
		t.Errorf("expected 'visible', got %q", result)
	}

	ctx["show"] = false
	result, err = RenderLeafTemplate(template, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result != "hidden" {
		t.Errorf("expected 'hidden', got %q", result)
	}
}

func TestLeafRenderer_IfNilCheck(t *testing.T) {
	ctx := map[string]any{
		"value": "exists",
	}

	template := "#if(value != nil):has value#else:no value#endif"
	result, err := RenderLeafTemplate(template, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result != "has value" {
		t.Errorf("expected 'has value', got %q", result)
	}

	delete(ctx, "value")
	result, err = RenderLeafTemplate(template, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result != "no value" {
		t.Errorf("expected 'no value', got %q", result)
	}
}

func TestLeafRenderer_ForLoop(t *testing.T) {
	ctx := map[string]any{
		"items": []any{"a", "b", "c"},
	}

	template := "#for(item in items):#(item),#endfor"
	result, err := RenderLeafTemplate(template, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := "a,b,c,"
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestLeafRenderer_ForLoopWithObjects(t *testing.T) {
	ctx := map[string]any{
		"stops": []any{
			map[string]any{"latitude": 49.81216, "longitude": 19.033813, "type": "stop"},
			map[string]any{"latitude": 49.812443, "longitude": 19.031079, "type": "gym"},
		},
	}

	template := `#for(stop in stops):{"lat": #(stop.latitude), "type": "#(stop.type)"},#endfor`
	result, err := RenderLeafTemplate(template, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := `{"lat": 49.81216, "type": "stop"},{"lat": 49.812443, "type": "gym"},`
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestLeafRenderer_NestedIfInForLoop(t *testing.T) {
	ctx := map[string]any{
		"stops": []any{
			map[string]any{"type": "gym", "teamId": 3.0},
			map[string]any{"type": "stop"},
		},
	}

	template := `#for(stop in stops):#if(stop.type == "gym"):#(stop.teamId)#else:pokestop#endif,#endfor`
	result, err := RenderLeafTemplate(template, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := "3,pokestop,"
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestLeafRenderer_ComplexTemplate(t *testing.T) {
	ctx := map[string]any{
		"latitude":  49.815531100541534,
		"longitude": 19.02513016660636,
		"imgUrl":    "https://example.com/pokemon.png",
		"seen_type": "encounter",
		"nearbyStops": []any{
			map[string]any{"latitude": 49.81216, "longitude": 19.033813, "type": "stop"},
			map[string]any{"latitude": 49.81238, "longitude": 19.03372, "type": "gym", "teamId": 3.0},
		},
	}

	template := `{
  "style": "osm-bright",
  "latitude": #(latitude),
  "longitude": #(longitude),
  "markers": [
    #if(nearbyStops != nil):
      #for(stop in nearbyStops): 
      {
        #if(stop.type == "gym"):
          "url": "https://pogo.moe/tiles/#(stop.teamId).png",
        #else:
          "url": "https://pogo.moe/tiles/#(stop.type).png",
        #endif
        "latitude": #(stop.latitude),
        "longitude": #(stop.longitude)
      },
      #endfor
    #endif
    {
      "url": "#(imgUrl)",
      "latitude": #(latitude),
      "longitude": #(longitude)
    }
  ],
  #if(seen_type == "nearby_cell"):
    "polygons": []
  #else: 
    "circles": [
      {
        "latitude": #(latitude),
        "longitude": #(longitude),
        "radius": 40
      }
    ]
  #endif
}`

	result, err := RenderLeafTemplate(template, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Clean trailing commas
	result = CleanJSONTrailingCommas(result)

	// Try to parse as JSON to verify it's valid
	var parsed map[string]any
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Errorf("result is not valid JSON: %v\nResult:\n%s", err, result)
	}

	// Verify some values
	if parsed["latitude"] != 49.815531100541534 {
		t.Errorf("expected latitude 49.815531100541534, got %v", parsed["latitude"])
	}

	markers, ok := parsed["markers"].([]any)
	if !ok {
		t.Fatalf("markers is not an array")
	}
	if len(markers) != 3 { // 2 from loop + 1 static
		t.Errorf("expected 3 markers, got %d", len(markers))
	}
}

func TestCleanJSONTrailingCommas(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{`[1, 2, 3,]`, `[1, 2, 3]`},
		{`{"a": 1,}`, `{"a": 1}`},
		{`[1, 2, 3, ]`, `[1, 2, 3 ]`},
		{`{"a": 1, }`, `{"a": 1 }`},
		{`[1,
]`, `[1
]`},
		{`{"a": [1, 2,], "b": 3,}`, `{"a": [1, 2], "b": 3}`},
	}

	for _, tt := range tests {
		result := CleanJSONTrailingCommas(tt.input)
		if result != tt.expected {
			t.Errorf("CleanJSONTrailingCommas(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestLeafRenderer_Index(t *testing.T) {
	ctx := map[string]any{
		"coords": []any{49.815, 19.025},
	}

	template := "[#index(coords,0), #index(coords,1)]"
	result, err := RenderLeafTemplate(template, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := "[49.815, 19.025]"
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestLeafRenderer_IndexVariable(t *testing.T) {
	ctx := map[string]any{
		"items": []any{"a", "b", "c"},
	}

	template := "#for(item in items):#(index):#(item),#endfor"
	result, err := RenderLeafTemplate(template, ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := "0:a,1:b,2:c,"
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestLeafRenderer_ElseIf(t *testing.T) {
	tests := []struct {
		name     string
		ctx      map[string]any
		template string
		expected string
	}{
		{
			name:     "elseif first condition true",
			ctx:      map[string]any{"value": "a"},
			template: `#if(value == "a"):first#elseif(value == "b"):second#else:third#endif`,
			expected: "first",
		},
		{
			name:     "elseif second condition true",
			ctx:      map[string]any{"value": "b"},
			template: `#if(value == "a"):first#elseif(value == "b"):second#else:third#endif`,
			expected: "second",
		},
		{
			name:     "elseif else branch",
			ctx:      map[string]any{"value": "c"},
			template: `#if(value == "a"):first#elseif(value == "b"):second#else:third#endif`,
			expected: "third",
		},
		{
			name:     "multiple elseif",
			ctx:      map[string]any{"value": "c"},
			template: `#if(value == "a"):first#elseif(value == "b"):second#elseif(value == "c"):third#else:fourth#endif`,
			expected: "third",
		},
		{
			name:     "elseif with nil check",
			ctx:      map[string]any{"old": nil, "nearbyStops": []any{"stop1"}},
			template: `#if(old != nil):old#elseif(nearbyStops != nil):nearby#else:none#endif`,
			expected: "nearby",
		},
		{
			name:     "elseif without else",
			ctx:      map[string]any{"value": "b"},
			template: `#if(value == "a"):first#elseif(value == "b"):second#endif`,
			expected: "second",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := RenderLeafTemplate(tt.template, tt.ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}
