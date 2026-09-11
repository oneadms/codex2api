package proxy

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// 模拟 Codex 的真实循环：收到客户端工具调用后回传结果，不额外发送“继续”。
func TestTraeCNToolSearchContinuesIntoLoadedNamespace(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := auth.ConfiguredTraeCNSettings()
	t.Cleanup(func() { auth.SetConfiguredTraeCNSettings(previous) })
	auth.SetConfiguredTraeCNSettings(auth.TraeCNSettings{ModelMapping: map[string]string{"gpt-5.6-sol": "doubao-seed-code"}})
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			resetResponseCacheStateForTest(testResponseCacheConfig())
			t.Cleanup(func() { resetResponseCacheStateForTest(defaultResponseCacheConfig()) })
			upstreamCalls := 0
			handler := newTraeCNContextTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				upstreamCalls++
				if gjson.GetBytes(body, "model").String() != "doubao-seed-code" {
					t.Errorf("model mapping changed: %s", body)
				}
				if !gjson.GetBytes(body, `tools.#(function.name=="tool_search")`).Exists() {
					t.Errorf("tool search was dropped: %s", body)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				switch upstreamCalls {
				case 1:
					io.WriteString(w, traeCNLoopTestSSE("output", map[string]any{"response": "我还会检查参考目录。", "tool_calls": []any{
						map[string]any{"id": "call_search", "function_call": map[string]any{"name": "tool_search", "arguments": `{"query":"read project files","limit":2}`}},
					}}))
				case 2:
					if !gjson.GetBytes(body, `tools.#(function.name=="files__read")`).Exists() || !gjson.GetBytes(body, `tools.#(function.name=="files__write")`).Exists() {
						t.Errorf("loaded namespace tools missing: %s", body)
					}
					io.WriteString(w, traeCNLoopTestSSE("output", map[string]any{"response": "接下来读取配置。", "tool_calls": []any{
						map[string]any{"id": "call_read", "function_call": map[string]any{"name": "files__read", "arguments": `{"path":"config.json"}`}},
					}}))
				case 3:
					searchCalls, readCalls, results := 0, 0, 0
					for _, message := range gjson.GetBytes(body, "messages").Array() {
						for _, call := range message.Get("tool_calls").Array() {
							switch call.Get("function_call.name").String() {
							case "tool_search":
								searchCalls++
							case "files__read":
								readCalls++
							}
						}
						if message.Get("role").String() == "tool" {
							results++
						}
					}
					if searchCalls != 1 || readCalls != 1 || results != 2 {
						t.Errorf("history lost or duplicated tools: %s", body)
					}
					io.WriteString(w, traeCNLoopTestSSE("output", map[string]any{"response": "配置检查完成，已确认版本。"}))
				default:
					t.Errorf("unexpected upstream request %d", upstreamCalls)
				}
				io.WriteString(w, traeCNLoopTestSSE("done", map[string]any{"finish_reason": "stop"}))
			})
			request := map[string]any{"model": "gpt-5.6-sol", "stream": stream, "tools": []any{map[string]any{"type": "tool_search"}}, "input": "检查配置版本"}
			first := traeCNContextTestResponse(t, invokeTraeCNContextTestRequest(t, handler, 91101, database.UpstreamChannelTraeCN, request), stream)
			search := first.Get(`output.#(type=="tool_search_call")`)
			if search.Get("execution").String() != "client" || !search.Get("arguments").IsObject() || search.Get("call_id").String() != "call_search" {
				t.Fatalf("Codex received no executable search: %s", first.Raw)
			}
			request["previous_response_id"] = first.Get("id").String()
			// 两批搜索结果向同一命名空间追加工具，不能按命名空间名称粗略去重。
			request["input"] = []any{search.Value(), map[string]any{
				"type": "tool_search_output", "call_id": "call_search", "output": "Loaded project file tools.", "tools": []any{
					map[string]any{"type": "namespace", "name": "files", "tools": []any{
						map[string]any{"type": "function", "name": "read", "parameters": map[string]any{
							"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []any{"path"},
						}},
					}},
					map[string]any{"type": "namespace", "name": "files", "tools": []any{
						map[string]any{"type": "function", "name": "write", "parameters": map[string]any{"type": "object"}},
					}},
				},
			}}
			second := traeCNContextTestResponse(t, invokeTraeCNContextTestRequest(t, handler, 91101, database.UpstreamChannelTraeCN, request), stream)
			read := second.Get(`output.#(type=="function_call")`)
			if read.Get("namespace").String() != "files" || read.Get("name").String() != "read" || read.Get("call_id").String() != "call_read" {
				t.Fatalf("Codex cannot execute the loaded tool: %s", second.Raw)
			}
			request["previous_response_id"] = second.Get("id").String()
			request["input"] = []any{map[string]any{"type": "function_call_output", "call_id": "call_read", "output": `{"version":2}`}}
			third := traeCNContextTestResponse(t, invokeTraeCNContextTestRequest(t, handler, 91101, database.UpstreamChannelTraeCN, request), stream)
			if upstreamCalls != 3 || !strings.Contains(third.Get("output.0.content.0.text").String(), "检查完成") {
				t.Fatalf("tool loop stopped early: calls=%d response=%s", upstreamCalls, third.Raw)
			}
		})
	}
}
