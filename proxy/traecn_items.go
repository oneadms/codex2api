package proxy

import (
	"encoding/json"
	"strings"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// Responses 里客户端执行的调用项不只有 function_call / custom_tool_call：托管 shell、
// 托管 apply_patch、MCP、计算机操作、代码解释器等都有自己的 item 形态，而且 Codex 会
// 在下一轮把它们连同输出回放进来。Trae 只认 function 调用，因此这些 item 必须被
// 归一成「函数调用 + 工具结果」这对消息，否则整轮请求会以
// "cannot be represented by Trae CN" 400。
const (
	// traeCNBridgeCustomTool 对应 freeform（自由文本）工具：Trae 侧按 {"input": "..."}
	// 调用，响应侧还原成 custom_tool_call。
	traeCNBridgeCustomTool = "custom_tool_call"
	// traeCNBridgeLocalShell / traeCNBridgeShellCall 对应托管 shell：响应侧还原成
	// 对应 item 形态，客户端才会真的执行命令。
	traeCNBridgeLocalShell = "local_shell_call"
	traeCNBridgeShellCall  = "shell_call"
)

// traeCNBridges 是「Trae 函数名 -> 响应侧要还原的 item 类型」。
type traeCNBridges map[string]string

func (b traeCNBridges) add(name, bridge string) {
	if name == "" || bridge == "" {
		return
	}
	if b[name] == bridge {
		return
	}
	b[name] = bridge
}

func (b traeCNBridges) merge(other traeCNBridges) {
	for name, bridge := range other {
		b.add(name, bridge)
	}
}

// traeCNUnwrapArguments 兜住弱模型最常见的形状错误：把参数再包一层。
// 实测 Codex 会话里 exec_command 被写成 {"args":{"cmd":...}}，客户端直接回
// "missing field `cmd`"。当外层对象不满足声明的必填字段、而某个包装字段的
// 对象满足时，解开这一层；其余情况原样返回（含合法 JSON 之外的输入）。
func traeCNUnwrapArguments(arguments string, contract traeCNToolContract) string {
	if len(contract.Required) == 0 {
		return arguments
	}
	trimmed := strings.TrimSpace(arguments)
	if !strings.HasPrefix(trimmed, "{") {
		return arguments
	}
	var outer map[string]json.RawMessage
	if json.Unmarshal([]byte(trimmed), &outer) != nil {
		return arguments
	}
	missing := func(value map[string]json.RawMessage) bool {
		for _, field := range contract.Required {
			if _, ok := value[field]; !ok {
				return true
			}
		}
		return false
	}
	if !missing(outer) {
		return arguments
	}
	var candidate []byte
	for _, wrapper := range []string{"args", "arguments", "params", "parameters", "input"} {
		raw, ok := outer[wrapper]
		if !ok || len(outer) != 1 {
			continue
		}
		var inner map[string]json.RawMessage
		if json.Unmarshal(raw, &inner) != nil || missing(inner) {
			continue
		}
		candidate = raw
		break
	}
	if len(candidate) == 0 {
		return arguments
	}
	return string(candidate)
}

// traeCNBridgedCall 是一条被归一成函数调用的客户端调用项。
type traeCNBridgedCall struct {
	Name      string
	CallID    string
	Arguments string
	// Bridge 非空表示响应侧要还原成该 item 类型；为空表示只作历史上下文
	// （例如 MCP/代码解释器这类 Trae 提供不了的托管工具）。
	Bridge string
}

func traeCNFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func traeCNJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

// traeCNShellArguments 从托管 shell 的 action 里取回命令参数（Trae 侧的函数参数形态）。
func traeCNShellArguments(action gjson.Result) string {
	payload := map[string]any{}
	if action.IsObject() {
		if command := action.Get("command"); command.Exists() {
			payload["command"] = command.Value()
		}
		for _, field := range []string{"timeout_ms", "env", "working_directory", "user"} {
			if value := action.Get(field); value.Exists() && value.Type != gjson.Null {
				payload[field] = value.Value()
			}
		}
	}
	return traeCNJSON(payload)
}

// traeCNShellAction 反向构造托管 shell 的 action：模型按降级后的 schema 回
// {"command": [...]}，这里还原成 item 需要的结构。
func traeCNShellAction(arguments, bridge string) map[string]any {
	action := map[string]any{}
	if parsed := gjson.Parse(strings.TrimSpace(arguments)); parsed.IsObject() {
		switch command := parsed.Get("command"); {
		case command.IsArray():
			action["command"] = command.Value()
		case command.Type == gjson.String:
			action["command"] = []any{command.String()}
		}
		for _, field := range []string{"timeout_ms", "env", "working_directory", "user"} {
			if value := parsed.Get(field); value.Exists() && value.Type != gjson.Null {
				action[field] = value.Value()
			}
		}
	}
	if _, exists := action["command"]; !exists {
		action["command"] = []any{}
	}
	if bridge == traeCNBridgeLocalShell {
		action["type"] = "exec"
	}
	return action
}

// traeCNPatchText 从托管 apply_patch 的 action 里取回补丁文本。
func traeCNPatchText(action gjson.Result) string {
	if !action.IsObject() {
		return traeArgumentString(action)
	}
	for _, field := range []string{"patch", "diff", "input", "text"} {
		if value := action.Get(field); value.Type == gjson.String && value.String() != "" {
			return value.String()
		}
	}
	return strings.TrimSpace(action.Raw)
}

// traeCNBridgedCallFromItem 归一化一条调用项；typ 是已经解析出的 item 类型。
func traeCNBridgedCallFromItem(item gjson.Result, typ string) (traeCNBridgedCall, bool) {
	call := traeCNBridgedCall{
		CallID: traeCNFirstNonEmpty(item.Get("call_id").String(), item.Get("id").String()),
	}
	if call.CallID == "" {
		call.CallID = "call_" + uuid.NewString()
	}
	arguments := func() string {
		return traeArgumentString(item.Get("arguments"))
	}
	switch typ {
	case "custom_tool_call":
		call.Name = strings.TrimSpace(item.Get("name").String())
		call.Arguments = traeCNJSON(map[string]any{"input": item.Get("input").String()})
		call.Bridge = traeCNBridgeCustomTool
	case "local_shell_call", "shell_call":
		call.Name = traeCNFirstNonEmpty(item.Get("name").String(), strings.TrimSuffix(typ, "_call"))
		call.Arguments = traeCNShellArguments(item.Get("action"))
		if typ == "local_shell_call" {
			call.Bridge = traeCNBridgeLocalShell
		} else {
			call.Bridge = traeCNBridgeShellCall
		}
	case "apply_patch_call":
		call.Name = traeCNFirstNonEmpty(item.Get("name").String(), "apply_patch")
		call.Arguments = traeCNJSON(map[string]any{"input": traeCNPatchText(item.Get("action"))})
	case "tool_search_call":
		call.Name = traeCNFirstNonEmpty(item.Get("name").String(), "tool_search")
		if call.Arguments = arguments(); call.Arguments == "" {
			call.Arguments = traeCNJSON(map[string]any{"query": item.Get("query").String()})
		}
	case "mcp_tool_call", "mcp_call":
		server := traeCNFirstNonEmpty(item.Get("server").String(), item.Get("server_label").String())
		tool := traeCNFirstNonEmpty(item.Get("tool_name").String(), item.Get("name").String())
		call.Name = traeCNFirstNonEmpty(strings.Trim(strings.Join([]string{server, tool}, "__"), "_"), "mcp_tool")
		if call.Arguments = arguments(); call.Arguments == "" {
			call.Arguments = traeArgumentString(item.Get("input"))
		}
	case "computer_call":
		call.Name = "computer"
		call.Arguments = traeArgumentString(item.Get("action"))
	case "code_interpreter_call":
		call.Name = "code_interpreter"
		call.Arguments = traeCNJSON(map[string]any{"code": item.Get("code").String()})
	case "file_search_call":
		call.Name = "file_search"
		call.Arguments = traeCNJSON(map[string]any{"queries": item.Get("queries").Value()})
	default:
		return call, false
	}
	if call.Arguments == "" {
		call.Arguments = "{}"
	}
	return call, true
}

// traeCNBridgedOutputFromItem 归一化一条调用输出项，返回 (call_id, 文本, 是否需要
// 占位调用)。占位调用用于历史从中间开始时保持「调用 + 结果」配对。
func traeCNBridgedOutputFromItem(item gjson.Result, typ string) (string, string, string) {
	callID := traeCNFirstNonEmpty(item.Get("call_id").String(), item.Get("id").String())
	text := traeToolOutputText(item.Get("output"))
	if text == "" {
		text = traeToolOutputText(item.Get("result"))
	}
	name := strings.TrimSuffix(strings.TrimSuffix(typ, "_output"), "_call")
	if name == "" {
		name = "tool"
	}
	return callID, text, name
}

// traeCNInputItemIsMetadata 判断一条 input 项是否只承载元数据、没有任何对话内容
// （Trae 侧直接跳过即可，不必也不该变成消息）。
func traeCNInputItemIsMetadata(typ string) bool {
	switch typ {
	case "item_reference", "compaction", "compaction_trigger", "context_compaction",
		"encrypted_content", "computer_screenshot", "web_search_call", "image_generation_call",
		"mcp_list_tools", "mcp_approval_request", "mcp_approval_response",
		"input_image", "image", "file":
		return true
	}
	return false
}

// traeCNInputItemText 取一条纯文本 input 项的内容与角色。
func traeCNInputItemText(item gjson.Result, typ string) (role, text string) {
	switch typ {
	case "input_text":
		return "user", traeFirstText(item, "text", "content")
	case "output_text":
		return "assistant", traeFirstText(item, "text", "content")
	case "refusal":
		return "assistant", traeFirstText(item, "refusal", "text")
	case "summary_text":
		return "assistant", traeFirstText(item, "text", "content")
	case "agent_message":
		return "assistant", traeFirstText(item, "content", "text", "message")
	}
	return "", ""
}
