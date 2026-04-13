package utils

import (
	"fmt"
	"maps"
	"regexp"
	"strconv"
	"strings"
)

// Pre-compiled regexes for performance
var (
	leafVarRegex           = regexp.MustCompile(`#\(([^)]+)\)`)
	leafIndexRegex         = regexp.MustCompile(`#index\((\w+),\s*(\d+)\)`)
	jsonTrailingCommaArray = regexp.MustCompile(`,(\s*)\]`)
	jsonTrailingCommaObj   = regexp.MustCompile(`,(\s*)\}`)
)

// LeafRenderer renders Leaf templates with context
type LeafRenderer struct {
	context map[string]any
}

// NewLeafRenderer creates a new Leaf renderer
func NewLeafRenderer(context map[string]any) *LeafRenderer {
	return &LeafRenderer{context: context}
}

// Render renders a Leaf template string
func (r *LeafRenderer) Render(template string) (string, error) {
	result := template

	// Process #for loops first (they can contain other directives)
	result = r.processForLoops(result)

	// Process #if conditionals
	result = r.processIfStatements(result)

	// Process simple variable substitutions #(var)
	result = r.processVariables(result)

	// Process #index(array, idx)
	result = r.processIndex(result)

	return result, nil
}

// processVariables handles #(variable) and #(object.property) syntax
func (r *LeafRenderer) processVariables(template string) string {
	return leafVarRegex.ReplaceAllStringFunc(template, func(match string) string {
		// Extract variable name
		varName := match[2 : len(match)-1]
		value := r.getValue(varName)
		return r.formatValue(value)
	})
}

// processIfStatements handles #if(condition): ... #elseif(condition): ... #else: ... #endif
func (r *LeafRenderer) processIfStatements(template string) string {
	// Process from innermost to outermost by finding #if without nested #if
	for {
		// Find the first #if
		ifStart := strings.Index(template, "#if(")
		if ifStart == -1 {
			break
		}

		// Find matching #endif by counting nesting
		depth := 0
		pos := ifStart
		endifPos := -1
		for pos < len(template) {
			if strings.HasPrefix(template[pos:], "#if(") {
				depth++
				pos += 4
			} else if strings.HasPrefix(template[pos:], "#endif") {
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
		fullBlock := template[ifStart : endifPos+6]

		// Parse condition
		condEnd := strings.Index(fullBlock, "):")
		if condEnd == -1 {
			break
		}
		condition := fullBlock[4:condEnd]

		// Find #elseif, #else at same nesting level
		body := fullBlock[condEnd+2 : len(fullBlock)-6]

		// Parse the if/elseif/else chain
		replacement := r.processIfChain(condition, body)

		// Recursively process the replacement
		replacement = r.processIfStatements(replacement)

		template = template[:ifStart] + replacement + template[endifPos+6:]
	}

	return template
}

// processIfChain processes an if/elseif/else chain and returns the matching block
func (r *LeafRenderer) processIfChain(condition string, body string) string {
	// Find #elseif and #else at depth 0
	depth := 0
	thenBlock := body
	elseifConditions := []string{}
	elseifBlocks := []string{}
	elseBlock := ""

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
				// This ends the "then" block
				thenBlock = body[:i]
			} else {
				// This ends a previous elseif block
				elseifBlocks = append(elseifBlocks, body[currentBlockStart:i])
			}

			// Parse the elseif condition
			condStart := i + 8 // len("#elseif(")
			condEnd := strings.Index(body[condStart:], "):")
			if condEnd == -1 {
				i++
				continue
			}
			elseifConditions = append(elseifConditions, body[condStart:condStart+condEnd])
			currentBlockStart = condStart + condEnd + 2 // after "):"
			i = currentBlockStart
		} else if depth == 0 && strings.HasPrefix(body[i:], "#else:") {
			// Found #else at depth 0
			if currentBlockStart == 0 {
				// This ends the "then" block
				thenBlock = body[:i]
			} else {
				// This ends a previous elseif block
				elseifBlocks = append(elseifBlocks, body[currentBlockStart:i])
			}
			elseBlock = body[i+6:]
			break
		} else {
			i++
		}
	}

	// If we didn't find #else, the remaining body after the last elseif is the last elseif block
	if elseBlock == "" && len(elseifConditions) > len(elseifBlocks) {
		elseifBlocks = append(elseifBlocks, body[currentBlockStart:])
	}

	// Evaluate the chain
	if r.evaluateCondition(condition) {
		return strings.TrimSpace(thenBlock)
	}

	for idx, elseifCond := range elseifConditions {
		if r.evaluateCondition(elseifCond) {
			if idx < len(elseifBlocks) {
				return strings.TrimSpace(elseifBlocks[idx])
			}
			return ""
		}
	}

	return strings.TrimSpace(elseBlock)
}

