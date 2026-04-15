package utils

import (
	"regexp"
	"strings"
)

// Pre-compiled regexes for Leaf to Jet conversion
var (
	jetVarRegex    = regexp.MustCompile(`#\(([^)]+)\)`)
	jetForPattern  = regexp.MustCompile(`#for\s*\(\s*(\w+)\s+in\s+(\w+(?:\.\w+)*)\s*\)\s*:`)
	jetIndexRegex  = regexp.MustCompile(`#index\s*\(\s*([^,]+)\s*,\s*([^)]+)\s*\)`)
	jetIndexWordRe = regexp.MustCompile(`\bindex\b`)
	// Jet's `x != nil` does not safeguard against undefined identifiers —
	// accessing an unset context key raises at render time. `isset(x)`
	// is the idiomatic check and also covers the nil case.
	jetNotNilRegex = regexp.MustCompile(`(\w+(?:\.\w+)*)\s*!=\s*nil`)
	jetIsNilRegex  = regexp.MustCompile(`(\w+(?:\.\w+)*)\s*==\s*nil`)
)

// LeafToJetConverter converts Leaf template syntax to Jet template syntax
type LeafToJetConverter struct{}

// NewLeafToJetConverter creates a new converter
func NewLeafToJetConverter() *LeafToJetConverter {
	return &LeafToJetConverter{}
}

// Convert converts a Leaf template to Jet syntax
func (c *LeafToJetConverter) Convert(leaf string) string {
	result := leaf

	// Process #for loops: #for(item in array): ... #endfor -> {{ range _, item := array }}...{{ end }}
	result = c.convertForLoops(result)

	// Process #if/#elseif/#else/#endif blocks
	result = c.convertIfStatements(result)

	// Process simple variable substitutions: #(var) -> {{ var }}
	result = c.convertVariables(result)

	// Process #index(array, idx) -> {{ index(array, idx) }}
	result = c.convertIndex(result)

	// Clean up escaped hash: \# -> #
	result = strings.ReplaceAll(result, `\#`, `#`)

	return result
}

// convertVariables handles #(variable) and #(object.property) -> {{ variable }} or {{ object.property }}
func (c *LeafToJetConverter) convertVariables(template string) string {
	result := jetVarRegex.ReplaceAllStringFunc(template, func(match string) string {
		varName := match[2 : len(match)-1] // Extract variable name from #(varName)
		// Convert Leaf's magic "index" variable to Jet's "i" (from range loop)
		if varName == "index" {
			return "{{ i }}"
		}
		return "{{ " + varName + " }}"
	})
	return result
}

// convertForLoops handles #for(item in array): ... #endfor -> {{ range _, item := array }}...{{ end }}
func (c *LeafToJetConverter) convertForLoops(template string) string {

	result := template
	for {
		match := jetForPattern.FindStringSubmatchIndex(result)
		if match == nil {
			break
		}

		// Extract item name and array name
		itemName := result[match[2]:match[3]]
		arrayName := result[match[4]:match[5]]

		// Find matching #endfor
		forStart := match[0]
		forEnd := match[1]
		endforPos := c.findMatchingEndfor(result[forEnd:])
		if endforPos == -1 {
			break
		}
		endforPos += forEnd

		// Extract body
		body := result[forEnd:endforPos]

		// Build Jet range loop (use i for index so it's available in the loop body)
		jetLoop := "{{ range i, " + itemName + " := " + arrayName + " }}" + body + "{{ end }}"

		result = result[:forStart] + jetLoop + result[endforPos+7:] // 7 = len("#endfor")
	}

	return result
}

// findMatchingEndfor finds the position of the matching #endfor
func (c *LeafToJetConverter) findMatchingEndfor(s string) int {
	depth := 1
	i := 0
	for i < len(s) {
		if strings.HasPrefix(s[i:], "#for(") {
			depth++
			i += 5
		} else if strings.HasPrefix(s[i:], "#endfor") {
			depth--
			if depth == 0 {
				return i
			}
			i += 7
		} else {
			i++
		}
	}
	return -1
}

// convertIfStatements handles #if/#elseif/#else/#endif
func (c *LeafToJetConverter) convertIfStatements(template string) string {
	result := template

	for {
		ifStart := strings.Index(result, "#if(")
		if ifStart == -1 {
			break
		}

		// Find matching #endif
		depth := 0
		pos := ifStart
		endifPos := -1
		for pos < len(result) {
			if strings.HasPrefix(result[pos:], "#if(") {
				depth++
				pos += 4
			} else if strings.HasPrefix(result[pos:], "#endif") {
				depth--
				if depth == 0 {
					endifPos = pos
					break
				}
				pos += 6
			} else {
				pos++
			}
		}

		if endifPos == -1 {
			break
		}

		// Extract the full if block
		fullBlock := result[ifStart : endifPos+6]

		// Parse condition
		condEnd := strings.Index(fullBlock, "):")
		if condEnd == -1 {
			break
		}
		condition := fullBlock[4:condEnd]
		body := fullBlock[condEnd+2 : len(fullBlock)-6]

		// Convert the if block to Jet
		jetBlock := c.convertIfBlock(condition, body)

		result = result[:ifStart] + jetBlock + result[endifPos+6:]
	}

	return result
}

