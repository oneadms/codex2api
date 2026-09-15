package proxy

import (
	"io"
	"strings"
	"testing"
)

// finish_reason 可以先出现在内容帧，随后才收到 [DONE]，不能把截断改写成正常完成。
func TestTraeCNStreamPreservesFinishReason(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, provider, event, reason string }{
		{"chat_length", "data: {\"choices\":[{\"delta\":{\"content\":\"还有工作\"},\"finish_reason\":\"length\"}]}\n\ndata: [DONE]\n\n", "response.incomplete", "max_output_tokens"},
		{"native_length", "event: output\ndata: {\"response\":\"还有工作\",\"finish_reason\":\"length\"}\n\nevent: done\ndata: {}\n\n", "response.incomplete", "max_output_tokens"},
		{"partial_tool_length", "event: output\ndata: {\"tool_calls\":[{\"id\":\"call_cut\",\"function_call\":{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\"}}]}\n\nevent: done\ndata: {\"finish_reason\":\"length\"}\n\n", "response.incomplete", "max_output_tokens"},
		{"missing_calls", "event: output\ndata: {\"response\":\"我还会继续检查\"}\n\nevent: done\ndata: {\"finish_reason\":\"tool_calls\"}\n\n", "response.failed", "missing_tool_calls"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := io.ReadAll(traeCNCanonicalStream(io.NopCloser(strings.NewReader(tc.provider)), "doubao-seed-code"))
			if err != nil {
				t.Fatal(err)
			}
			events := canonicalSSEEvents(t, raw)
			event, ok := findCanonicalEvent(events, tc.event)
			if !ok || (event.Get("response.incomplete_details.reason").String() != tc.reason && event.Get("response.error.code").String() != tc.reason) {
				t.Fatalf("incorrect terminal event: %s", raw)
			}
			if _, ok := findCanonicalEvent(events, "response.completed"); ok {
				t.Fatalf("unfinished response reported success: %s", raw)
			}
			if tc.name == "partial_tool_length" {
				if _, ok := findCanonicalEvent(events, "response.output_item.done"); ok {
					t.Fatalf("partial tool must not be made executable: %s", raw)
				}
			}
		})
	}
}

// 参数中的嵌套左花括号是有效增量，不能当成已发送过的参数前缀丢掉。
func TestTraeCNStreamKeepsNestedArgumentDeltas(t *testing.T) {
	t.Parallel()
	provider := ""
	for index, delta := range []string{`{"filter":`, `{`, `"name":"device"}}`} {
		call := map[string]any{"index": 0, "function_call": map[string]any{"arguments": delta}}
		if index == 0 {
			call["id"] = "call_nested"
			call["function_call"].(map[string]any)["name"] = "lookup"
		}
		provider += traeCNLoopTestSSE("output", map[string]any{"tool_calls": []any{call}})
	}
	provider += traeCNLoopTestSSE("done", map[string]any{"finish_reason": "tool_calls"})
	raw, err := io.ReadAll(traeCNCanonicalStream(io.NopCloser(strings.NewReader(provider)), "doubao-seed-code"))
	if err != nil {
		t.Fatal(err)
	}
	completed, ok := findCanonicalEvent(canonicalSSEEvents(t, raw), "response.completed")
	if !ok || completed.Get("response.output.0.arguments").String() != `{"filter":{"name":"device"}}` {
		t.Fatalf("nested argument delta was lost: %s", raw)
	}
}

// 只有思考或空白的 stop 不能作为成功交付，否则客户端会结束本轮且不再重试。
func TestTraeCNStreamRejectsCompletionWithoutAnswer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, content, terminal, wantEvent, wantCode string
	}{
		{"empty_stop", `{}`, "event: done\ndata: {\"finish_reason\":\"stop\"}\n\n", "response.failed", "upstream_empty_output"},
		{"empty_done", `{}`, "data: [DONE]\n\n", "response.failed", "upstream_empty_output"},
		{"reasoning_stop", `{"reasoning_content":"正在检查文件"}`, "event: done\ndata: {\"finish_reason\":\"stop\"}\n\n", "response.failed", "upstream_empty_output"},
		{"reasoning_done", `{"reasoning_content":"正在检查文件"}`, "data: [DONE]\n\n", "response.failed", "upstream_empty_output"},
		{"whitespace", `{"content":" \n\t"}`, "data: [DONE]\n\n", "response.failed", "upstream_empty_output"},
		{"reasoning_whitespace", `{"reasoning_content":"正在检查文件","content":"　 "}`, "data: [DONE]\n\n", "response.failed", "upstream_empty_output"},
		{"reasoning_length", `{"reasoning_content":"正在检查文件"}`, "event: done\ndata: {\"finish_reason\":\"length\"}\n\n", "response.incomplete", ""},
		{"reasoning_filtered", `{"reasoning_content":"正在检查文件"}`, "event: done\ndata: {\"finish_reason\":\"content_filter\"}\n\n", "response.incomplete", ""},
		{"answer", `{"reasoning_content":"已检查","content":"结果正常"}`, "data: [DONE]\n\n", "response.completed", ""},
		{"tool", `{"reasoning_content":"需要读文件","tool_calls":[{"id":"call_read","function_call":{"name":"read_file","arguments":"{}"}}]}`, "event: done\ndata: {\"finish_reason\":\"tool_calls\"}\n\n", "response.completed", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := "event: output\ndata: " + tc.content + "\n\nevent: token_usage\ndata: {\"prompt_tokens\":3,\"completion_tokens\":4,\"total_tokens\":7}\n\n" + tc.terminal
			raw, err := io.ReadAll(traeCNCanonicalStream(io.NopCloser(strings.NewReader(provider)), "doubao-seed-code"))
			if err != nil {
				t.Fatal(err)
			}
			events := canonicalSSEEvents(t, raw)
			terminal, ok := findCanonicalEvent(events, tc.wantEvent)
			if !ok || terminal.Get("response.error.code").String() != tc.wantCode {
				t.Fatalf("unexpected terminal: %s", raw)
			}
			if terminal.Get("response.usage.output_tokens").Int() != 4 {
				t.Fatalf("lost usage at terminal: %s", raw)
			}
			terminals := 0
			for _, event := range events {
				switch event.Get("type").String() {
				case "response.completed", "response.failed", "response.incomplete":
					terminals++
				case "response.output_item.done":
					if tc.wantEvent != "response.completed" {
						t.Fatalf("unfinished output marked as acknowledged: %s", raw)
					}
				}
			}
			if terminals != 1 {
				t.Fatalf("terminal count=%d: %s", terminals, raw)
			}
		})
	}
}
