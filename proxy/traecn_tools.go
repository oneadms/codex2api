package proxy

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
)

// 历史工具调用使用 function_call，工具定义仍使用 function，不能在整个请求中统一替换。
func traeCNToolHistoryFromChat(calls gjson.Result) []any {
	history, _ := calls.Value().([]any)
	for _, raw := range history {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if function, exists := call["function"]; exists {
			if call["function_call"] == nil {
				call["function_call"] = function
			}
			delete(call, "function")
		}
	}
	return history
}

func traeCNDeclarationName(tool gjson.Result) string {
	name := strings.TrimSpace(tool.Get("name").String())
	if name == "" {
		name = strings.TrimSpace(tool.Get("function.name").String())
	}
	return name
}

// traeCNCarrierToolRaws 收集 Responses Lite 载体项（input[] 里的
// type=additional_tools）声明的原始工具 JSON。按名去重、先出现者优先：续会话回放
// 会把同一个载体项重复带上。
func traeCNCarrierToolRaws(carrier gjson.Result, taken map[string]struct{}) []string {
	tools := carrier.Get("tools")
	if !tools.IsArray() {
		return nil
	}
	var raws []string
	tools.ForEach(func(_, tool gjson.Result) bool {
		if !tool.IsObject() {
			return true
		}
		if name := traeCNDeclarationName(tool); name != "" {
			if _, exists := taken[name]; exists {
				return true
			}
			taken[name] = struct{}{}
		}
		raws = append(raws, tool.Raw)
		return true
	})
	return raws
}

// traeCNMergedToolSpecs 合并顶层 tools[] 与载体声明的原始 JSON。同名工具以顶层
// 声明为准：顶层是客户端的权威声明，载体只是补充（Trae 只在顶层接收工具声明）。
func traeCNMergedToolSpecs(topLevel gjson.Result, carriers []string) gjson.Result {
	if len(carriers) == 0 {
		return topLevel
	}
	declared := make(map[string]struct{}, len(carriers))
	if topLevel.IsArray() {
		topLevel.ForEach(func(_, tool gjson.Result) bool {
			if name := traeCNDeclarationName(tool); name != "" {
				declared[name] = struct{}{}
			}
			return true
		})
	}
	var merged strings.Builder
	merged.WriteByte('[')
	first := true
	write := func(raw string) {
		if !first {
			merged.WriteByte(',')
		}
		first = false
		merged.WriteString(raw)
	}
	if topLevel.IsArray() {
		topLevel.ForEach(func(_, tool gjson.Result) bool { write(tool.Raw); return true })
	}
	for _, raw := range carriers {
		if name := traeCNDeclarationName(gjson.Parse(raw)); name != "" {
			if _, duplicate := declared[name]; duplicate {
				continue
			}
			declared[name] = struct{}{}
		}
		write(raw)
	}
	merged.WriteByte(']')
	return gjson.Parse(merged.String())
}

// traeCNToolContract 是一个工具在 Trae 侧的函数契约：回程要还原的 item 类型，
// 以及顶层必填/可选参数名（用于把模型多包一层的参数解回来，见
// traeCNUnwrapArguments）。
type traeCNToolContract struct {
	Bridge   string
	Required []string
	Fields   []string
}

type traeCNContracts map[string]traeCNToolContract

// traeCNToolPlanFromResponses 把顶层 tools[] 与 Responses Lite 载体声明一起转换成
// Trae 只接受的 function 声明，并回传每个工具的还原契约。
func traeCNToolPlanFromResponses(topLevel gjson.Result, carriers []string) (gjson.Result, traeCNContracts) {
	return traeCNConvertToolSpecs(traeCNMergedToolSpecs(topLevel, carriers))
}

