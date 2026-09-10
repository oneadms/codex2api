package proxy

import (
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/codex2api/api"
	"github.com/tidwall/gjson"
)

// Codex 只有在 manifest 声明 apply_patch_tool_type 非空时才会带上 apply_patch 工具，
// 而它是 custom（freeform）声明：Trae 没有自由文本工具调用，必须双向桥接——
// 请求侧降级成带单个 input 参数的 function，响应侧还原成 custom_tool_call，
// 否则客户端收到它无法执行的 function_call（"unsupported call"），文件改不动，
// 也不会有任何审批提示。
func TestTraeCNBridgesFreeformApplyPatch(t *testing.T) {
	t.Parallel()
	canonical := []byte(`{
  "model":"deepseek-v3",
  "input":[{"type":"message","role":"user","content":"add a header comment to main.go"}],
  "tools":[
    {"type":"custom","name":"apply_patch","description":"Use the apply_patch tool to edit files.","format":{"type":"grammar","syntax":"lark","definition":"start: begin_patch hunk+ end_patch"}},
    {"type":"function","name":"shell","parameters":{"type":"object","properties":{"command":{"type":"array"}}}}
  ]
}`)
	body, _, bridges, _, err := traeCNRequestBodyPlan(canonical)
	if err != nil {
		t.Fatalf("traeCNRequestBodyPlan() error = %v", err)
	}
	if len(bridges) != 1 || bridges["apply_patch"] != traeCNBridgeCustomTool {
		t.Fatalf("bridges = %v, want apply_patch -> custom_tool_call", bridges)
	}
	tools := gjson.GetBytes(body, "tools").Array()
	if len(tools) != 2 {
		t.Fatalf("tools = %d, want apply_patch + shell: %s", len(tools), body)
	}
	patch := gjson.GetBytes(body, `tools.#(function.name=="apply_patch")`)
	if patch.Get("type").String() != "function" {
		t.Fatalf("apply_patch must reach Trae as a function declaration: %s", body)
	}
	parameters := patch.Get("function.parameters").String()
	if gjson.Get(parameters, "required.0").String() != "input" || gjson.Get(parameters, "properties.input.type").String() != "string" {
		t.Fatalf("bridged schema = %s, want a required string input", parameters)
	}
	// 补丁语法只存在于 custom.format.definition，函数描述里必须带上，模型才知道格式。
	if !strings.Contains(patch.Get("function.description").String(), "start: begin_patch hunk+ end_patch") {
		t.Fatalf("patch grammar was dropped: %s", patch.Raw)
	}
}

