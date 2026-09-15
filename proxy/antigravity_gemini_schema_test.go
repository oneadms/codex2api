package proxy

import (
	"testing"
)

func TestAntigravityNormalizeMalformedToolSchemaWrapsBareProperties(t *testing.T) {
	root := map[string]any{
		"task_id": map[string]any{
			"type":        "string",
			"description": "Task identifier",
		},
	}
	got := antigravityNormalizeMalformedToolSchema(root)
	props, ok := got["properties"].(map[string]any)
	if !ok || props["task_id"] == nil {
		t.Fatalf("bare property must be wrapped: %#v", got)
	}
	if got["type"] != "object" {
		t.Fatalf("type = %#v", got["type"])
	}
}

func TestAntigravityNormalizeMalformedToolSchemaPromotesBooleanRequired(t *testing.T) {
	root := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"q": map[string]any{
				"type":     "string",
				"required": true,
			},
		},
	}
	got := antigravityNormalizeMalformedToolSchema(root)
	required := antigravityStringSlice(got["required"])
	if len(required) != 1 || required[0] != "q" {
		t.Fatalf("required = %#v", got["required"])
	}
	q, _ := got["properties"].(map[string]any)["q"].(map[string]any)
	if _, ok := q["required"]; ok {
		t.Fatalf("boolean required must be promoted: %#v", q)
	}
}

func TestAntigravityFinalizeToolSchemaDropsEnumAndAddsArrayItems(t *testing.T) {
	got := antigravityFinalizeToolSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tags": map[string]any{"type": "array"},
			"mode": map[string]any{"type": "string", "enum": []any{"a", "b"}},
		},
	})
	tags, _ := got["properties"].(map[string]any)["tags"].(map[string]any)
	if tags["items"] == nil {
		t.Fatalf("array items missing: %#v", tags)
	}
	mode, _ := got["properties"].(map[string]any)["mode"].(map[string]any)
	if _, ok := mode["enum"]; ok {
		t.Fatalf("enum must be dropped: %#v", mode)
	}
	if mode["description"] == "" {
		t.Fatal("enum hint missing from description")
	}
}

func TestAntigravityEnsureGeminiLeadingUserContent(t *testing.T) {
	request := map[string]any{
		"contents": []any{
			map[string]any{"role": "model", "parts": []any{map[string]any{"text": "prior"}}},
			map[string]any{"role": "user", "parts": []any{map[string]any{"text": "next"}}},
		},
	}
	antigravityEnsureGeminiLeadingUserContent(request, "gemini-3.7-flash-tiered")
	contents := request["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents len = %d", len(contents))
	}
	first, _ := contents[0].(map[string]any)
	if first["role"] != "user" {
		t.Fatalf("first role = %#v", first["role"])
	}
}

func TestUnwrapAntigravityCountTokensResponse(t *testing.T) {
	got := unwrapAntigravityCountTokensResponse([]byte(`{"response":{"totalTokens":42}}`))
	if string(got) != `{"totalTokens":42}` {
		t.Fatalf("unwrap = %s", got)
	}
}