// traeCNConvertToolSpecs 归一化 Responses 工具声明：
//
//   - function 原样保留（参数名/描述/schema 不变）；
//   - custom（Codex 的 freeform apply_patch 等自由文本工具）降级成带单个 input
//     字符串参数的 function——Trae 没有自由文本工具调用，只能这样表达；工具名
//     会回传，响应侧再还原成 custom_tool_call，否则 Codex 只会看到一个它无法执行
//     的 function_call（表现为 "unsupported call"）；
//   - 托管 shell（local_shell / shell）降级成 shell 函数，响应侧还原成
//     local_shell_call / shell_call，命令才会真的被执行；
//   - namespace 摊平成内部声明；
//   - 其余托管/延迟工具（tool_search、web_search、image_generation、MCP、
//     computer_use 等）在 Trae 没有对应形态，跳过声明而不是让整轮请求失败。
func traeCNConvertToolSpecs(specs gjson.Result) (gjson.Result, traeCNContracts) {
	if !specs.IsArray() {
		return specs, nil
	}
	var out strings.Builder
	out.WriteByte('[')
	first := true
	seen := make(map[string]struct{})
	contracts := traeCNContracts{}
	emit := func(spec map[string]any, bridge string) {
		name, _ := spec["name"].(string)
		if name == "" {
			return
		}
		if _, duplicate := seen[name]; duplicate {
			return
		}
		seen[name] = struct{}{}
		if contract, ok := traeCNContractFromSpec(spec, bridge); ok {
			contracts[name] = contract
		}
		encoded, err := json.Marshal(spec)
		if err != nil {
			return
		}
		if !first {
			out.WriteByte(',')
		}
		first = false
		out.WriteString(string(encoded))
	}
	var walk func(tool gjson.Result)
	walk = func(tool gjson.Result) {
		if !tool.IsObject() {
			return
		}
		switch strings.TrimSpace(tool.Get("type").String()) {
		case "namespace":
			tool.Get("tools").ForEach(func(_, nested gjson.Result) bool { walk(nested); return true })
		case "", "function":
			if spec, ok := traeCNFunctionSpec(tool); ok {
				emit(spec, "")
			}
		case "custom", "apply_patch":
			// 托管 apply_patch 与 freeform 同形：Trae 都用单参数 input 表达。
			if spec, ok := traeCNCustomFunctionSpec(tool); ok {
				emit(spec, traeCNBridgeCustomTool)
			}
		case "local_shell", "shell":
			name := "shell"
			if strings.TrimSpace(tool.Get("type").String()) == "local_shell" {
				name = "local_shell"
				emit(traeCNShellFunctionSpec(tool, name), traeCNBridgeLocalShell)
			} else {
				emit(traeCNShellFunctionSpec(tool, name), traeCNBridgeShellCall)
			}
		default:
			// 上游无法表示的托管工具：跳过声明，保留这一轮请求。
		}
	}
	specs.ForEach(func(_, tool gjson.Result) bool { walk(tool); return true })
	out.WriteByte(']')
	return gjson.Parse(out.String()), contracts
}

// traeCNShellFunctionSpec 把托管 shell 声明降级成 Trae 可调用的函数。
func traeCNShellFunctionSpec(tool gjson.Result, name string) map[string]any {
	spec := map[string]any{
		"type":        "function",
		"name":        name,
		"description": traeCNFirstNonEmpty(traeCNDescription(tool), "Run a shell command in the user's workspace."),
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":           map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Command and arguments to run."},
				"timeout_ms":        map[string]any{"type": "integer", "description": "Optional timeout in milliseconds."},
				"working_directory": map[string]any{"type": "string", "description": "Optional working directory."},
			},
			"required": []any{"command"},
		},
	}
	return spec
}

// traeCNContractFromSpec 从归一后的函数声明里取出顶层参数名，并给描述补一行调用契约。
//
// Trae 侧只有 function 调用，弱模型很容易把参数再包一层（实测 Codex 会话里
// exec_command 被写成 {"args":{"cmd":...}}，客户端因此报 missing field `cmd`）。
// 描述里写清「顶层字段 + 不要嵌套包裹」能显著减少这类形状错误，回程还有
// traeCNUnwrapArguments 兜底。
func traeCNContractFromSpec(spec map[string]any, bridge string) (traeCNToolContract, bool) {
	contract := traeCNToolContract{Bridge: bridge}
	parameters, _ := spec["parameters"].(map[string]any)
	if properties, ok := parameters["properties"].(map[string]any); ok {
		for name := range properties {
			contract.Fields = append(contract.Fields, name)
		}
		sort.Strings(contract.Fields)
	}
	if required, ok := parameters["required"].([]any); ok {
		for _, raw := range required {
			if name, ok := raw.(string); ok && strings.TrimSpace(name) != "" {
				contract.Required = append(contract.Required, name)
			}
		}
	}
	if len(contract.Fields) == 0 {
		return contract, bridge != ""
	}
	fields := make([]string, 0, len(contract.Fields))
	for _, name := range contract.Fields {
		if slices.Contains(contract.Required, name) {
			fields = append(fields, name+" (required)")
			continue
		}
		fields = append(fields, name)
	}
	description, _ := spec["description"].(string)
	hint := "Argument contract: call this tool with a single JSON object whose top-level fields are " + strings.Join(fields, ", ") + ". Do not wrap the object in another field such as \"args\"."
	if description != "" {
		hint = description + "\n\n" + hint
	}
	spec["description"] = hint
	return contract, true
}

