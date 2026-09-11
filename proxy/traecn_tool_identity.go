package proxy

import (
	"crypto/sha256"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

const traeCNBridgeToolSearch = "tool_search_call"

func traeCNNamespaceChildren(tool gjson.Result) gjson.Result {
	if children := tool.Get("tools"); children.IsArray() {
		return children
	}
	return tool.Get("children")
}

// 展平名在声明、历史和工具选择之间保持一致；长名称用摘要保留区分能力。
func traeCNToolWireName(namespace, name string) string {
	namespace, name = strings.TrimSpace(namespace), strings.TrimSpace(name)
	if namespace == "" {
		return name
	}
	full := namespace + "__" + name
	if len(full) <= 64 {
		return full
	}
	sum := sha256.Sum256([]byte(full))
	prefix := full[:46]
	for !utf8.ValidString(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	return fmt.Sprintf("%s__%x", prefix, sum[:8])
}

func traeCNToolChoiceToChat(choice gjson.Result) (any, error) {
	if !choice.IsObject() {
		return responsesToolChoiceToChat(choice)
	}
	switch choice.Get("type").String() {
	case "function", "custom":
		name := traeCNDeclarationName(choice)
		namespace := traeFirstText(choice, "namespace", "function.namespace")
		return map[string]any{"type": "function", "function": map[string]any{"name": traeCNToolWireName(namespace, name)}}, nil
	case "tool_search":
		return map[string]any{"type": "function", "function": map[string]any{"name": "tool_search"}}, nil
	default:
		return responsesToolChoiceToChat(choice)
	}
}

func traeCNToolSearchSpec() map[string]any {
	return map[string]any{
		"type": "function", "name": "tool_search",
		"description": "Find tools available to the client. Search for the capability needed next, then use the returned tools to continue the task.",
		"parameters": map[string]any{
			"type": "object", "properties": map[string]any{
				"query": map[string]any{"type": "string"},
				"limit": map[string]any{"type": "integer"},
			},
			"required": []any{"query"},
		},
	}
}

// 每个输出阶段都恢复客户端声明的名字和命名空间，不能只修 completed 里的副本。
func (s *traeCNCanonicalState) toolItem(tool *traeCNToolCall, kind, arguments, status string) map[string]any {
	item := map[string]any{"id": tool.id, "type": kind, "call_id": tool.callID, "name": tool.name, "status": status}
	if contract, ok := s.contracts[tool.name]; ok {
		if contract.Name != "" {
			item["name"] = contract.Name
		}
		if contract.Namespace != "" {
			item["namespace"] = contract.Namespace
		}
	}
	if kind == "custom_tool_call" {
		item["input"] = arguments
	} else {
		item["arguments"] = arguments
	}
	return item
}

// tool_search 由 Codex 客户端执行，回程必须恢复调用项及对象形式的 arguments。
func (s *traeCNCanonicalState) emitToolSearch(writer io.Writer, tool *traeCNToolCall, status string) (map[string]any, error) {
	arguments := traeCNUnwrapArguments(tool.arguments.String(), s.contracts[tool.name])
	item := map[string]any{"id": tool.id, "type": traeCNBridgeToolSearch, "call_id": tool.callID, "execution": "client", "arguments": gjson.Parse(arguments).Value(), "status": "in_progress"}
	if err := writeTraeCanonicalEvent(writer, marshalTraeCanonicalEvent("response.output_item.added", map[string]any{"response_id": s.responseID, "output_index": tool.outputIndex, "item": item})); err != nil {
		return nil, err
	}
	item["status"] = status
	if status == "completed" {
		if err := writeTraeCanonicalEvent(writer, marshalTraeCanonicalEvent("response.output_item.done", map[string]any{"response_id": s.responseID, "output_index": tool.outputIndex, "item": item})); err != nil {
			return nil, err
		}
	}
	return item, nil
}
