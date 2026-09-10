package proxy

import (
	"encoding/json"
	"fmt"

	"github.com/tidwall/gjson"
)

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