// traeUltraDelegationMarker 标记网关注入的委派规则，保证同一请求只注入一次。
const traeUltraDelegationMarker = "[codex2api multi-agent delegation]"

// traeCNUltraDelegationHint 在 ultra 档位给上游模型一条可执行的委派规则。
//
// ultra 在官方模型上等价于「最大推理 + 自动任务委派」，客户端也会注入 multi-agent
// 提示；但 Trae 背后的模型（豆包 / DeepSeek / GLM 等）极少主动调用 spawn_agent，
// 于是表现为「开了 ultra 却没有多智能体」。这里只在 ultra 且客户端确实声明了协作
// 工具时追加一条带阈值和工具名的指令，其余档位完全不动。
func traeCNUltraDelegationHint(effort string, tools gjson.Result) string {
	if !strings.EqualFold(strings.TrimSpace(effort), "ultra") || !traeCNHasCollaborationTool(tools) {
		return ""
	}
	return traeUltraDelegationMarker + "\n" +
		"Multi-agent delegation is active for this turn (reasoning effort: ultra). " +
		"Before starting the work yourself, split the request into independent subtasks (at most 4) and start each one with spawn_agent; " +
		"keep the main thread for coordination, integration and verification. " +
		"Skip delegation only when the whole request is a single small step."
}

// traeCNHasCollaborationTool 判断客户端是否声明了多智能体协作工具。
func traeCNHasCollaborationTool(tools gjson.Result) bool {
	return traeCNAnyTool(tools, func(name, _ string) bool {
		switch name {
		case "spawn_agent", "followup_task", "send_message", "list_agents":
			return true
		}
		return false
	})
}

// traeCNAnyTool 遍历工具声明（含 namespace 嵌套），命中 match 即返回。
// name 已小写；typ 为声明形态（function / custom / namespace 子项等，可能为空）。
func traeCNAnyTool(tools gjson.Result, match func(name, typ string) bool) bool {
	found := false
	var scan func(list gjson.Result)
	scan = func(list gjson.Result) {
		list.ForEach(func(_, tool gjson.Result) bool {
			if found || !tool.IsObject() {
				return !found
			}
			typ := strings.ToLower(strings.TrimSpace(tool.Get("type").String()))
			name := strings.ToLower(traeCNDeclarationName(tool))
			if typ == "namespace" {
				scan(tool.Get("tools"))
				return !found
			}
			if name != "" && match(name, typ) {
				found = true
				return false
			}
			return !found
		})
	}
	scan(tools)
	return found
}

// traeCNHasCallableTool 判断本轮是否给了模型任何能调用的工具。
func traeCNHasCallableTool(tools gjson.Result) bool {
	return traeCNAnyTool(tools, func(_, typ string) bool {
		switch typ {
		case "", "function", "custom", "apply_patch", "local_shell", "shell":
			return true
		}
		return false
	})
}

// traeContinueWorkingMarker 标记网关注入的反收尾规则。
const traeContinueWorkingMarker = "[codex2api continue-working guard]"

// traeCNContinueGuardDisabled 允许运维关闭反收尾规则（TRAECN_CONTINUE_GUARD_DISABLED=1）。
func traeCNContinueGuardDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TRAECN_CONTINUE_GUARD_DISABLED"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// traeCNContinueWorkingHint 在请求带了工具时追加一条反收尾规则。
//
// Trae 上游模型习惯把"我接下来要做什么"当最终答复发出来，而 Responses 语义里
// assistant message 不带后续工具调用就等于本轮结束——表现就是"不设 goal 时任务跑
// 一会就自己停了"。这里要求它：要么继续调用工具，要么给出带结果的最终答复，
// 不要只用一句计划/进度说明收尾。没有工具可调用的纯问答请求不注入。
func traeCNContinueWorkingHint(tools gjson.Result) string {
	if traeCNContinueGuardDisabled() || !traeCNHasCallableTool(tools) {
		return ""
	}
	return traeContinueWorkingMarker + "\n" +
		"Do not end the turn with a status note or a plan. Keep calling tools until the work is actually done, " +
		"and only then give the final answer with the concrete results. " +
		"Never reply with only \"I will…\", \"Let's…\" or a progress sentence: if the task is unfinished, call the next tool instead of replying."
}

