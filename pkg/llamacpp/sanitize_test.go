package llamacpp

import (
	"encoding/json"
	"testing"

	"github.com/auxothq/auxot/pkg/openai"
)

func TestSanitizeTools(t *testing.T) {
	tests := []struct {
		name     string
		input    []openai.Tool
		wantErr  bool
		validate func(t *testing.T, output []openai.Tool)
	}{
		{
			name: "strips extra keys from properties, keeps type/description/enum/required",
			input: []openai.Tool{
				{
					Type: "function",
					Function: openai.ToolFunction{
						Name:        "test_tool",
						Description: "A test tool",
						Parameters: json.RawMessage(`{
							"type": "object",
							"properties": {
								"name": {
									"type": "string",
									"description": "The name",
									"default": "hello",
									"minLength": 2
								},
								"count": {
									"type": "integer",
									"description": "A count",
									"minimum": 0,
									"maximum": 100,
									"exclusiveMinimum": 0
								},
								"mode": {
									"type": "string",
									"description": "The mode",
									"enum": ["fast", "slow"]
								}
							},
							"required": ["name"]
						}`),
					},
				},
			},
			wantErr: false,
			validate: func(t *testing.T, output []openai.Tool) {
				if len(output) != 1 {
					t.Fatalf("expected 1 tool, got %d", len(output))
				}

				var params map[string]any
				if err := json.Unmarshal(output[0].Function.Parameters, &params); err != nil {
					t.Fatalf("failed to unmarshal parameters: %v", err)
				}

				props := params["properties"].(map[string]any)

				// name: should keep type+description, strip default+minLength
				nameProp := props["name"].(map[string]any)
				if nameProp["type"] != "string" {
					t.Error("name: type should be preserved")
				}
				if nameProp["description"] != "The name" {
					t.Error("name: description should be preserved")
				}
				if _, has := nameProp["default"]; has {
					t.Error("name: default should be stripped")
				}
				if _, has := nameProp["minLength"]; has {
					t.Error("name: minLength should be stripped")
				}

				// count: should keep type+description, strip minimum/maximum/exclusiveMinimum
				countProp := props["count"].(map[string]any)
				if countProp["type"] != "integer" {
					t.Error("count: type should be preserved")
				}
				if _, has := countProp["minimum"]; has {
					t.Error("count: minimum should be stripped")
				}
				if _, has := countProp["maximum"]; has {
					t.Error("count: maximum should be stripped")
				}
				if _, has := countProp["exclusiveMinimum"]; has {
					t.Error("count: exclusiveMinimum should be stripped")
				}

				// mode: should keep type+description+enum
				modeProp := props["mode"].(map[string]any)
				if modeProp["type"] != "string" {
					t.Error("mode: type should be preserved")
				}
				enumArr, ok := modeProp["enum"].([]any)
				if !ok || len(enumArr) != 2 {
					t.Error("mode: enum should be preserved with 2 values")
				}

				// required should be preserved at top level
				req := params["required"].([]any)
				if len(req) != 1 || req[0] != "name" {
					t.Error("required should be preserved")
				}
			},
		},
		{
			name: "strips $schema and additionalProperties from top level",
			input: []openai.Tool{
				{
					Type: "function",
					Function: openai.ToolFunction{
						Name:        "test_tool",
						Description: "A tool",
						Parameters: json.RawMessage(`{
							"$schema": "https://json-schema.org/draft/2020-12/schema",
							"type": "object",
							"properties": {
								"x": {"type": "string"}
							},
							"required": ["x"],
							"additionalProperties": false
						}`),
					},
				},
			},
			wantErr: false,
			validate: func(t *testing.T, output []openai.Tool) {
				var params map[string]any
				if err := json.Unmarshal(output[0].Function.Parameters, &params); err != nil {
					t.Fatalf("failed to unmarshal: %v", err)
				}
				if _, has := params["$schema"]; has {
					t.Error("$schema should be stripped from top level")
				}
				if _, has := params["additionalProperties"]; has {
					t.Error("additionalProperties should be stripped from top level")
				}
				if params["type"] != "object" {
					t.Error("type should be preserved")
				}
			},
		},
		{
			name: "strips nested object properties (items, properties, anyOf, etc)",
			input: []openai.Tool{
				{
					Type: "function",
					Function: openai.ToolFunction{
						Name:        "nested_tool",
						Description: "A tool with nested schemas",
						Parameters: json.RawMessage(`{
							"type": "object",
							"properties": {
								"tags": {
									"type": "array",
									"description": "Tags",
									"items": {"type": "string"},
									"minItems": 1,
									"maxItems": 10
								},
								"config": {
									"type": "object",
									"description": "Config",
									"properties": {"a": {"type": "string"}},
									"additionalProperties": false
								},
								"status": {
									"description": "Status",
									"anyOf": [
										{"type": "string", "enum": ["a","b"]},
										{"type": "string", "const": "c"}
									]
								}
							}
						}`),
					},
				},
			},
			wantErr: false,
			validate: func(t *testing.T, output []openai.Tool) {
				var params map[string]any
				if err := json.Unmarshal(output[0].Function.Parameters, &params); err != nil {
					t.Fatalf("failed to unmarshal: %v", err)
				}
				props := params["properties"].(map[string]any)

				// tags: keep type+description, strip items/minItems/maxItems
				tagsProp := props["tags"].(map[string]any)
				if tagsProp["type"] != "array" {
					t.Error("tags: type should be preserved")
				}
				if _, has := tagsProp["items"]; has {
					t.Error("tags: items should be stripped")
				}
				if _, has := tagsProp["minItems"]; has {
					t.Error("tags: minItems should be stripped")
				}

				// config: keep type+description, strip properties/additionalProperties
				configProp := props["config"].(map[string]any)
				if configProp["type"] != "object" {
					t.Error("config: type should be preserved")
				}
				if _, has := configProp["properties"]; has {
					t.Error("config: nested properties should be stripped")
				}

				// status: no type originally, should default to "string"; strip anyOf
				statusProp := props["status"].(map[string]any)
				if statusProp["type"] != "string" {
					t.Errorf("status: should default to 'string' when no type, got %v", statusProp["type"])
				}
				if _, has := statusProp["anyOf"]; has {
					t.Error("status: anyOf should be stripped")
				}
			},
		},
		{
			name: "handles empty tools array",
			input: []openai.Tool{},
			wantErr: false,
			validate: func(t *testing.T, output []openai.Tool) {
				if len(output) != 0 {
					t.Errorf("expected empty array, got %d tools", len(output))
				}
			},
		},
		{
			name: "handles tool with no parameters",
			input: []openai.Tool{
				{
					Type: "function",
					Function: openai.ToolFunction{
						Name:        "no_params_tool",
						Description: "A tool with no parameters",
						Parameters:  json.RawMessage{},
					},
				},
			},
			wantErr: false,
			validate: func(t *testing.T, output []openai.Tool) {
				if len(output) != 1 {
					t.Fatalf("expected 1 tool, got %d", len(output))
				}
				if output[0].Function.Name != "no_params_tool" {
					t.Errorf("expected tool name to be preserved")
				}
			},
		},
		{
			name: "strips format, propertyNames, and boolean additionalProperties from properties",
			input: []openai.Tool{
				{
					Type: "function",
					Function: openai.ToolFunction{
						Name:        "complex_tool",
						Description: "Complex tool",
						Parameters: json.RawMessage(`{
							"type": "object",
							"properties": {
								"url": {
									"type": "string",
									"description": "A URL",
									"format": "uri"
								},
								"metadata": {
									"type": "object",
									"description": "Metadata",
									"propertyNames": {"type": "string"},
									"additionalProperties": {}
								}
							}
						}`),
					},
				},
			},
			wantErr: false,
			validate: func(t *testing.T, output []openai.Tool) {
				var params map[string]any
				if err := json.Unmarshal(output[0].Function.Parameters, &params); err != nil {
					t.Fatalf("failed to unmarshal: %v", err)
				}
				props := params["properties"].(map[string]any)

				urlProp := props["url"].(map[string]any)
				if _, has := urlProp["format"]; has {
					t.Error("url: format should be stripped")
				}
				if urlProp["type"] != "string" {
					t.Error("url: type should be preserved")
				}

				metaProp := props["metadata"].(map[string]any)
				if _, has := metaProp["propertyNames"]; has {
					t.Error("metadata: propertyNames should be stripped")
				}
				if _, has := metaProp["additionalProperties"]; has {
					t.Error("metadata: additionalProperties should be stripped")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, err := SanitizeTools(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("SanitizeTools() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.validate != nil {
				tt.validate(t, output)
			}
		})
	}
}
