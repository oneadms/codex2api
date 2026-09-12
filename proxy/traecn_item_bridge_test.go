package proxy

import (
	"encoding/json"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// Codex 会回放客户端执行的调用项：托管 shell、托管补丁、MCP、计算机操作、代码
// 解释器等都有自己的 item 形态；Trae 只认 function 调用，网关必须把它们归一成
// 「调用 + 结果」这对消息，而不是整轮 400。
func TestTraeCNBridgesClientOnlyInputItems(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		call    string
		output  string
		wantFn  string
		wantArg string
	}{
		{
			name:    "local_shell",
			call:    `{"type":"local_shell_call","call_id":"c_shell","action":{"type":"exec","command":["ls","-la"]}}`,
			output:  `{"type":"local_shell_call_output","call_id":"c_shell","output":"total 0"}`,
			wantFn:  "local_shell",
			wantArg: "ls",
		},
		{
			name:    "shell",
			call:    `{"type":"shell_call","call_id":"c_sh","action":{"command":["pwd"]}}`,
			output:  `{"type":"shell_call_output","call_id":"c_sh","output":"/tmp"}`,
			wantFn:  "shell",
			wantArg: "pwd",
		},
		{
			name:    "apply_patch",
			call:    `{"type":"apply_patch_call","call_id":"c_ap","action":{"type":"update_file","patch":"*** Begin Patch\n*** End Patch"}}`,
			output:  `{"type":"apply_patch_call_output","call_id":"c_ap","output":"Applied"}`,
			wantFn:  "apply_patch",
			wantArg: "*** Begin Patch",
		},
		{
			name:    "tool_search",
			call:    `{"type":"tool_search_call","call_id":"c_ts","execution":"client","arguments":{"query":"deferred tools"}}`,
			output:  `{"type":"tool_search_output","call_id":"c_ts","output":"2 tools"}`,
			wantFn:  "tool_search",
			wantArg: "deferred tools",
		},
		{
			name:    "mcp",
			call:    `{"type":"mcp_tool_call","call_id":"c_mcp","server":"linear","tool_name":"list_issues","arguments":"{\"limit\":1}"}`,
			output:  `{"type":"mcp_tool_call_output","call_id":"c_mcp","output":"ISSUE-1"}`,
			wantFn:  "linear__list_issues",
			wantArg: "limit",
		},
		{
			name:    "computer",
			call:    `{"type":"computer_call","call_id":"c_cu","action":{"type":"screenshot"}}`,
			output:  `{"type":"computer_call_output","call_id":"c_cu","output":"shot"}`,
			wantFn:  "computer",
			wantArg: "screenshot",
		},
		{
			name:    "code_interpreter",
			call:    `{"type":"code_interpreter_call","call_id":"c_ci","code":"print(1)"}`,
			output:  `{"type":"code_interpreter_call_output","call_id":"c_ci","output":"1"}`,
			wantFn:  "code_interpreter",
			wantArg: "print(1)",
		},
		{
			name:    "file_search",
			call:    `{"type":"file_search_call","call_id":"c_fs","queries":["vector store"]}`,
			output:  `{"type":"file_search_call_output","call_id":"c_fs","output":"hit"}`,
			wantFn:  "file_search",
			wantArg: "vector store",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			canonical := []byte(`{"model":"deepseek-v3","input":[` + tc.call + `,` + tc.output + `]}`)
			body, _, _, _, err := traeCNRequestBodyPlan(canonical)
			if err != nil {
				t.Fatalf("traeCNRequestBodyPlan() error = %v", err)
			}
			call := gjson.GetBytes(body, "messages.0.tool_calls.0")
			if call.Get("function_call.name").String() != tc.wantFn {
				t.Fatalf("call name = %q, want %q: %s", call.Get("function_call.name").String(), tc.wantFn, body)
			}
			if !strings.Contains(call.Get("function_call.arguments").String(), tc.wantArg) {
				t.Fatalf("call arguments lost %q: %s", tc.wantArg, body)
			}
			if gjson.GetBytes(body, "messages.1.role").String() != "tool" ||
				gjson.GetBytes(body, "messages.1.tool_call_id").String() != call.Get("id").String() {
				t.Fatalf("tool result was not paired with the call: %s", body)
			}
		})
	}
}