// processForLoops handles #for(item in collection): ... #endfor
func (r *LeafRenderer) processForLoops(template string) string {
	for {
		// Find the first #for
		forStart := strings.Index(template, "#for(")
		if forStart == -1 {
			break
		}

		// Find matching #endfor by counting nesting
		depth := 0
		pos := forStart
		endforPos := -1
		for pos < len(template) {
			if strings.HasPrefix(template[pos:], "#for(") {
				depth++
				pos += 5
			} else if strings.HasPrefix(template[pos:], "#endfor") {
				depth--
				if depth == 0 {
					endforPos = pos
					break
				}
				pos += 7
			} else {
				pos++
			}
		}

		if endforPos == -1 {
			break
		}

		// Extract the full for block
		fullBlock := template[forStart : endforPos+7]

		// Parse "item in collection"
		headerEnd := strings.Index(fullBlock, "):")
		if headerEnd == -1 {
			break
		}
		header := fullBlock[5:headerEnd]
		parts := strings.Split(header, " in ")
		if len(parts) != 2 {
			break
		}
		itemName := strings.TrimSpace(parts[0])
		collectionName := strings.TrimSpace(parts[1])

		loopBody := fullBlock[headerEnd+2 : len(fullBlock)-7]

		collection := r.getValue(collectionName)
		var result strings.Builder

		if arr, ok := collection.([]any); ok {
			for idx, item := range arr {
				// Create a new context with the loop variable
				oldContext := r.context
				r.context = make(map[string]any)
				maps.Copy(r.context, oldContext)
				r.context[itemName] = item
				r.context["index"] = idx

				// Process the loop body recursively
				processed := r.processForLoops(loopBody)
				processed = r.processIfStatements(processed)
				processed = r.processVariables(processed)
				processed = r.processIndex(processed)
				result.WriteString(processed)

				r.context = oldContext
			}
		}

		template = template[:forStart] + result.String() + template[endforPos+7:]
	}

	return template
}

// processIndex handles #index(array, idx) syntax
func (r *LeafRenderer) processIndex(template string) string {
	return leafIndexRegex.ReplaceAllStringFunc(template, func(match string) string {
		parts := leafIndexRegex.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}
		arrName := parts[1]
		idx, _ := strconv.Atoi(parts[2])

		value := r.getValue(arrName)
		if arr, ok := value.([]any); ok && idx < len(arr) {
			return r.formatValue(arr[idx])
		}
		return match
	})
}

// getValue retrieves a value from context, supporting dot notation
func (r *LeafRenderer) getValue(path string) any {
	parts := strings.Split(path, ".")
	var current any = r.context

	for _, part := range parts {
		switch v := current.(type) {
		case map[string]any:
			current = v[part]
		default:
			return nil
		}
	}

	return current
}

// evaluateCondition evaluates a condition expression
func (r *LeafRenderer) evaluateCondition(condition string) bool {
	condition = strings.TrimSpace(condition)

	// Handle && (AND) - split and evaluate both sides
	if strings.Contains(condition, " && ") {
		parts := strings.SplitN(condition, " && ", 2)
		if len(parts) == 2 {
			return r.evaluateCondition(parts[0]) && r.evaluateCondition(parts[1])
		}
	}

	// Handle || (OR) - split and evaluate both sides
	if strings.Contains(condition, " || ") {
		parts := strings.SplitN(condition, " || ", 2)
		if len(parts) == 2 {
			return r.evaluateCondition(parts[0]) || r.evaluateCondition(parts[1])
		}
	}

	// Handle != nil check
	if before, ok := strings.CutSuffix(condition, " != nil"); ok {
		varName := before
		value := r.getValue(varName)
		return value != nil
	}

	// Handle == nil check
	if before, ok := strings.CutSuffix(condition, " == nil"); ok {
		varName := before
		value := r.getValue(varName)
		return value == nil
	}

	// Handle == comparison
	if strings.Contains(condition, " == ") {
		parts := strings.SplitN(condition, " == ", 2)
		if len(parts) == 2 {
			left := r.getValue(strings.TrimSpace(parts[0]))
			right := strings.Trim(strings.TrimSpace(parts[1]), "\"")
			return fmt.Sprintf("%v", left) == right
		}
	}

	// Handle != comparison
	if strings.Contains(condition, " != ") {
		parts := strings.SplitN(condition, " != ", 2)
		if len(parts) == 2 {
			left := r.getValue(strings.TrimSpace(parts[0]))
			right := strings.Trim(strings.TrimSpace(parts[1]), "\"")
			return fmt.Sprintf("%v", left) != right
		}
	}

	// Simple truthy check
	value := r.getValue(condition)
	if value == nil {
		return false
	}
	if b, ok := value.(bool); ok {
		return b
	}
	if s, ok := value.(string); ok {
		return s != ""
	}
	if arr, ok := value.([]any); ok {
		return len(arr) > 0
	}
	return true
}

// formatValue formats a value for output
func (r *LeafRenderer) formatValue(value any) string {
	if value == nil {
		return "null"
	}
	switch v := value.(type) {
	case string:
		return v
	case float64:
		// Check if it's a whole number
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case bool:
		return strconv.FormatBool(v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// RenderLeafTemplate is a convenience function to render a Leaf template
func RenderLeafTemplate(template string, context map[string]any) (string, error) {
	renderer := NewLeafRenderer(context)
	return renderer.Render(template)
}

// CleanJSONTrailingCommas removes trailing commas before ] and } in JSON
// This is needed because Leaf templates often produce trailing commas after loops
func CleanJSONTrailingCommas(json string) string {
	// Remove trailing commas before ] (with optional whitespace)
	json = jsonTrailingCommaArray.ReplaceAllString(json, "$1]")

	// Remove trailing commas before } (with optional whitespace)
	json = jsonTrailingCommaObj.ReplaceAllString(json, "$1}")

	// Convert escaped hash \# to just # (Leaf escape for # that JSON doesn't understand)
	json = strings.ReplaceAll(json, `\#`, `#`)

	return json
}
