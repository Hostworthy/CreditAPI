package fakeclaude

import "strings"

// SampleFromSchema builds a deterministic value that satisfies a (simple)
// JSON schema: enums and consts pick their first value, strings become
// "fake <name>" (or a date/email when a format asks for one), numbers 42,
// booleans true, arrays one sample item and objects every property.
func SampleFromSchema(schema map[string]any, name string) any {
	if schema == nil {
		return map[string]any{}
	}
	if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
		return enum[0]
	}
	if c, ok := schema["const"]; ok {
		return c
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		if list, ok := schema[key].([]any); ok && len(list) > 0 {
			if first, ok := list[0].(map[string]any); ok {
				return SampleFromSchema(first, name)
			}
		}
	}

	typ, _ := schema["type"].(string)
	if list, ok := schema["type"].([]any); ok {
		for _, t := range list {
			if s, _ := t.(string); s != "" && s != "null" {
				typ = s
				break
			}
		}
	}
	if typ == "" {
		if _, ok := schema["properties"]; ok {
			typ = "object"
		}
	}

	switch typ {
	case "string":
		switch schema["format"] {
		case "date":
			return "2026-01-01"
		case "date-time":
			return "2026-01-01T12:00:00Z"
		case "email":
			return "fake@example.com"
		}
		return "fake " + strings.ReplaceAll(name, "_", " ")
	case "integer", "number":
		return 42
	case "boolean":
		return true
	case "null":
		return nil
	case "array":
		items, _ := schema["items"].(map[string]any)
		return []any{SampleFromSchema(items, name)}
	default: // object
		out := map[string]any{}
		props, _ := schema["properties"].(map[string]any)
		for key, raw := range props {
			if sub, ok := raw.(map[string]any); ok {
				out[key] = SampleFromSchema(sub, key)
			}
		}
		return out
	}
}