// convertIfBlock converts a single if/elseif/else block to Jet
func (c *LeafToJetConverter) convertIfBlock(condition, body string) string {
	// Convert condition to Jet syntax
	jetCondition := c.convertCondition(condition)

	// Find #elseif and #else at depth 0
	parts := c.splitIfBody(body)

	var result strings.Builder
	result.WriteString("{{ if ")
	result.WriteString(jetCondition)
	result.WriteString(" }}")
	result.WriteString(parts.thenBlock)

	for i, elseifCond := range parts.elseifConditions {
		jetElseifCond := c.convertCondition(elseifCond)
		result.WriteString("{{ else if ")
		result.WriteString(jetElseifCond)
		result.WriteString(" }}")
		if i < len(parts.elseifBlocks) {
			result.WriteString(parts.elseifBlocks[i])
		}
	}

	if parts.elseBlock != "" {
		result.WriteString("{{ else }}")
		result.WriteString(parts.elseBlock)
	}

	result.WriteString("{{ end }}")

	return result.String()
}

// convertCondition converts Leaf condition to Jet condition
func (c *LeafToJetConverter) convertCondition(condition string) string {
	condition = strings.TrimSpace(condition)

	condition = jetNotNilRegex.ReplaceAllString(condition, "isset($1)")
	condition = jetIsNilRegex.ReplaceAllString(condition, "!isset($1)")

	// Convert Leaf's magic "index" variable to Jet's "i" (from range loop)
	// Use word boundary matching to avoid replacing "index" in "indexOf" etc.
	condition = jetIndexWordRe.ReplaceAllString(condition, "i")

	return condition
}

type jetIfParts struct {
	thenBlock        string
	elseifConditions []string
	elseifBlocks     []string
	elseBlock        string
}

// splitIfBody splits the body into then, elseif, and else parts
func (c *LeafToJetConverter) splitIfBody(body string) jetIfParts {
	parts := jetIfParts{}
	depth := 0
	currentBlockStart := 0
	i := 0

	for i < len(body) {
		if strings.HasPrefix(body[i:], "#if(") {
			depth++
			i += 4
		} else if strings.HasPrefix(body[i:], "#endif") {
			depth--
			i += 6
		} else if depth == 0 && strings.HasPrefix(body[i:], "#elseif(") {
			// Found #elseif at depth 0
			if currentBlockStart == 0 {
				parts.thenBlock = body[:i]
			} else {
				parts.elseifBlocks = append(parts.elseifBlocks, body[currentBlockStart:i])
			}

			// Parse the elseif condition
			condStart := i + 8 // len("#elseif(")
			condEnd := strings.Index(body[condStart:], "):")
			if condEnd == -1 {
				i++
				continue
			}
			parts.elseifConditions = append(parts.elseifConditions, body[condStart:condStart+condEnd])
			currentBlockStart = condStart + condEnd + 2 // after "):"
			i = currentBlockStart
		} else if depth == 0 && strings.HasPrefix(body[i:], "#else:") {
			// Found #else at depth 0
			if currentBlockStart == 0 {
				parts.thenBlock = body[:i]
			} else {
				parts.elseifBlocks = append(parts.elseifBlocks, body[currentBlockStart:i])
			}
			parts.elseBlock = body[i+6:]
			return parts
		} else {
			i++
		}
	}

	// No #else found
	if currentBlockStart == 0 {
		parts.thenBlock = body
	} else if len(parts.elseifConditions) > len(parts.elseifBlocks) {
		parts.elseifBlocks = append(parts.elseifBlocks, body[currentBlockStart:])
	}

	return parts
}

// convertIndex handles #index(array, idx) -> {{ index(array, idx) }}
func (c *LeafToJetConverter) convertIndex(template string) string {
	return jetIndexRegex.ReplaceAllStringFunc(template, func(match string) string {
		parts := jetIndexRegex.FindStringSubmatch(match)
		if len(parts) == 3 {
			array := strings.TrimSpace(parts[1])
			idx := strings.TrimSpace(parts[2])
			return "{{ index(" + array + ", " + idx + ") }}"
		}
		return match
	})
}