// 纯元数据 / 纯文本项：能表达的表达成消息，没内容的安静跳过，都不得报错。
func TestTraeCNHandlesMetadataAndTextInputItems(t *testing.T) {
	t.Parallel()
	canonical := []byte(`{"model":"deepseek-v3","input":[
	  {"type":"item_reference","id":"msg_1"},
	  {"type":"compaction","encrypted_content":"opaque"},
	  {"type":"context_compaction"},
	  {"type":"web_search_call","id":"ws_1","status":"completed"},
	  {"type":"image_generation_call","id":"ig_1","result":"png"},
	  {"type":"mcp_approval_request","id":"mcp_1"},
	  {"type":"computer_screenshot","id":"shot_1"},
	  {"type":"input_text","text":"first question"},
	  {"type":"output_text","text":"first answer"},
	  {"type":"agent_message","id":"am_1","content":[{"type":"output_text","text":"delegate report"}]},
	  {"type":"message","role":"user","content":"go on"}
	]}`)
	body, _, _, _, err := traeCNRequestBodyPlan(canonical)
	if err != nil {
		t.Fatalf("traeCNRequestBodyPlan() error = %v", err)
	}
	messages := gjson.GetBytes(body, "messages").Array()
	var got []string
	for _, message := range messages {
		got = append(got, message.Get("role").String()+":"+message.Get("content.0.text").String())
	}
	want := []string{"user:first question", "assistant:first answer", "assistant:delegate report", "user:go on"}
	if len(got) != len(want) {
		t.Fatalf("messages = %v, want %v; body=%s", got, want, body)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("message %d = %q, want %q; body=%s", index, got[index], want[index], body)
		}
	}
}

// 历史从中间开始（输出项先到）时不能 400：补一条占位调用，保证「调用 + 结果」配对。
func TestTraeCNToleratesOrphanToolOutputs(t *testing.T) {
	t.Parallel()
	canonical := []byte(`{"model":"deepseek-v3","input":[
	  {"type":"local_shell_call_output","call_id":"c_orphan","output":"command finished"},
	  {"type":"custom_tool_call_output","call_id":"c_patch","output":"Applied"}
	]}`)
	body, _, _, _, err := traeCNRequestBodyPlan(canonical)
	if err != nil {
		t.Fatalf("traeCNRequestBodyPlan() error = %v", err)
	}
	if gjson.GetBytes(body, "messages.#").Int() != 4 {
		t.Fatalf("want placeholder call + result for each orphan: %s", body)
	}
	if gjson.GetBytes(body, "messages.0.tool_calls.0.function_call.name").String() != "local_shell" ||
		gjson.GetBytes(body, "messages.2.tool_calls.0.function_call.name").String() != "custom_tool" {
		t.Fatalf("placeholder calls are wrong: %s", body)
	}
}