// 上游只能回 function 调用，响应侧要按客户端声明的名字还原成 custom_tool_call，
// 并把 {"input": "..."} 脱壳成原始补丁文本。
func TestTraeCNRestoresCustomToolCallFromFunctionBridge(t *testing.T) {
	t.Parallel()
	provider := "event: output\ndata: {\"response\":\"\",\"tool_calls\":[{\"id\":\"call_patch\",\"function_call\":{\"name\":\"apply_patch\",\"arguments\":\"{\\\"input\\\":\\\"*** Begin Patch\\n*** End Patch\\\"}\"}}]}\n\nevent: done\ndata: {\"finish_reason\":\"tool_calls\"}\n\n"
	raw, err := io.ReadAll(traeCNCanonicalStreamForTools(io.NopCloser(strings.NewReader(provider)), "deepseek-v3", traeCNBridges{"apply_patch": traeCNBridgeCustomTool}, nil))
	if err != nil {
		t.Fatal(err)
	}
	events := canonicalSSEEvents(t, raw)
	added, ok := findCanonicalEvent(events, "response.output_item.added")
	if !ok || added.Get("item.type").String() != "custom_tool_call" {
		t.Fatalf("output_item.added = %s, want custom_tool_call", raw)
	}
	inputDone, ok := findCanonicalEvent(events, "response.custom_tool_call_input.done")
	if !ok {
		t.Fatalf("missing custom_tool_call_input.done: %s", raw)
	}
	if got := inputDone.Get("input").String(); got != "*** Begin Patch\n*** End Patch" {
		t.Fatalf("input = %q, want the unwrapped patch text", got)
	}
	if strings.Contains(string(raw), "response.function_call_arguments.delta") {
		t.Fatalf("custom tool must not stream JSON argument deltas: %s", raw)
	}
	completed, ok := findCanonicalEvent(events, "response.completed")
	if !ok || completed.Get(`response.output.#(type=="custom_tool_call").call_id`).String() != "call_patch" {
		t.Fatalf("terminal output lost the custom call: %s", raw)
	}
	// 未声明为 custom 的工具名保持原生 function_call 行为。
	functionRaw, err := io.ReadAll(traeCNCanonicalStreamForTools(io.NopCloser(strings.NewReader(provider)), "deepseek-v3", nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(functionRaw), "response.function_call_arguments.delta") {
		t.Fatalf("undeclared tools must keep function_call streaming: %s", functionRaw)
	}
}

// 下一轮 Codex 会把上一轮的 custom_tool_call / custom_tool_call_output 回放进来，
// 必须能转成 Trae 的 function 调用历史，而不是转成 400。
func TestTraeCNAcceptsCustomToolCallHistory(t *testing.T) {
	t.Parallel()
	canonical := []byte(`{
  "model":"deepseek-v3",
  "input":[
    {"type":"custom_tool_call","call_id":"call_patch","name":"apply_patch","input":"*** Begin Patch\n*** End Patch"},
    {"type":"custom_tool_call_output","call_id":"call_patch","output":"Done!"}
  ],
  "tools":[{"type":"custom","name":"apply_patch","description":"patch"}]
}`)
	body, _, _, _, err := traeCNRequestBodyPlan(canonical)
	if err != nil {
		t.Fatalf("traeCNRequestBodyPlan() error = %v", err)
	}
	call := gjson.GetBytes(body, "messages.0.tool_calls.0")
	if call.Get("id").String() != "call_patch" || call.Get("function_call.name").String() != "apply_patch" ||
		call.Get("function_call.arguments").String() != `{"input":"*** Begin Patch\n*** End Patch"}` {
		t.Fatalf("custom history was not rewrapped as a function call: %s", body)
	}
	if gjson.GetBytes(body, "messages.1.tool_call_id").String() != "call_patch" ||
		gjson.GetBytes(body, "messages.1.content.0.text").String() != "Done!" {
		t.Fatalf("custom tool output was lost: %s", body)
	}
}

// Codex 只在 model_info.apply_patch_tool_type 非空时才注册 apply_patch 工具
// (codex-rs core/src/tools/spec_plan.rs)。网关自建 manifest 必须声明它，否则模型
// 完全没有改文件的手段，也不会触发任何审批提示。
func TestScopedCodexManifestAdvertisesApplyPatchTool(t *testing.T) {
	t.Parallel()
	body, err := buildScopedCodexManifest([]api.Model{{ID: "gpt-5.6-sol", OwnedBy: "trae"}, {ID: "deepseek-v3", OwnedBy: "trae"}})
	if err != nil {
		t.Fatal(err)
	}
	models := gjson.GetBytes(body, "models").Array()
	if len(models) != 2 {
		t.Fatalf("models = %d, want 2: %s", len(models), body)
	}
	for _, model := range models {
		if got := model.Get("apply_patch_tool_type").String(); got != "freeform" {
			t.Fatalf("%s apply_patch_tool_type = %q, want freeform: %s", model.Get("slug").String(), got, body)
		}
		// ultra（"Maximum reasoning with automatic task delegation"）与多智能体
		// 协作工具都由模型信息驱动：缺了这两项，客户端就没有 ultra、子 agent 也
		// 拿不到 spawn_agent。
		if got := model.Get("multi_agent_version").String(); got != "v2" {
			t.Fatalf("%s multi_agent_version = %q, want v2: %s", model.Get("slug").String(), got, body)
		}
		efforts := []string{}
		for _, level := range model.Get("supported_reasoning_levels").Array() {
			efforts = append(efforts, level.Get("effort").String())
		}
		if !slices.Contains(efforts, "ultra") {
			t.Fatalf("%s reasoning levels = %v, want ultra included: %s", model.Get("slug").String(), efforts, body)
		}
	}
}

// ultra 在官方模型上等价于「最大推理 + 自动任务委派」。Trae 背后的模型不会主动
// 调用 spawn_agent，因此网关在 ultra 档位补一条可执行的委派规则；其它档位不动。
func TestTraeCNInjectsUltraDelegationHintOnlyWhenDelegationIsPossible(t *testing.T) {
	t.Parallel()
	canonical := func(effort string, withCollaboration bool) []byte {
		tools := `{"type":"function","name":"exec_command","parameters":{"type":"object"}}`
		if withCollaboration {
			tools += `,{"type":"function","name":"spawn_agent","parameters":{"type":"object"}},{"type":"function","name":"send_message","parameters":{"type":"object"}}`
		}
		return []byte(`{"model":"deepseek-v3","input":"build three pages","reasoning":{"effort":"` + effort + `"},"instructions":"You are Codex.","tools":[` + tools + `]}`)
	}
	systemText := func(body []byte) string {
		for _, message := range gjson.GetBytes(body, "messages").Array() {
			if message.Get("role").String() == "system" {
				return message.Get("content.0.text").String()
			}
		}
		return ""
	}

	body, _, _, _, err := traeCNRequestBodyPlan(canonical("ultra", true))
	if err != nil {
		t.Fatal(err)
	}
	system := systemText(body)
	if !strings.Contains(system, traeUltraDelegationMarker) || !strings.Contains(system, "spawn_agent") {
		t.Fatalf("ultra hint missing: %s", system)
	}
	if !strings.Contains(system, "You are Codex.") {
		t.Fatalf("existing instructions were dropped: %s", system)
	}

	for _, tc := range []struct {
		name          string
		effort        string
		collaboration bool
	}{
		{name: "high_effort", effort: "high", collaboration: true},
		{name: "ultra_without_collaboration_tools", effort: "ultra", collaboration: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, _, _, _, err := traeCNRequestBodyPlan(canonical(tc.effort, tc.collaboration))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(systemText(body), traeUltraDelegationMarker) {
				t.Fatalf("hint injected when it should not be: %s", systemText(body))
			}
		})
	}

	// 没有 instructions 时补一条 system 消息，而不是丢掉指令。
	bare, _, _, _, err := traeCNRequestBodyPlan([]byte(`{"model":"deepseek-v3","input":"hi","reasoning":{"effort":"ultra"},"tools":[{"type":"function","name":"spawn_agent","parameters":{"type":"object"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(systemText(bare), traeUltraDelegationMarker) {
		t.Fatalf("hint missing without instructions: %s", bare)
	}
}

// 能力声明按渠道下发：只有走网关桥接的渠道才声明 freeform 工具和 ultra 档位，
// relay / Antigravity 保持原样，避免把上游没有的能力丢给模型。
func TestScopedCodexManifestScopesCapabilitiesPerChannel(t *testing.T) {
	t.Parallel()
	body, err := buildScopedCodexManifest([]api.Model{
		{ID: "deepseek-v3", OwnedBy: "trae"},
		{ID: "grok-4.6", OwnedBy: "xai"},
		{ID: "some-relay-model", OwnedBy: "codex2api"},
		{ID: "gemini-3.8-flash", OwnedBy: "google"},
		{ID: "gpt-5.5", OwnedBy: "openai"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range gjson.GetBytes(body, "models").Array() {
		slug := model.Get("slug").String()
		efforts := []string{}
		for _, level := range model.Get("supported_reasoning_levels").Array() {
			efforts = append(efforts, level.Get("effort").String())
		}
		// 网关模型都保留 v2，子 agent 才有协作工具；官方 Codex 条目不动。
		wantVersion := "v2"
		if slug == "gpt-5.5" {
			wantVersion = ""
		}
		if got := model.Get("multi_agent_version").String(); got != wantVersion {
			t.Fatalf("%s multi_agent_version = %q, want %q", slug, got, wantVersion)
		}
		switch slug {
		case "gpt-5.5":
			// 官方 Codex 模型的条目由上游 manifest 决定，网关不追加任何声明。
			if model.Get("multi_agent_version").Exists() || model.Get("apply_patch_tool_type").Exists() || len(efforts) != 0 {
				t.Fatalf("codex-official entry was rewritten: %s", model.Raw)
			}
		case "deepseek-v3":
			if model.Get("apply_patch_tool_type").String() != "freeform" || !slices.Contains(efforts, "ultra") {
				t.Fatalf("trae model lost bridged capabilities: %s", model.Raw)
			}
		case "grok-4.6":
			if model.Get("apply_patch_tool_type").String() != "freeform" {
				t.Fatalf("grok model lost apply_patch bridging: %s", model.Raw)
			}
			if len(efforts) != 0 {
				t.Fatalf("grok reasoning ladder must stay provider-owned: %s", model.Raw)
			}
		case "some-relay-model":
			if model.Get("apply_patch_tool_type").Exists() || len(efforts) != 0 {
				t.Fatalf("relay model gained unbridged capabilities: %s", model.Raw)
			}
		case "gemini-3.8-flash":
			if model.Get("apply_patch_tool_type").Exists() || len(efforts) != 0 {
				t.Fatalf("antigravity model gained unbridged capabilities: %s", model.Raw)
			}
		default:
			t.Fatalf("unexpected model %s", slug)
		}
	}
}
