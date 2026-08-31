package llm

import (
	"encoding/json"
	"github.com/kasuganosora/thinkbot/util/errs"
	"reflect"
)

// resolveSchema converts a Tool's Parameters value into a standard JSON Schema
// representation (map[string]any).
//
// Accepted types:
//   - nil: returns nil
//   - map[string]any: returned as-is
//   - any other type: marshaled to JSON then unmarshaled into map[string]any
//
// For automatic struct-to-schema inference, use NewTool which leverages
// encoding/json reflection to produce a basic schema from struct tags.
func resolveSchema(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	if m, ok := v.(map[string]any); ok {
		return m, nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, errs.Wrap(err, "llm: marshal tool schema")
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, errs.Wrap(err, "llm: unmarshal tool schema")
	}
	return m, nil
}

// inferStructSchema produces a basic JSON Schema from a Go struct type using
// reflection and json struct tags. This is a lightweight alternative to pulling
// in a full JSON Schema inference library.
func inferStructSchema(t reflect.Type) map[string]any {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return map[string]any{"type": "object"}
	}

	properties := make(map[string]any)
	var required []string

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}

		jsonTag := field.Tag.Get("json")
		if jsonTag == "-" {
			continue
		}

		name := field.Name
		omitempty := false
		if jsonTag != "" {
			parts := splitJSONTag(jsonTag)
			if parts[0] != "" {
				name = parts[0]
			}
			for _, opt := range parts[1:] {
				if opt == "omitempty" || opt == "omitzero" {
					omitempty = true
				}
			}
		}

		propSchema := goTypeToJSONSchema(field.Type)
		if desc := field.Tag.Get("jsonschema"); desc != "" {
			propSchema["description"] = desc
		}
		properties[name] = propSchema

		if !omitempty {
			required = append(required, name)
		}
	}

	schema := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func splitJSONTag(tag string) []string {
	var parts []string
	current := ""
	for _, c := range tag {
		if c == ',' {
			parts = append(parts, current)
			current = ""
		} else {
			current += string(c)
		}
	}
	parts = append(parts, current)
	return parts
}

func goTypeToJSONSchema(t reflect.Type) map[string]any {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Slice, reflect.Array:
		return map[string]any{
			"type":  "array",
			"items": goTypeToJSONSchema(t.Elem()),
		}
	case reflect.Map:
		return map[string]any{
			"type": "object",
		}
	case reflect.Struct:
		return inferStructSchema(t)
	case reflect.Interface:
		return map[string]any{}
	default:
		return map[string]any{}
	}
}

// NewTool creates a Tool with a JSON Schema inferred from the type parameter T
// and a type-safe Execute handler. T must be a struct type with exported fields.
//
// The inferred schema uses json struct tags for property names and the jsonschema
// struct tag for descriptions. Fields without omitempty are marked as required.
//
// Example:
//
//	type WeatherParams struct {
//	    Location string `json:"location" jsonschema:"The city name"`
//	    Units    string `json:"units,omitempty" jsonschema:"metric or imperial"`
//	}
//
//	tool := llm.NewTool("get_weather", "Get weather for a location",
//	    func(ctx *llm.ToolExecContext, input WeatherParams) (any, error) {
//	        return "Sunny, 22°C", nil
//	    })
func NewTool[T any](name, description string, execute func(ctx *ToolExecContext, input T) (any, error)) Tool {
	schema := inferStructSchema(reflect.TypeOf((*T)(nil)).Elem())
	return Tool{
		Name:        name,
		Description: description,
		Parameters:  schema,
		Execute: func(ctx *ToolExecContext, input any) (any, error) {
			data, err := json.Marshal(input)
			if err != nil {
				return nil, errs.Wrap(err, "llm: marshal tool input")
			}
			var typed T
			if err := json.Unmarshal(data, &typed); err != nil {
				return nil, errs.Wrapf(err, "llm: unmarshal tool input to %T", typed)
			}
			return execute(ctx, typed)
		},
	}
}
