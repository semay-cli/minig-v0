package jsonschema

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// Validate checks instance against a small subset of JSON Schema:
// type, required, properties, items, and enum.
func Validate(instance json.RawMessage, schemaDoc json.RawMessage) error {
	if len(bytesTrim(schemaDoc)) == 0 || string(bytesTrim(schemaDoc)) == "null" {
		return nil
	}
	var schema any
	if err := json.Unmarshal(schemaDoc, &schema); err != nil {
		return fmt.Errorf("response schema is not valid JSON: %w", err)
	}
	var value any
	if err := json.Unmarshal(instance, &value); err != nil {
		return fmt.Errorf("output is not valid JSON: %w", err)
	}
	if err := validateNode(value, schema, "$"); err != nil {
		return err
	}
	return nil
}

func validateNode(value, schema any, path string) error {
	spec, ok := schema.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: schema must be an object", path)
	}
	if enum, ok := spec["enum"].([]any); ok && len(enum) > 0 {
		matched := false
		for _, item := range enum {
			if reflect.DeepEqual(value, item) {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s: value %v is not in enum", path, value)
		}
	}
	if typeName, ok := spec["type"].(string); ok && typeName != "" {
		if err := checkType(value, typeName, path); err != nil {
			return err
		}
	}
	if req, ok := spec["required"].([]any); ok {
		obj, _ := value.(map[string]any)
		for _, item := range req {
			name, _ := item.(string)
			if name == "" {
				continue
			}
			if obj == nil {
				return fmt.Errorf("%s: missing required property %q", path, name)
			}
			if _, exists := obj[name]; !exists {
				return fmt.Errorf("%s: missing required property %q", path, name)
			}
		}
	}
	if props, ok := spec["properties"].(map[string]any); ok {
		if obj, isObj := value.(map[string]any); isObj {
			for name, child := range obj {
				childSchema, exists := props[name]
				if !exists {
					continue
				}
				childPath := path + "." + name
				if err := validateNode(child, childSchema, childPath); err != nil {
					return err
				}
			}
		}
	}
	if items, ok := spec["items"]; ok {
		if arr, isArr := value.([]any); isArr {
			for i, child := range arr {
				if err := validateNode(child, items, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func checkType(value any, typeName, path string) error {
	switch typeName {
	case "object":
		if _, ok := value.(map[string]any); !ok {
			return fmt.Errorf("%s: expected object", path)
		}
	case "array":
		if _, ok := value.([]any); !ok {
			return fmt.Errorf("%s: expected array", path)
		}
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("%s: expected string", path)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s: expected boolean", path)
		}
	case "number":
		if _, ok := value.(float64); !ok {
			return fmt.Errorf("%s: expected number", path)
		}
	case "integer":
		n, ok := value.(float64)
		if !ok || n != float64(int64(n)) {
			return fmt.Errorf("%s: expected integer", path)
		}
	case "null":
		if value != nil {
			return fmt.Errorf("%s: expected null", path)
		}
	default:
		return fmt.Errorf("%s: unsupported schema type %q", path, typeName)
	}
	return nil
}

func bytesTrim(raw json.RawMessage) json.RawMessage {
	return json.RawMessage(strings.TrimSpace(string(raw)))
}