// traeCNMessagesContain 判断系统指令里是否已经包含某段文本（避免重复注入）。
func traeCNMessagesContain(messages []map[string]any, needle string) bool {
	if needle == "" {
		return false
	}
	for _, message := range messages {
		content, ok := message["content"].([]any)
		if !ok {
			continue
		}
		for _, rawPart := range content {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := part["text"].(string); ok && strings.Contains(text, needle) {
				return true
			}
		}
	}
	return false
}

// traeCNAppendSystemInstruction 把一段指令并进系统消息（没有系统消息时补一条）。
func traeCNAppendSystemInstruction(messages []map[string]any, text string) []map[string]any {
	if text == "" {
		return messages
	}
	if len(messages) > 0 && messages[0]["role"] == "system" {
		if parts, ok := messages[0]["content"].([]any); ok && len(parts) > 0 {
			if part, ok := parts[0].(map[string]any); ok {
				if existing, ok := part["text"].(string); ok {
					part["text"] = existing + "\n\n" + text
					return messages
				}
			}
		}
	}
	message := map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": text}}}
	return append([]map[string]any{message}, messages...)
}

// traeCNFunctionSpec 归一化单条 function 声明（含 Chat 形态的嵌套 function 写法）。
func traeCNFunctionSpec(tool gjson.Result) (map[string]any, bool) {
	name := strings.TrimSpace(tool.Get("name").String())
	if name == "" {
		name = strings.TrimSpace(tool.Get("function.name").String())
	}
	if name == "" {
		return nil, false
	}
	spec := map[string]any{"type": "function", "name": name}
	if description := traeCNDescription(tool); description != "" {
		spec["description"] = description
	}
	for _, path := range []string{"parameters", "function.parameters", "input_schema", "function.input_schema"} {
		if parameters := tool.Get(path); parameters.Exists() {
			spec["parameters"] = parameters.Value()
			break
		}
	}
	return spec, true
}

// traeCNCustomFunctionSpec 把自由文本工具降级成单参数 function：模型把原始文本放进
// input，网关在响应侧脱壳还原。Codex 把补丁语法放在 format.definition，Trae 只收到
// 函数描述，因此把语法定义一并附到描述里，模型才知道该写什么格式。
func traeCNCustomFunctionSpec(tool gjson.Result) (map[string]any, bool) {
	spec, ok := traeCNFunctionSpec(tool)
	if !ok {
		return nil, false
	}
	description, _ := spec["description"].(string)
	if definition := strings.TrimSpace(tool.Get("format.definition").String()); definition != "" && !strings.Contains(description, definition) {
		const maxDefinition = 6000
		if len(definition) > maxDefinition {
			definition = definition[:maxDefinition]
		}
		if description != "" {
			description += "\n\n"
		}
		description += definition
		spec["description"] = description
	}
	spec["parameters"] = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"input": map[string]any{"type": "string", "description": "The complete raw input for this tool, as free-form text."},
		},
		"required": []any{"input"},
	}
	return spec, true
}

func traeCNDescription(tool gjson.Result) string {
	description := strings.TrimSpace(tool.Get("description").String())
	if description == "" {
		description = strings.TrimSpace(tool.Get("function.description").String())
	}
	return description
}

// traeCNCustomToolInput 从自由文本工具调用的参数里取回原始文本：模型按降级后的
// schema 回 {"input": "..."}，已经是裸文本时原样返回。
func traeCNCustomToolInput(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if parsed := gjson.Parse(trimmed); parsed.IsObject() {
		if input := parsed.Get("input"); input.Type == gjson.String {
			return input.String()
		}
	}
	return trimmed
}

// TRAE 的 FunctionDefinition 将 parameters 定义为字符串，发送前需要把
// JSON Schema 对象序列化一次。已编码的字符串保持原样，避免重复转义。
func traeCNToolsFromResponses(specs gjson.Result) ([]any, error) {
	if err := validateCrossProtocolTools(specs, "Trae CN"); err != nil {
		return nil, err
	}
	tools := responsesToolsToChat(specs)
	for _, tool := range tools {
		function := tool.(map[string]any)["function"].(map[string]any)
		parameters, exists := function["parameters"]
		if !exists || parameters == nil {
			continue
		}
		if _, encoded := parameters.(string); encoded {
			continue
		}
		encoded, err := json.Marshal(parameters)
		if err != nil {
			return nil, fmt.Errorf("encode Trae CN tool %q parameters: %w", function["name"], err)
		}
		function["parameters"] = string(encoded)
	}
	return tools, nil
}
