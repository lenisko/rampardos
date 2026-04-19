package utils

import (
	"testing"
)

func TestLeafToJetConverter(t *testing.T) {
	tests := []struct {
		name     string
		leaf     string
		expected string
	}{
		{
			name:     "simple variable",
			leaf:     `#(variable)`,
			expected: `{{ variable }}`,
		},
		{
			name:     "nested property",
			leaf:     `#(object.property)`,
			expected: `{{ object.property }}`,
		},
		{
			name:     "if not nil",
			leaf:     `#if(var != nil):content#endif`,
			expected: `{{ if isset(var) }}content{{ end }}`,
		},
		{
			name:     "if nil",
			leaf:     `#if(var == nil):content#endif`,
			expected: `{{ if !isset(var) }}content{{ end }}`,
		},
		{
			name:     "if not nil nested property",
			leaf:     `#if(obj.prop != nil):content#endif`,
			expected: `{{ if isset(obj.prop) }}content{{ end }}`,
		},
		{
			name:     "if else",
			leaf:     `#if(var != nil):yes#else:no#endif`,
			expected: `{{ if isset(var) }}yes{{ else }}no{{ end }}`,
		},
		{
			name:     "boolean and with nil check",
			leaf:     `#if(a != nil && b == "x"):ok#endif`,
			expected: `{{ if isset(a) && b == "x" }}ok{{ end }}`,
		},
		{
			name:     "if elseif else",
			leaf:     `#if(a == "x"):A#elseif(b == "y"):B#else:C#endif`,
			expected: `{{ if a == "x" }}A{{ else if b == "y" }}B{{ else }}C{{ end }}`,
		},
		{
			name:     "multiple elseif",
			leaf:     `#if(type == "gym"):gym#elseif(type == "pokestop"):stop#elseif(type == "pokemon"):mon#else:unknown#endif`,
			expected: `{{ if type == "gym" }}gym{{ else if type == "pokestop" }}stop{{ else if type == "pokemon" }}mon{{ else }}unknown{{ end }}`,
		},
		{
			name:     "for loop",
			leaf:     `#for(item in items):#(item.name)#endfor`,
			expected: `{{ range i, item := items }}{{ item.name }}{{ end }}`,
		},
		{
			name:     "for loop simple",
			leaf:     `#for(item in items):#(item),#endfor`,
			expected: `{{ range i, item := items }}{{ item }},{{ end }}`,
		},
		{
			name:     "for loop with index variable",
			leaf:     `#for(item in items):#(index):#(item)#endfor`,
			expected: `{{ range i, item := items }}{{ i }}:{{ item }}{{ end }}`,
		},
		{
			name:     "for loop with index condition",
			leaf:     `#for(item in items):#if(index != 0):,#endif#(item)#endfor`,
			expected: `{{ range i, item := items }}{{ if i != 0 }},{{ end }}{{ item }}{{ end }}`,
		},
		{
			name:     "escaped hash",
			leaf:     `\#not-a-directive`,
			expected: `#not-a-directive`,
		},
		{
			name:     "index access",
			leaf:     `#index(array, 0)`,
			expected: `{{ index(array, 0) }}`,
		},
		{
			name:     "complex template",
			leaf:     `{"name": "#(name)", "items": [#for(item in items):{"id": "#(item.id)"}#endfor]}`,
			expected: `{"name": "{{ name }}", "items": [{{ range i, item := items }}{"id": "{{ item.id }}"}{{ end }}]}`,
		},
	}

	converter := NewLeafToJetConverter()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := converter.Convert(tt.leaf)
			if result != tt.expected {
				t.Errorf("\nexpected: %q\ngot:      %q", tt.expected, result)
			}
		})
	}
}
