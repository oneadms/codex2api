package proxy

import (
	"io"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// 原生 output 事件使用 function_call，后续分片可以只带参数并把名称留空。
const traeCNNativeReadFileOutput = `event: output
data: {"response":"","tool_calls":[{"index":0,"id":"call_read","type":"function","function_call":{"name":"read_file","arguments":"{\"path\":"}}]}

event: output
data: {"response":"","tool_calls":[{"index":0,"function_call":{"name":"","arguments":"\"a.go\"}"}}]}

`

// 名称和参数可以分批到达，结束事件中的完整调用也必须参与合并。
func TestTraeCNStreamToolCallVariants(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		stream string
	}{
		{"native_function_call_deltas", traeCNNativeReadFileOutput},
		{"native_function_call", `event: output
data: {"response":"","tool_calls":[{"id":"call_read","function_call":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}}]}

`},
		{"encoded_native_function_call", `event: output
data: {"tool_calls":[{"id":"call_read","function_call":"{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"a.go\\\"}\"}"}]}

`},
		{"native_name_in_done", `event: output
data: {"tool_calls":[{"index":0,"function_call":{"arguments":"{\"path\":\"a.go\"}"}}]}

event: done
data: {"finish_reason":"stop","tool_calls":[{"index":0,"id":"call_read","function_call":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}}]}

`},
		{"named_event", "event: tool_call\ndata: {\"index\":0,\"id\":\"call_read\",\"function\":{\"name\":\"read_file\"}}\n\nevent: output\ndata: {\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"path\\\":\\\"a.go\\\"}\"}}]}\n\n"},
		{"encoded_function", `event: output
data: {"tool_calls":[{"index":0,"id":"call_read","function":"{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"a.go\\\"}\"}"}]}

`},
		{"name_in_done", `event: output
data: {"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"a.go\"}"}}]}

event: done
data: {"finish_reason":"tool_calls","tool_calls":[{"index":0,"id":"call_read","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}}]}

`},
		{"empty_placeholders", `event: output
data: {"tool_calls":[null,{}, {"index":0,"id":"call_read","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}}, {"index":1,"function":{}}]}

`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := tc.stream + "event: done\ndata: {\"finish_reason\":\"tool_calls\"}\n\n"
			raw, err := io.ReadAll(traeCNCanonicalStream(io.NopCloser(strings.NewReader(provider)), "doubao-seed-code"))
			if err != nil {
				t.Fatal(err)
			}
			events := canonicalSSEEvents(t, raw)
			completed, ok := findCanonicalEvent(events, "response.completed")
			if !ok {
				t.Fatalf("missing completed response: %s", raw)
			}
			output := completed.Get("response.output").Array()
			if len(output) != 1 || output[0].Get("name").String() != "read_file" || output[0].Get("call_id").String() != "call_read" || output[0].Get("arguments").String() != `{"path":"a.go"}` {
				t.Fatalf("tool call mismatch: %s", completed.Raw)
			}
			added, ok := findCanonicalEvent(events, "response.output_item.added")
			if !ok || added.Get("item.call_id").String() != "call_read" {
				t.Fatalf("tool identity changed: %s", raw)
			}
		})
	}
}

func TestTraeCNStreamDistinctToolIDsWithoutIndex(t *testing.T) {
	t.Parallel()
	provider := `event: output
data: {"tool_calls":[{"id":"call_a","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}}]}

event: output
data: {"tool_calls":[{"id":"call_b","function":{"name":"read_file","arguments":"{\"path\":\"b.go\"}"}}]}

event: done
data: {"finish_reason":"tool_calls"}

`
	raw, err := io.ReadAll(traeCNCanonicalStream(io.NopCloser(strings.NewReader(provider)), "doubao-seed-code"))
	if err != nil {
		t.Fatal(err)
	}
	completed, ok := findCanonicalEvent(canonicalSSEEvents(t, raw), "response.completed")
	if !ok {
		t.Fatalf("missing completed response: %s", raw)
	}
	output := completed.Get("response.output").Array()
	if len(output) != 2 || output[0].Get("call_id").String() != "call_a" || output[1].Get("call_id").String() != "call_b" || gjson.Get(output[1].Get("arguments").String(), "path").String() != "b.go" {
		t.Fatalf("distinct tool calls were merged: %s", completed.Raw)
	}
}

func TestTraeCNStreamRejectsNamelessToolCall(t *testing.T) {
	t.Parallel()
	provider := "event: output\ndata: {\"tool_calls\":[{\"id\":\"call_bad\",\"function\":{\"arguments\":\"{}\"}}]}\n\nevent: done\ndata: {}\n\n"
	raw, err := io.ReadAll(traeCNCanonicalStream(io.NopCloser(strings.NewReader(provider)), "doubao-seed-code"))
	if err != nil {
		t.Fatal(err)
	}
	failed, ok := findCanonicalEvent(canonicalSSEEvents(t, raw), "response.failed")
	if !ok || failed.Get("response.error.code").String() != "malformed_tool_call" {
		t.Fatalf("missing failure: %s", raw)
	}
}
