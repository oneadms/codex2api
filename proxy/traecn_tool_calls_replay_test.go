package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func traeCNTestJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func traeCNTestSSE(event string, value any) string {
	raw, _ := json.Marshal(value)
	if event == "" {
		return "data: " + string(raw) + "\n\n"
	}
	return "event: " + event + "\ndata: " + string(raw) + "\n\n"
}

func TestTraeToolCallsSkipsEmptyArraysAndDedupes(t *testing.T) {
	t.Parallel()
	readCall := map[string]any{
		"id": "call_read", "type": "function",
		"function_call": map[string]any{"name": "read_file", "arguments": traeCNTestJSON(t, map[string]any{"path": "README.md"})},
	}
	openaiCall := map[string]any{
		"id":       "call_read",
		"function": map[string]any{"name": "read_file", "arguments": traeCNTestJSON(t, map[string]any{"path": "a.go"})},
	}
	for _, tc := range []struct {
		name string
		raw  any
		want []string
	}{
		{
			name: "empty_top_level_masks_message",
			raw: map[string]any{
				"response": "接下来检查仓库结构。", "tool_calls": []any{},
				"message": map[string]any{"role": "assistant", "content": "接下来检查仓库结构。", "tool_calls": []any{readCall}},
			},
			want: []string{"call_read"},
		},
		{
			name: "empty_delta_masks_choice_message",
			raw: map[string]any{"choices": []any{map[string]any{
				"delta":         map[string]any{"content": "接下来检查……", "tool_calls": []any{}},
				"message":       map[string]any{"tool_calls": []any{openaiCall}},
				"finish_reason": "stop",
			}}},
			want: []string{"call_read"},
		},
		{
			name: "duplicate_fields_same_id",
			raw: map[string]any{
				"tool_calls": []any{readCall},
				"message":    map[string]any{"tool_calls": []any{readCall}},
			},
			want: []string{"call_read"},
		},
		{
			name: "empty_everywhere",
			raw: map[string]any{
				"tool_calls": []any{},
				"message":    map[string]any{"tool_calls": []any{}},
				"choices":    []any{map[string]any{"delta": map[string]any{"tool_calls": []any{}}, "finish_reason": "stop"}},
			},
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := traeToolCalls(gjson.Parse(traeCNTestJSON(t, tc.raw)), "output")
			if len(calls) != len(tc.want) {
				t.Fatalf("got %d calls, want %d: %s", len(calls), len(tc.want), traeCNTestJSON(t, tc.raw))
			}
			for i, wantID := range tc.want {
				gotID := strings.TrimSpace(traeFirstText(calls[i], "id", "call_id", "tool_call_id"))
				if wantID != "" && gotID != wantID {
					t.Fatalf("call %d id=%q, want %q", i, gotID, wantID)
				}

			}
		})
	}
}

func TestTraeCNStreamEmptyTopLevelToolCallsStillDeliverNestedCall(t *testing.T) {
	t.Parallel()
	readFile := func(path string) map[string]any {
		return map[string]any{"id": "call_read", "type": "function", "function_call": map[string]any{"name": "read_file", "arguments": traeCNTestJSON(t, map[string]any{"path": path})}}
	}
	for _, tc := range []struct {
		name   string
		stream string
	}{
		{"native_empty_array_with_message", traeCNTestSSE("output", map[string]any{
			"response": "接下来检查仓库结构。", "tool_calls": []any{},
			"message": map[string]any{"role": "assistant", "content": "接下来检查仓库结构。", "tool_calls": []any{readFile("README.md")}},
		}) + traeCNTestSSE("done", map[string]any{"finish_reason": "stop"})},
		{"openai_empty_delta_with_message", traeCNTestSSE("output", map[string]any{"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"content": "接下来检查……", "tool_calls": []any{}},
			"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"id": "call_read", "type": "function",
				"function": map[string]any{"name": "read_file", "arguments": traeCNTestJSON(t, map[string]any{"path": "a.go"})},
			}}},
			"finish_reason": "stop",
		}}}) + traeCNTestSSE("done", map[string]any{})},
		{"nested_data_wrapper", traeCNTestSSE("", map[string]any{
			"type": "output",
			"data": map[string]any{
				"response": "接下来检查。", "tool_calls": []any{},
				"choices": []any{map[string]any{"message": map[string]any{"tool_calls": []any{readFile("b.go")}}}},
			},
		}) + traeCNTestSSE("done", map[string]any{"finish_reason": "stop"})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := io.ReadAll(traeCNCanonicalStream(io.NopCloser(strings.NewReader(tc.stream)), "doubao-seed-code"))
			if err != nil {
				t.Fatal(err)
			}
			completed, ok := findCanonicalEvent(canonicalSSEEvents(t, raw), "response.completed")
			if !ok {
				t.Fatalf("missing completed response: %s", raw)
			}
			tool := completed.Get("response.output.#(type==\"function_call\")")
			if tool.Get("name").String() != "read_file" {
				t.Fatalf("nested tool call was dropped: %s", completed.Raw)
			}
			if tool.Get("call_id").String() != "call_read" {
				t.Fatalf("call id lost: %s", completed.Raw)
			}
			args := tool.Get("arguments").String()
			if !strings.Contains(args, ".go") && !strings.Contains(args, "README.md") {
				t.Fatalf("tool arguments missing: %s", completed.Raw)
			}
		})
	}
}

