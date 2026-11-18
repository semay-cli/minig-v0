package jsonschema

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateRequiredAndTypes(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"required": ["severity", "should_alert"],
		"properties": {
			"severity": {"type": "string", "enum": ["low", "high"]},
			"should_alert": {"type": "boolean"}
		}
	}`)
	ok := json.RawMessage(`{"severity":"high","should_alert":true,"extra":1}`)
	if err := Validate(ok, schema); err != nil {
		t.Fatal(err)
	}
	bad := json.RawMessage(`{"severity":"critical","should_alert":true}`)
	if err := Validate(bad, schema); err == nil || !strings.Contains(err.Error(), "enum") {
		t.Fatalf("expected enum error, got %v", err)
	}
	missing := json.RawMessage(`{"severity":"high"}`)
	if err := Validate(missing, schema); err == nil || !strings.Contains(err.Error(), "should_alert") {
		t.Fatalf("expected missing property, got %v", err)
	}
}
