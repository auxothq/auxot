// Package llamacpp provides an HTTP client for the llama.cpp server.
//
// sanitize.go contains utilities to clean tool schemas before sending them
// to llama.cpp, preventing Jinja template errors caused by unsupported keys.
package llamacpp

import (
	"encoding/json"
	"fmt"

	"github.com/auxothq/auxot/pkg/openai"
)

// allowedPropertyKeys are the ONLY keys the llama.cpp Unsloth Jinja chat template
// handles inside each property definition. Any other key triggers the `replace`
// Jinja filter which does not exist in llama.cpp's minja engine, causing:
//
//	"Value is not callable: null"
//
// The template's handled_keys list is: ['type', 'description', 'enum', 'required']
// These are the only keys that DON'T trigger the broken code path.
var allowedPropertyKeys = map[string]bool{
	"type":        true,
	"description": true,
	"enum":        true,
	"required":    true,
}

// SanitizeTools strips tool parameter schemas to only the keys that llama.cpp's
// Jinja template can handle. The minja engine lacks the `replace` filter, so any
// JSON Schema keyword beyond type/description/enum/required crashes the template.
//
// This strips: default, minimum, maximum, format, items, properties,
// additionalProperties, anyOf, oneOf, allOf, $schema, minLength, maxLength,
// minItems, maxItems, exclusiveMinimum, exclusiveMaximum, const, propertyNames,
// and any other non-handled keys.
func SanitizeTools(tools []openai.Tool) ([]openai.Tool, error) {
	if len(tools) == 0 {
		return tools, nil
	}

	sanitized := make([]openai.Tool, 0, len(tools))
	for _, tool := range tools {
		cleaned, err := sanitizeTool(tool)
		if err != nil {
			return nil, fmt.Errorf("sanitizing tool %q: %w", tool.Function.Name, err)
		}
		sanitized = append(sanitized, cleaned)
	}

	return sanitized, nil
}

// sanitizeTool cleans a single tool's parameter schema.
func sanitizeTool(tool openai.Tool) (openai.Tool, error) {
	if len(tool.Function.Parameters) == 0 {
		return tool, nil
	}

	var params map[string]any
	if err := json.Unmarshal(tool.Function.Parameters, &params); err != nil {
		return tool, fmt.Errorf("unmarshaling parameters: %w", err)
	}

	// Build a clean parameters object with only what the template uses
	cleaned := make(map[string]any)

	// Keep "type" (always "object" at top level)
	if v, ok := params["type"]; ok {
		cleaned["type"] = v
	}

	// Sanitize properties: strip each property to allowed keys only
	if props, ok := params["properties"].(map[string]any); ok {
		cleanedProps := make(map[string]any)
		for propName, propDef := range props {
			if propMap, ok := propDef.(map[string]any); ok {
				cleanedProps[propName] = stripToAllowedKeys(propMap)
			}
			// Skip non-map property definitions (shouldn't happen)
		}
		cleaned["properties"] = cleanedProps
	}

	// Keep "required" array as-is (it's handled by the template)
	if req, ok := params["required"]; ok {
		cleaned["required"] = req
	}

	// Re-marshal
	cleanedBytes, err := json.Marshal(cleaned)
	if err != nil {
		return tool, fmt.Errorf("re-marshaling parameters: %w", err)
	}

	tool.Function.Parameters = cleanedBytes
	return tool, nil
}

// stripToAllowedKeys removes all keys from a property definition except those
// in the template's handled_keys list: type, description, enum, required.
func stripToAllowedKeys(propDef map[string]any) map[string]any {
	stripped := make(map[string]any)
	for key, value := range propDef {
		if allowedPropertyKeys[key] {
			stripped[key] = value
		}
	}
	// Ensure "type" exists (default to "string" if missing)
	if _, ok := stripped["type"]; !ok {
		stripped["type"] = "string"
	}
	return stripped
}