// 托管 shell 声明要能被模型调用，并且调用结果按 item 契约还原：
// 模型回 {"command": ["ls","-la"]} → 客户端收到 local_shell_call。
func TestTraeCNBridgesHostedShellBothWays(t *testing.T) {
	t.Parallel()
	canonical := []byte(`{
  "model":"deepseek-v3",
  "input":[{"type":"message","role":"user","content":"list the directory"}],
  "tools":[{"type":"local_shell"}]
}`)
	body, _, bridges, _, err := traeCNRequestBodyPlan(canonical)
	if err != nil {
		t.Fatalf("traeCNRequestBodyPlan() error = %v", err)
	}
	if bridges["local_shell"] != traeCNBridgeLocalShell {
		t.Fatalf("bridges = %v, want local_shell -> local_shell_call", bridges)
	}
	parameters := gjson.GetBytes(body, `tools.#(function.name=="local_shell").function.parameters`).String()
	if gjson.Get(parameters, "required.0").String() != "command" || gjson.Get(parameters, "properties.command.type").String() != "array" {
		t.Fatalf("hosted shell schema = %s, want a required command array", parameters)
	}

	provider := "event: output\ndata: {\"tool_calls\":[{\"id\":\"call_ls\",\"function_call\":{\"name\":\"local_shell\",\"arguments\":\"{\\\"command\\\":[\\\"ls\\\",\\\"-la\\\"],\\\"timeout_ms\\\":5000}\"}}]}\n\nevent: done\ndata: {\"finish_reason\":\"tool_calls\"}\n\n"
	raw, err := io.ReadAll(traeCNCanonicalStreamForTools(io.NopCloser(strings.NewReader(provider)), "deepseek-v3", bridges, nil))
	if err != nil {
		t.Fatal(err)
	}
	events := canonicalSSEEvents(t, raw)
	added, ok := findCanonicalEvent(events, "response.output_item.added")
	if !ok || added.Get("item.type").String() != traeCNBridgeLocalShell {
		t.Fatalf("output_item.added = %s, want local_shell_call", raw)
	}
	if added.Get("item.action.type").String() != "exec" || added.Get("item.action.command.0").String() != "ls" {
		t.Fatalf("local_shell action was not restored: %s", raw)
	}
	completed, ok := findCanonicalEvent(events, "response.completed")
	if !ok || completed.Get(`response.output.#(type=="local_shell_call").action.command.1`).String() != "-la" {
		t.Fatalf("terminal output lost the hosted shell call: %s", raw)
	}
	if strings.Contains(string(raw), "function_call_arguments.delta") {
		t.Fatalf("bridged tools must not stream function argument deltas: %s", raw)
	}
}

// 实测故障：Codex 的 exec_command 被弱模型写成 {"args":{"cmd":...}}，客户端回
// "failed to parse function arguments: missing field `cmd`"。网关在描述里写清顶层
// 字段，并在终态把多包的一层解开。
func TestTraeCNRepairsArgsWrappedFunctionArguments(t *testing.T) {
	t.Parallel()
	canonical := []byte(`{
  "model":"deepseek-v3",
  "input":[{"type":"message","role":"user","content":"list files"}],
  "tools":[{"type":"function","name":"exec_command","description":"Run a command.","parameters":{"type":"object","properties":{"cmd":{"type":"string"},"shell":{"type":"string"},"max_output_tokens":{"type":"number"}},"required":["cmd"]}}]
}`)
	body, _, _, contracts, err := traeCNRequestBodyPlan(canonical)
	if err != nil {
		t.Fatalf("traeCNRequestBodyPlan() error = %v", err)
	}
	description := gjson.GetBytes(body, `tools.#(function.name=="exec_command").function.description`).String()
	if !strings.Contains(description, "Argument contract") || !strings.Contains(description, "cmd (required)") {
		t.Fatalf("argument contract hint missing: %q", description)
	}
	if got := contracts["exec_command"]; len(got.Required) != 1 || got.Required[0] != "cmd" {
		t.Fatalf("contract = %+v, want cmd required", got)
	}

	for _, tc := range []struct {
		name      string
		arguments string
		want      string
	}{
		{name: "wrapped_in_args", arguments: `{"args":{"cmd":"ls -la","shell":"zsh"}}`, want: `{"cmd":"ls -la","shell":"zsh"}`},
		{name: "wrapped_in_arguments", arguments: `{"arguments":{"cmd":"pwd"}}`, want: `{"cmd":"pwd"}`},
		{name: "already_flat", arguments: `{"cmd":"whoami"}`, want: `{"cmd":"whoami"}`},
		{name: "wrapper_without_required", arguments: `{"args":{"shell":"zsh"}}`, want: `{"args":{"shell":"zsh"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			provider := "event: output\ndata: {\"tool_calls\":[{\"id\":\"call_exec\",\"function_call\":{\"name\":\"exec_command\",\"arguments\":" + strconv.Quote(tc.arguments) + "}}]}\n\nevent: done\ndata: {\"finish_reason\":\"tool_calls\"}\n\n"
			raw, err := io.ReadAll(traeCNCanonicalStreamForTools(io.NopCloser(strings.NewReader(provider)), "deepseek-v3", nil, contracts))
			if err != nil {
				t.Fatal(err)
			}
			events := canonicalSSEEvents(t, raw)
			completed, ok := findCanonicalEvent(events, "response.completed")
			if !ok {
				t.Fatalf("missing completed event: %s", raw)
			}
			got := completed.Get(`response.output.#(type=="function_call").arguments`).String()
			var decoded, want any
			if json.Unmarshal([]byte(got), &decoded) != nil {
				t.Fatalf("arguments are not JSON: %q", got)
			}
			if json.Unmarshal([]byte(tc.want), &want) != nil {
				t.Fatalf("bad fixture: %s", tc.want)
			}
			if !reflect.DeepEqual(decoded, want) {
				t.Fatalf("arguments = %s, want %s; raw=%s", got, tc.want, raw)
			}
		})
	}
}