func TestTraeCNStreamDuplicateToolFieldsAreNotDeliveredTwice(t *testing.T) {
	t.Parallel()
	call := map[string]any{"id": "call_read", "function_call": map[string]any{"name": "read_file", "arguments": traeCNTestJSON(t, map[string]any{"path": "a.go"})}}
	provider := traeCNTestSSE("output", map[string]any{
		"response": "", "tool_calls": []any{call},
		"message": map[string]any{"tool_calls": []any{call}},
	}) + traeCNTestSSE("done", map[string]any{"finish_reason": "tool_calls"})
	raw, err := io.ReadAll(traeCNCanonicalStream(io.NopCloser(strings.NewReader(provider)), "doubao-seed-code"))
	if err != nil {
		t.Fatal(err)
	}
	completed, ok := findCanonicalEvent(canonicalSSEEvents(t, raw), "response.completed")
	if !ok {
		t.Fatalf("missing completed response: %s", raw)
	}
	var tools []gjson.Result
	for _, item := range completed.Get("response.output").Array() {
		if item.Get("type").String() == "function_call" {
			tools = append(tools, item)
		}
	}
	if len(tools) != 1 || tools[0].Get("arguments").String() != traeCNTestJSON(t, map[string]any{"path": "a.go"}) {
		t.Fatalf("duplicate fields were delivered twice or concatenated: %s", completed.Raw)
	}
}

func TestTraeCNFinishReasonSourceDistinguishesDefaultedStop(t *testing.T) {
	t.Parallel()
	t.Run("defaulted_on_done_sentinel", func(t *testing.T) {
		state := newTraeCNCanonicalState("doubao-seed-code")
		var buf bytes.Buffer
		if err := state.consume(&buf, "output", []byte(traeCNTestJSON(t, map[string]any{"content": "结果正常"}))); err != nil {
			t.Fatal(err)
		}
		if err := state.consume(&buf, "", []byte("[DONE]")); err != nil {
			t.Fatal(err)
		}
		if state.finishReason != "stop" || state.finishReasonSource != traeCNFinishReasonDefaulted {
			t.Fatalf("finish_reason=%q source=%q, want stop/defaulted", state.finishReason, state.finishReasonSource)
		}
	})
	t.Run("upstream_stop", func(t *testing.T) {
		state := newTraeCNCanonicalState("doubao-seed-code")
		var buf bytes.Buffer
		if err := state.consume(&buf, "output", []byte(traeCNTestJSON(t, map[string]any{"content": "结果正常"}))); err != nil {
			t.Fatal(err)
		}
		if err := state.consume(&buf, "done", []byte(traeCNTestJSON(t, map[string]any{"finish_reason": "stop"}))); err != nil {
			t.Fatal(err)
		}
		if state.finishReason != "stop" || state.finishReasonSource != traeCNFinishReasonUpstream {
			t.Fatalf("finish_reason=%q source=%q, want stop/upstream", state.finishReason, state.finishReasonSource)
		}
	})
	t.Run("upstream_on_choice_then_done", func(t *testing.T) {
		state := newTraeCNCanonicalState("doubao-seed-code")
		var buf bytes.Buffer
		if err := state.consume(&buf, "output", []byte(traeCNTestJSON(t, map[string]any{"choices": []any{map[string]any{
			"delta": map[string]any{"content": "还有工作"}, "finish_reason": "length",
		}}}))); err != nil {
			t.Fatal(err)
		}
		if err := state.consume(&buf, "", []byte("[DONE]")); err != nil {
			t.Fatal(err)
		}
		if state.finishReason != "length" || state.finishReasonSource != traeCNFinishReasonUpstream {
			t.Fatalf("finish_reason=%q source=%q, want length/upstream", state.finishReason, state.finishReasonSource)
		}
	})
}

func TestTraeCNEmptyAliasesWithFragmentsAndTerminalCall(t *testing.T) {
	t.Parallel()
	first := map[string]any{"index": 0, "id": "call_read", "function_call": map[string]any{"name": "read_file", "arguments": "{\"path\":"}}
	fragment := map[string]any{"index": 0, "function_call": map[string]any{"arguments": "\"README.md\"}"}}
	final := map[string]any{"index": 0, "id": "call_read", "function_call": map[string]any{"name": "read_file", "arguments": "{\"path\":\"README.md\"}"}}
	provider := traeCNTestSSE("output", map[string]any{"tool_calls": []any{}, "message": map[string]any{"tool_calls": []any{first}}}) +
		traeCNTestSSE("output", map[string]any{"tool_calls": []any{fragment}, "choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{fragment}}}}}) +
		traeCNTestSSE("done", map[string]any{"tool_calls": []any{}, "message": map[string]any{"tool_calls": []any{final}}, "finish_reason": "stop"})
	raw, err := io.ReadAll(traeCNCanonicalStream(io.NopCloser(strings.NewReader(provider)), "kimi-k3"))
	if err != nil {
		t.Fatal(err)
	}
	events := canonicalSSEEvents(t, raw)
	completed, ok := findCanonicalEvent(events, "response.completed")
	if !ok || completed.Get("response.output.#").Int() != 1 || completed.Get("response.output.0.arguments").String() != "{\"path\":\"README.md\"}" {
		t.Fatalf("lost or duplicated fragment: %s", raw)
	}
	added, done := 0, 0
	for _, event := range events {
		if event.Get("item.type").String() != "function_call" {
			continue
		}
		if event.Get("type").String() == "response.output_item.added" {
			added++
		}
		if event.Get("type").String() == "response.output_item.done" {
			done++
		}
	}
	if added != 1 || done != 1 {
		t.Fatalf("tool delivered %d/%d times", added, done)
	}
}
