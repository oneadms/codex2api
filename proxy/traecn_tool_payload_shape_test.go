package proxy

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// 多智能体模式下"子智能体被拉起来却收到空任务"的形状矩阵。
//
// 前半段（round-trip）钉住回程解析不能丢载荷：对象、自由文本、双层编码、多包一层
// 都必须还原成同一份内容；后半段钉住空载荷必须整轮判成 malformed_tool_call，
// 而不是把一个空调用交给客户端去执行。
func TestTraeCNToolPayloadShapeMatrix(t *testing.T) {
	t.Parallel()
	const task = "写一首关于海的诗"
	canonical := []byte(`{"model":"deepseek-v3","input":"hi","tools":[
		{"type":"custom","name":"apply_patch","description":"edit files","format":{"type":"grammar","syntax":"lark","definition":"start: patch"}},
		{"type":"function","name":"followup_task","description":"send a task to an agent","parameters":{"type":"object","properties":{"target":{"type":"string"},"message":{"type":"string"}},"required":["target","message"]}}
	]}`)
	_, _, bridges, contracts, err := traeCNRequestBodyPlan(canonical)
	if err != nil {
		t.Fatal(err)
	}

	// build 用真实契约跑一遍转换，返回终态里的调用项（或失败码）。
	run := func(tool, args string, rawJSON bool) (string, string) {
		t.Helper()
		var arguments any = args
		if rawJSON {
			arguments = json.RawMessage(args)
		}
		data, err := json.Marshal(map[string]any{
			"response": "",
			"tool_calls": []any{map[string]any{
				"id":            "call_1",
				"function_call": map[string]any{"name": tool, "arguments": arguments},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		stream := "event: output\ndata: " + string(data) + "\n\nevent: done\ndata: {\"finish_reason\":\"tool_calls\"}\n\n"
		out, err := io.ReadAll(traeCNCanonicalStreamForTools(io.NopCloser(strings.NewReader(stream)), "deepseek-v3", bridges, contracts))
		if err != nil {
			t.Fatal(err)
		}
		events := canonicalSSEEvents(t, out)
		if failed, ok := findCanonicalEvent(events, "response.failed"); ok {
			return "", failed.Get("response.error.code").String()
		}
		completed, ok := findCanonicalEvent(events, "response.completed")
		if !ok {
			t.Fatalf("既没有 completed 也没有 failed: %s", out)
		}
		item := completed.Get(`response.output.0`)
		return item.Get("input").String(), item.Get("arguments").String()
	}

	for _, tc := range []struct {
		name, tool, args string
		rawJSON          bool
		wantInput        string
		wantArguments    string
		wantFailure      string
	}{
		{name: "custom_object", tool: "apply_patch", args: `{"input":"` + task + `"}`, rawJSON: true, wantInput: task},
		{name: "custom_raw_text", tool: "apply_patch", args: task, wantInput: task},
		{name: "custom_double_encoded", tool: "apply_patch", args: `{"input":"` + task + `"}`, wantInput: task},
		{name: "custom_wrapped", tool: "apply_patch", args: `{"args":{"input":"` + task + `"}}`, rawJSON: true, wantInput: task},
		{name: "custom_empty_object", tool: "apply_patch", args: `{}`, rawJSON: true, wantFailure: "malformed_tool_call"},
		{name: "custom_empty_input", tool: "apply_patch", args: `{"input":""}`, rawJSON: true, wantFailure: "malformed_tool_call"},
		{name: "function_complete", tool: "followup_task", args: `{"target":"Poet agent","message":"` + task + `"}`, rawJSON: true, wantArguments: `{"target":"Poet agent","message":"` + task + `"}`},
		{name: "function_wrapped", tool: "followup_task", args: `{"args":{"target":"Poet agent","message":"` + task + `"}}`, rawJSON: true, wantArguments: `{"target":"Poet agent","message":"` + task + `"}`},
		{name: "function_empty_message", tool: "followup_task", args: `{"target":"Poet agent","message":""}`, rawJSON: true, wantFailure: "malformed_tool_call"},
		{name: "function_empty_object", tool: "followup_task", args: `{}`, rawJSON: true, wantFailure: "malformed_tool_call"},
		{name: "function_only_optional", tool: "followup_task", args: `{"message":"   "}`, rawJSON: true, wantFailure: "malformed_tool_call"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			input, arguments := run(tc.tool, tc.args, tc.rawJSON)
			if tc.wantFailure != "" {
				if input != "" || arguments != tc.wantFailure {
					t.Fatalf("空载荷被交付给客户端: input=%q arguments=%q, want failure=%q", input, arguments, tc.wantFailure)
				}
				return
			}
			if input != tc.wantInput || arguments != tc.wantArguments {
				t.Fatalf("载荷丢失: input=%q arguments=%q, want input=%q arguments=%q", input, arguments, tc.wantInput, tc.wantArguments)
			}
		})
	}
}

// 声明侧：自由文本工具必须带 required 的 input，模型才有依据填参数。
func TestTraeCNBridgedToolDeclarationCarriesRequiredFields(t *testing.T) {
	t.Parallel()
	canonical := []byte(`{"model":"deepseek-v3","input":"hi","tools":[
		{"type":"custom","name":"spawn_agent","description":"spawn","format":{"type":"grammar","syntax":"lark","definition":"start: task"}}
	]}`)
	body, _, _, contracts, err := traeCNRequestBodyPlan(canonical)
	if err != nil {
		t.Fatal(err)
	}
	tool := gjson.GetBytes(body, "tools.0")
	if tool.Get("function.name").String() != "spawn_agent" {
		t.Fatalf("tool declaration = %s", body)
	}
	parameters := tool.Get("function.parameters").String()
	if gjson.Get(parameters, "required.0").String() != "input" ||
		gjson.Get(parameters, "properties.input.type").String() != "string" {
		t.Fatalf("bridged schema = %s, want required string input", parameters)
	}
	if got := contracts["spawn_agent"].Required; len(got) != 1 || got[0] != "input" {
		t.Fatalf("contract required = %v", got)
	}
}
