package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Codex 的延迟工具加载把工具声明放进 input[] 的 additional_tools 载体项，而不是
// 顶层 tools[]。Trae 只在顶层接受 function 声明，因此载体里的可调用工具必须被提升，
// 载体项本身不能变成消息，也不能漏进上游请求体。
func TestTraeCNLiftsAdditionalToolsCarrier(t *testing.T) {
	t.Parallel()
	canonical := []byte(`{
  "model":"deepseek-v3",
  "input":[
    {"type":"additional_tools","tools":[
      {"type":"tool_search"},
      {"type":"web_search"},
      {"type":"custom","name":"apply_patch"},
      {"type":"namespace","name":"codex_app","tools":[
        {"type":"function","name":"load_workspace_dependencies","description":"load","input_schema":{"type":"object","properties":{},"additionalProperties":false},"deferLoading":true}
      ]},
      {"type":"function","name":"lookup","description":"Lookup","parameters":{"type":"object","properties":{"id":{"type":"integer"}},"required":["id"]}}
    ]},
    {"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}
  ]
}`)
	body, _, err := buildTraeCNRequestBody(canonical)
	if err != nil {
		t.Fatalf("buildTraeCNRequestBody() error = %v", err)
	}
	if strings.Contains(string(body), "additional_tools") || strings.Contains(string(body), "tool_search") {
		t.Fatalf("carrier leaked upstream: %s", body)
	}
	tools := gjson.GetBytes(body, "tools").Array()
	if len(tools) != 3 {
		t.Fatalf("lifted %d tools, want 3 (custom apply_patch + namespace function + function): %s", len(tools), body)
	}
	for _, tool := range tools {
		if tool.Get("type").String() != "function" || tool.Get("function_call").Exists() {
			t.Fatalf("lifted tool is not a Trae function declaration: %s", tool.Raw)
		}
		// TRAE 的 FunctionDefinition 要求 parameters 是 JSON 字符串。
		if tool.Get("function.parameters").Type != gjson.String {
			t.Fatalf("parameters must be a JSON string: %s", tool.Raw)
		}
	}
	if got := gjson.GetBytes(body, `tools.#(function.name=="load_workspace_dependencies").function.parameters`).String(); gjson.Get(got, "type").String() != "object" {
		t.Fatalf("namespace input_schema was not lifted: %s", body)
	}
	if gjson.GetBytes(body, `tools.#(function.name=="load_workspace_dependencies").deferLoading`).Exists() {
		t.Fatalf("deferLoading flag leaked into the Trae tool declaration: %s", body)
	}
	if got := gjson.GetBytes(body, `tools.#(function.name=="lookup").function.parameters`).String(); gjson.Get(got, "required.0").String() != "id" {
		t.Fatalf("function schema was lost: %s", body)
	}
	// 载体里的 custom 工具降级成带单个 input 参数的 function，回程再还原。
	if got := gjson.GetBytes(body, `tools.#(function.name=="apply_patch").function.parameters`).String(); gjson.Get(got, "required.0").String() != "input" {
		t.Fatalf("custom tool was not bridged to an input-parametered function: %s", body)
	}
	messages := gjson.GetBytes(body, "messages").Array()
	if len(messages) != 1 || messages[0].Get("role").String() != "user" || messages[0].Get("content.0.text").String() != "hello" {
		t.Fatalf("carrier produced a message: %s", body)
	}
}