// 推理条目不回灌上游：此前把 reasoning.summary 拼成 assistant 消息再发给 Trae，
// 模型会把自己上一轮的思维链当成已说过的话继续想，表现就是 Codex 里像死循环一样
// 一直思考。
func TestTraeCNRequestBodyDropsReasoningItems(t *testing.T) {
	t.Parallel()
	canonical := []byte(`{"model":"DeepSeek-V4-Pro","input":[
		{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"用户想让我列出文件，我先想想要不要用 exec_command。"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"列出当前目录"}]}
	]}`)
	body, _, _, _, err := traeCNRequestBodyPlan(canonical)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(body)
	if strings.Contains(raw, "我先想想要不要用 exec_command") {
		t.Fatalf("reasoning summary leaked back to the provider: %s", raw)
	}
	messages := gjson.GetBytes(body, "messages").Array()
	if len(messages) != 1 || messages[0].Get("role").String() != "user" {
		t.Fatalf("messages = %s, want only the user message", raw)
	}
}

// TRAECN_REASONING_DISABLED=1 时推理摘要不再下发给客户端。
func TestTraeCNReasoningEmissionCanBeDisabled(t *testing.T) {
	provider := strings.Join([]string{
		"event: output\ndata: {\"response\":\"\",\"reasoning_content\":\"先想一下\",\"tool_calls\":null}\n",
		"event: output\ndata: {\"response\":\"完成\",\"tool_calls\":null}\n",
		"event: done\ndata: {\"finish_reason\":\"stop\"}\n",
	}, "\n")

	read := func(t *testing.T) string {
		t.Helper()
		raw, err := io.ReadAll(traeCNCanonicalStream(io.NopCloser(strings.NewReader(provider)), "DeepSeek-V4-Pro"))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	t.Setenv("TRAECN_REASONING_DISABLED", "")
	defaultOutput := read(t)
	if !strings.Contains(defaultOutput, "response.reasoning_summary_text.delta") {
		t.Fatalf("reasoning delta missing by default: %s", defaultOutput)
	}

	t.Setenv("TRAECN_REASONING_DISABLED", "1")
	disabledOutput := read(t)
	if strings.Contains(disabledOutput, "response.reasoning_summary_text.delta") {
		t.Fatalf("reasoning delta must be suppressed when disabled: %s", disabledOutput)
	}
	if !strings.Contains(disabledOutput, "完成") {
		t.Fatalf("text output must survive: %s", disabledOutput)
	}
}
