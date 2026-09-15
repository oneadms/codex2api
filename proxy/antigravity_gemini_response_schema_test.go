package proxy

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

func TestAntigravityNormalizeNativeGeminiResponseSchema_MovesJsonSchema(t *testing.T) {
	request := map[string]any{
		"generationConfig": map[string]any{
			"responseMimeType": "application/json",
			"responseJsonSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"message": map[string]any{"type": "string"}},
				"required":   []any{"message"},
			},
		},
	}
	antigravityNormalizeNativeGeminiResponseSchema(request)
	encoded, _ := json.Marshal(request)
	if gjson.GetBytes(encoded, "generationConfig.responseJsonSchema").Exists() {
		t.Fatalf("responseJsonSchema must be removed: %s", encoded)
	}
	if got := gjson.GetBytes(encoded, "generationConfig.responseSchema.properties.message.type").String(); got == "" {
		t.Fatalf("responseSchema not populated: %s", encoded)
	}
	if gjson.GetBytes(encoded, "generationConfig.responseMimeType").String() != "application/json" {
		t.Fatal("sibling generationConfig keys must be preserved")
	}
}

func TestAntigravityNormalizeNativeGeminiResponseSchema_SnakeCaseAndExistingWins(t *testing.T) {
	request := map[string]any{
		"generation_config": map[string]any{
			"response_json_schema": map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}},
			"responseSchema":       map[string]any{"type": "OBJECT", "properties": map[string]any{"keep": map[string]any{"type": "STRING"}}},
		},
	}
	antigravityNormalizeNativeGeminiResponseSchema(request)
	encoded, _ := json.Marshal(request)
	if gjson.GetBytes(encoded, "generation_config.response_json_schema").Exists() {
		t.Fatalf("snake_case key must be removed: %s", encoded)
	}
	if !gjson.GetBytes(encoded, "generation_config.responseSchema.properties.keep").Exists() {
		t.Fatalf("explicit responseSchema must win: %s", encoded)
	}
}

func TestAntigravityNormalizeNativeGeminiResponseSchema_ResolvesDefsLikeResponsesPath(t *testing.T) {
	request := map[string]any{
		"generationConfig": map[string]any{
			"responseJsonSchema": map[string]any{
				"type": "object",
				"$defs": map[string]any{
					"Item": map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "integer"}}},
				},
				"properties": map[string]any{
					"items": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/Item"}},
				},
			},
		},
	}
	antigravityNormalizeNativeGeminiResponseSchema(request)
	encoded, _ := json.Marshal(request)
	schema := gjson.GetBytes(encoded, "generationConfig.responseSchema")
	if !schema.Exists() {
		t.Fatalf("responseSchema missing: %s", encoded)
	}
	if schema.Get("$defs").Exists() || schema.Get("properties.items.items.$ref").Exists() {
		t.Fatalf("$defs/$ref must be resolved for responseSchema: %s", schema.Raw)
	}
	if !schema.Get("properties.items.items.properties.id").Exists() {
		t.Fatalf("referenced definition must be inlined: %s", schema.Raw)
	}
}

func TestAntigravityNormalizeNativeGeminiResponseSchema_NoConfigNoop(t *testing.T) {
	request := map[string]any{"contents": []any{}}
	antigravityNormalizeNativeGeminiResponseSchema(request)
	if _, ok := request["generationConfig"]; ok {
		t.Fatal("must not invent generationConfig")
	}
	request = map[string]any{"generationConfig": map[string]any{"responseJsonSchema": nil}}
	antigravityNormalizeNativeGeminiResponseSchema(request)
	genConfig := request["generationConfig"].(map[string]any)
	if _, has := genConfig["responseSchema"]; has {
		t.Fatal("null responseJsonSchema must not produce a responseSchema")
	}
	if _, has := genConfig["responseJsonSchema"]; has {
		t.Fatal("null responseJsonSchema must still be dropped")
	}
}

func TestGeminiNativeToAntigravityEnvelope_NormalizesResponseJsonSchema(t *testing.T) {
	raw := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"responseMimeType":"application/json","responseJsonSchema":{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}}}`)
	payload, _, _, err := geminiNativeToAntigravityEnvelope(raw, "proj", "gemini-3.7-flash-high")
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if gjson.GetBytes(payload, "request.generationConfig.responseJsonSchema").Exists() {
		t.Fatalf("responseJsonSchema leaked to Antigravity: %s", payload)
	}
	if !gjson.GetBytes(payload, "request.generationConfig.responseSchema.properties.message").Exists() {
		t.Fatalf("responseSchema missing in envelope: %s", payload)
	}
}

// $ref 里的 JSON Pointer 转义(~1 → /、~0 → ~、百分号编码)要解码后再查定义键,
// 否则 "A/B" 这类键永远命中不了,引用会被清成 {} 丢约束。
func TestAntigravityNormalizeNativeGeminiResponseSchema_DecodesJSONPointerRefs(t *testing.T) {
	request := map[string]any{
		"generationConfig": map[string]any{
			"responseJsonSchema": map[string]any{
				"type": "object",
				"$defs": map[string]any{
					"A/B":    map[string]any{"type": "object", "properties": map[string]any{"slash": map[string]any{"type": "string"}}},
					"T~X":    map[string]any{"type": "object", "properties": map[string]any{"tilde": map[string]any{"type": "integer"}}},
					"Sp ace": map[string]any{"type": "object", "properties": map[string]any{"space": map[string]any{"type": "boolean"}}},
				},
				"properties": map[string]any{
					"a": map[string]any{"$ref": "#/$defs/A~1B"},
					"b": map[string]any{"$ref": "#/$defs/T~0X"},
					"c": map[string]any{"$ref": "#/$defs/Sp%20ace"},
				},
			},
		},
	}
	antigravityNormalizeNativeGeminiResponseSchema(request)
	encoded, _ := json.Marshal(request)
	schema := gjson.GetBytes(encoded, "generationConfig.responseSchema")
	for prop, leaf := range map[string]string{"a": "slash", "b": "tilde", "c": "space"} {
		if !schema.Get("properties." + prop + ".properties." + leaf).Exists() {
			t.Fatalf("$ref for %q not resolved through pointer decoding: %s", prop, schema.Raw)
		}
	}
	if schema.Get("properties.a.$ref").Exists() || schema.Get("$defs").Exists() {
		t.Fatalf("unresolved $ref/$defs leaked: %s", schema.Raw)
	}
}