// 续会话回放会重复带上同一个载体项，客户端也可能同时给出顶层声明。同名工具只保留
// 一份，且顶层（权威）声明优先，避免把过期 schema 送到上游。
func TestTraeCNAdditionalToolsCarrierDeduplicatesDeclarations(t *testing.T) {
	t.Parallel()
	canonical := []byte(`{
  "model":"deepseek-v3",
  "tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"id":{"type":"string"}}}}],
  "input":[
    {"type":"additional_tools","tools":[
      {"type":"function","name":"lookup","parameters":{"type":"object","properties":{"id":{"type":"integer"}}}},
      {"type":"function","name":"list_files","parameters":{"type":"object"}}
    ]},
    {"type":"additional_tools","tools":[
      {"type":"function","name":"list_files","parameters":{"type":"object","properties":{"stale":{"type":"string"}}}},
      {"type":"function","name":"list_files","parameters":{"type":"object","properties":{"stale":{"type":"string"}}}}
    ]},
    {"type":"message","role":"user","content":"list them"}
  ]
}`)
	body, _, err := buildTraeCNRequestBody(canonical)
	if err != nil {
		t.Fatalf("buildTraeCNRequestBody() error = %v", err)
	}
	tools := gjson.GetBytes(body, "tools").Array()
	if len(tools) != 2 {
		t.Fatalf("tools = %d, want 2 without duplicates: %s", len(tools), body)
	}
	lookup := gjson.GetBytes(body, `tools.#(function.name=="lookup").function.parameters`).String()
	if typ := gjson.Get(lookup, "properties.id.type").String(); typ != "string" {
		t.Fatalf("top-level declaration must win, got id type %q: %s", typ, body)
	}
	listFiles := gjson.GetBytes(body, `tools.#(function.name=="list_files").function.parameters`).String()
	if gjson.Get(listFiles, "properties.stale").Exists() {
		t.Fatalf("replayed carrier declaration was not deduplicated: %s", body)
	}
}

// 空载体（或 tools 缺失的载体）必须被安静跳过，而不是让整轮请求失败。
func TestTraeCNAdditionalToolsCarrierWithoutToolsIsIgnored(t *testing.T) {
	t.Parallel()
	canonical := []byte(`{"model":"deepseek-v3","input":[
  {"type":"additional_tools","tools":[]},
  {"type":"additional_tools"},
  {"type":"message","role":"user","content":"plain question"}
]}`)
	body, _, err := buildTraeCNRequestBody(canonical)
	if err != nil {
		t.Fatalf("buildTraeCNRequestBody() error = %v", err)
	}
	if gjson.GetBytes(body, "tools").Exists() {
		t.Fatalf("empty carrier produced tools: %s", body)
	}
	if gjson.GetBytes(body, "messages.#").Int() != 1 {
		t.Fatalf("empty carrier produced messages: %s", body)
	}
}

// 端到端回归：Codex 风格的 /v1/responses 请求（工具只声明在 additional_tools 载体里）
// 过去被 Trae 转换器拒绝为 400，现在必须到达上游并带上提升后的工具声明。
func TestTraeCNResponsesAdditionalToolsCarrierReachesUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetResponseCacheStateForTest(testResponseCacheConfig())
	t.Cleanup(func() { resetResponseCacheStateForTest(defaultResponseCacheConfig()) })
	var seen []byte
	handler := newTraeCNContextTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		seen, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: output\ndata: {\"type\":\"text\",\"content\":\"ok\"}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
	})
	recorder := invokeTraeCNContextTestRequest(t, handler, 91090, database.UpstreamChannelTraeCN, map[string]any{
		"model": "deepseek-v3", "stream": false,
		"input": []any{
			map[string]any{"type": "additional_tools", "tools": []any{
				map[string]any{"type": "tool_search"},
				map[string]any{"type": "function", "name": "lookup", "description": "Lookup", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}},
			}},
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hello"}}},
		},
	})
	traeCNContextTestResponse(t, recorder, false)
	if strings.Contains(string(seen), "additional_tools") {
		t.Fatalf("carrier reached the Trae upstream: %s", seen)
	}
	parameters := gjson.GetBytes(seen, `tools.#(function.name=="lookup").function.parameters`)
	if parameters.Type != gjson.String {
		t.Fatalf("lifted tool missing from upstream request: %s", seen)
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(parameters.String()), &schema); err != nil || schema["type"] != "object" {
		t.Fatalf("lifted parameters = %s (%v), want a JSON object schema", parameters.String(), err)
	}
	var userTexts []string
	for _, message := range gjson.GetBytes(seen, "messages").Array() {
		if message.Get("role").String() == "user" {
			userTexts = append(userTexts, message.Get("content.0.text").String())
		}
	}
	if len(userTexts) != 1 || userTexts[0] != "hello" {
		t.Fatalf("conversation changed: %s", seen)
	}
}
