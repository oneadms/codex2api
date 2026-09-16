package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptrace"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/security"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// traeResultText returns a textual provider value without serializing null.
// Trae has used both scalar content and arrays of {type,text/content} blocks.
func traeResultText(value gjson.Result) string {
	if !value.Exists() || value.Type == gjson.Null {
		return ""
	}
	if value.Type == gjson.String {
		return value.String()
	}
	if value.IsArray() {
		var result strings.Builder
		value.ForEach(func(_, part gjson.Result) bool {
			if text := traeResultText(part); text != "" {
				result.WriteString(text)
			}
			return true
		})
		return result.String()
	}
	if value.IsObject() {
		for _, path := range []string{"text", "content", "response", "value", "message.content"} {
			if text := traeResultText(value.Get(path)); text != "" {
				return text
			}
		}
		return ""
	}
	return strings.TrimSpace(value.String())
}

func traeFirstText(root gjson.Result, paths ...string) string {
	for _, path := range paths {
		if value := traeResultText(root.Get(path)); value != "" {
			return value
		}
	}
	return ""
}

func traeArgumentString(value gjson.Result) string {
	if !value.Exists() || value.Type == gjson.Null {
		return ""
	}
	if value.Type == gjson.String {
		return value.String()
	}
	if value.IsObject() || value.IsArray() || value.Type == gjson.JSON {
		return strings.TrimSpace(value.Raw)
	}
	return strings.TrimSpace(value.String())
}

func traeToolOutputText(value gjson.Result) string {
	if text := traeResultText(value); text != "" {
		return text
	}
	if value.IsObject() || value.IsArray() || value.Type == gjson.JSON {
		return strings.TrimSpace(value.Raw)
	}
	return ""
}

// traeCNMessagesFromResponses converts the canonical Responses request used by
// the proxy handlers into the message envelope expected by llm_utils_chat.
// The second return value carries the raw tool declarations lifted out of
// Responses Lite input carrier items (type=additional_tools); the caller merges
// them into the top-level tools[] because Trae only accepts function
// declarations there.
func traeCNMessagesFromResponses(body []byte) ([]map[string]any, []string, traeCNBridges, error) {
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return nil, nil, nil, fmt.Errorf("Trae CN request must be a JSON object")
	}
	messages := make([]map[string]any, 0)
	if instructions := strings.TrimSpace(root.Get("instructions").String()); instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": instructions}}})
	}
	contentParts := func(content gjson.Result) []any {
		if content.Type == gjson.String {
			return []any{map[string]any{"type": "text", "text": content.String()}}
		}
		if !content.IsArray() {
			return []any{map[string]any{"type": "text", "text": traeResultText(content)}}
		}
		parts := make([]any, 0)
		content.ForEach(func(_, part gjson.Result) bool {
			typ := strings.TrimSpace(part.Get("type").String())
			switch typ {
			case "input_text", "output_text", "text", "refusal", "summary_text":
				if text := traeFirstText(part, "text", "content"); text != "" {
					parts = append(parts, map[string]any{"type": "text", "text": text})
				}
			case "input_image", "image_url":
				imageURL := ""
				imageValue := part.Get("image_url")
				if imageValue.Type == gjson.String {
					imageURL = strings.TrimSpace(imageValue.String())
				} else {
					imageURL = strings.TrimSpace(part.Get("image_url.url").String())
				}
				if imageURL == "" {
					imageURL = strings.TrimSpace(part.Get("source.url").String())
				}
				if imageURL != "" {
					parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}})
				}
			default:
				if text := traeFirstText(part, "text", "content"); text != "" {
					parts = append(parts, map[string]any{"type": "text", "text": text})
				}
			}
			return true
		})
		if len(parts) == 0 {
			parts = append(parts, map[string]any{"type": "text", "text": ""})
		}
		return parts
	}

	appendMessage := func(item gjson.Result) {
		role := strings.ToLower(strings.TrimSpace(item.Get("role").String()))
		if role == "developer" {
			role = "system"
		}
		if role == "" {
			role = "user"
		}
		message := map[string]any{"role": role, "content": contentParts(item.Get("content"))}
		if callID := strings.TrimSpace(item.Get("tool_call_id").String()); callID != "" {
			message["tool_call_id"] = callID
		}
		if calls := item.Get("tool_calls"); calls.IsArray() {
			message["tool_calls"] = traeCNToolHistoryFromChat(calls)
		}
		messages = append(messages, message)
	}

	input := root.Get("input")
	knownCalls := make(map[string]struct{})
	// 载体项声明的工具按名去重（续会话回放会重复带上同一个载体）。
	var carrierTools []string
	carrierToolNames := make(map[string]struct{})
	// 被降级成 function 的工具（freeform / 托管 shell）：响应侧要按这个名字还原
	// 成客户端能执行的 item 形态。历史里的调用同样登记，续会话照旧可用。
	bridges := traeCNBridges{}
	appendToolCall := func(call traeCNBridgedCall) {
		item := map[string]any{"id": call.CallID, "type": "function", "function_call": map[string]any{"name": call.Name, "arguments": call.Arguments}}
		// 同一批并行调用属于同一条 assistant 消息，后面再跟各自的工具结果。
		if len(messages) > 0 {
			last := messages[len(messages)-1]
			if calls, ok := last["tool_calls"].([]any); ok && last["role"] == "assistant" {
				last["tool_calls"] = append(calls, item)
				return
			}
		}
		messages = append(messages, map[string]any{
			"role": "assistant", "content": []any{},
			"tool_calls": []any{item},
		})
	}
	appendToolOutput := func(callID, text string) {
		messages = append(messages, map[string]any{"role": "tool", "tool_call_id": callID, "content": []any{map[string]any{"type": "text", "text": text}}})
	}
	if input.Type == gjson.String {
		messages = append(messages, map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": input.String()}}})
	} else if input.IsArray() {
		var conversionErr error
		input.ForEach(func(_, item gjson.Result) bool {
			typ := strings.TrimSpace(item.Get("type").String())
			if typ == "" && item.Get("role").Exists() {
				typ = "message"
			}
			switch typ {
			case "message":
				appendMessage(item)
			case "reasoning":
				// 推理条目不回灌给上游。此前把 reasoning.summary 拼成 assistant 消息再发给
				// Trae，模型会把自己上一轮的思维链当成"已经说过的话"接着往下想，表现就是
				// Codex 里一直显示在思考、像死循环（同样的推理内容被来回喂）。
				// 客户端要的上下文由最终的 message（正文）与工具调用/结果承载，推理本身
				// 只是过程，丢回上游只会加重重复。
				_ = item
			case "function_call":
				callID := strings.TrimSpace(item.Get("call_id").String())
				if callID == "" {
					callID = strings.TrimSpace(item.Get("id").String())
				}
				if callID == "" {
					conversionErr = fmt.Errorf("function_call requires call_id")
					return false
				}
				arguments := item.Get("arguments").String()
				if strings.TrimSpace(arguments) == "" {
					arguments = "{}"
				}
				knownCalls[callID] = struct{}{}
				appendToolCall(traeCNBridgedCall{CallID: callID, Name: traeCNToolWireName(item.Get("namespace").String(), item.Get("name").String()), Arguments: arguments})
			case "function_call_output":
				callID := strings.TrimSpace(item.Get("call_id").String())
				if callID == "" {
					conversionErr = fmt.Errorf("function_call_output requires call_id")
					return false
				}
				if _, ok := knownCalls[callID]; !ok {
					conversionErr = fmt.Errorf("orphan function_call_output for unknown call_id %q", callID)
					return false
				}
				messages = append(messages, map[string]any{"role": "tool", "tool_call_id": callID, "content": []any{map[string]any{"type": "text", "text": traeToolOutputText(item.Get("output"))}}})
			case "custom_tool_call", "local_shell_call", "shell_call", "apply_patch_call",
				"tool_search_call", "mcp_tool_call", "mcp_call", "computer_call",
				"code_interpreter_call", "file_search_call":
				// 客户端执行的调用项：归一成 Trae 的 function 调用历史。Codex 会在
				// 下一轮回放这些形态（含托管 shell / 托管补丁），不处理就整轮 400。
				call, ok := traeCNBridgedCallFromItem(item, typ)
				if !ok {
					conversionErr = fmt.Errorf("Responses input item type %q cannot be represented by Trae CN", typ)
					return false
				}
				knownCalls[call.CallID] = struct{}{}
				bridges.add(call.Name, call.Bridge)
				appendToolCall(call)
			case "custom_tool_call_output", "local_shell_call_output", "shell_call_output",
				"apply_patch_call_output", "tool_search_output", "tool_search_call_output",
				"mcp_tool_call_output", "mcp_call_output", "computer_call_output",
				"code_interpreter_call_output", "file_search_call_output":
				if typ == "tool_search_output" || typ == "tool_search_call_output" {
					// 搜索结果携带本轮新加载的工具；只保留结果文字会让下一轮无工具可用。
					carrierTools = append(carrierTools, traeCNCarrierToolRaws(item, carrierToolNames)...)
				}
				callID, text, placeholder := traeCNBridgedOutputFromItem(item, typ)
				if callID == "" {
					// 输出项没有 call_id：保持内容，作为一个用户可见的结果文本。
					messages = append(messages, map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}})
					return true
				}
				if _, known := knownCalls[callID]; !known {
					// 历史从中间开始：补一条占位调用，保持「调用 + 结果」配对。
					knownCalls[callID] = struct{}{}
					appendToolCall(traeCNBridgedCall{Name: placeholder, CallID: callID, Arguments: "{}"})
				}
				appendToolOutput(callID, text)
			case "additional_tools":
				// Responses Lite 把延迟工具声明放进 input[] 载体项，它本身不是
				// 对话内容：可表示的工具提升到顶层 tools[]，不产生消息。
				carrierTools = append(carrierTools, traeCNCarrierToolRaws(item, carrierToolNames)...)
			case "":
				// Ignore empty input items emitted by some Responses clients.
			default:
				if traeCNInputItemIsMetadata(typ) {
					// 只承载元数据（item_reference、压缩标记、托管工具的结果载体等）：
					// 没有对话内容可表达，跳过。
					return true
				}
				if role, text := traeCNInputItemText(item, typ); role != "" {
					if text == "" {
						return true
					}
					messages = append(messages, map[string]any{"role": role, "content": []any{map[string]any{"type": "text", "text": text}}})
					return true
				}
				conversionErr = fmt.Errorf("Responses input item type %q cannot be represented by Trae CN", typ)
				return false
			}
			return true
		})
		if conversionErr != nil {
			return nil, nil, nil, conversionErr
		}
	} else if messagesInput := root.Get("messages"); messagesInput.IsArray() {
		// Defensive support for direct tests/callers; production handlers provide
		// canonical Responses input.
		messagesInput.ForEach(func(_, item gjson.Result) bool { appendMessage(item); return true })
	}
	if len(messages) == 0 {
		messages = append(messages, map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": ""}}})
	}
	return messages, carrierTools, bridges, nil
}

func buildTraeCNRequestBody(canonical []byte) ([]byte, string, error) {
	body, model, _, _, err := traeCNRequestBodyPlan(canonical)
	return body, model, err
}

// traeCNRequestBodyPlan 除了 Trae 请求体与模型名，还回传被降级成 function 的
// custom 工具名（Codex 的 freeform apply_patch 等）：响应侧必须把这些名字的调用
// 还原成 custom_tool_call，否则客户端只会看到一个它无法执行的 function_call。
// knownConfigSets[0] 是账号可用模型名集合（= 上游 config_name）。不传或传 nil 表示
// 调用方不做校验（测试/无账号上下文），此时只要不是 auto 就照发。
func traeCNRequestBodyPlan(canonical []byte, knownConfigSets ...[]string) ([]byte, string, traeCNBridges, traeCNContracts, error) {
	var knownConfigs []string
	if len(knownConfigSets) > 0 {
		knownConfigs = knownConfigSets[0]
	}
	root := gjson.ParseBytes(canonical)
	if !root.IsObject() {
		return nil, "", nil, nil, fmt.Errorf("invalid canonical request")
	}
	model := strings.TrimSpace(root.Get("model").String())
	messages, carrierTools, bridges, err := traeCNMessagesFromResponses(canonical)
	if err != nil {
		return nil, model, nil, nil, err
	}
	functionName := "chat_v3"
	targetModel := auth.TraeCNRequestModel(model)
	if targetModel == "" || strings.EqualFold(targetModel, "auto") {
		functionName = "inline_chat"
	}
	specs, contracts, err := traeCNToolPlanFromResponses(root.Get("tools"), carrierTools)
	if err != nil {
		return nil, model, nil, nil, err
	}
	if hint := traeCNUltraDelegationHint(root.Get("reasoning.effort").String(), traeCNMergedToolSpecs(root.Get("tools"), carrierTools)); hint != "" {
		messages = traeCNAppendSystemInstruction(messages, hint)
	}
	// 反收尾规则：Trae 后端模型爱把"接下来要做什么"当最终答复，客户端就判定本轮
	// 结束（不设 goal 时表现为任务自己停）。只在请求带了工具时注入。
	if hint := traeCNContinueWorkingHint(specs); hint != "" && !traeCNMessagesContain(messages, traeContinueWorkingMarker) {
		messages = traeCNAppendSystemInstruction(messages, hint)
	}
	body := map[string]any{"messages": messages, "function": functionName, "stream": true}
	if targetModel != "" && !strings.EqualFold(targetModel, "auto") {
		// 内置兼容别名表已删除：管理员在 TRAECN 设置里配置的映射目标就是上游模型名。
		//
		// 关键：Trae 服务端按 `config_name` 选后端，`model` 只是展示名——只发 model
		// 时上游会回落到默认后端（实测同一个账号：只发 model=glm-5.3-flash 得到
		// "我是豆包大语言模型"，补上 config_name=glm-5.3-flash 才真的走 GLM）。
		// auto（inline_chat）不指定，交给上游自动选。
		body["model"] = targetModel
		// Trae 按 config_name 选后端、而且区分大小写（deepseek-v4-pro -> 4001，
		// DeepSeek-V4-Pro -> 正常），所以先用账号目录把名字校正成 provider 的逐字写法。
		if knownConfigs == nil {
			body["config_name"] = targetModel
		} else if configName := traeCNResolveConfigName(targetModel, knownConfigs); configName != "" {
			body["model"] = configName
			body["config_name"] = configName
		} else {
			// 名字不在目录里：宁可只发 model（上游回落默认后端）也不要发出去拿 4001。
			log.Printf("[TRAECN] model %q is not an upstream config_name; sending model only (upstream will pick its default backend)", targetModel)
		}
	}
	for name, contract := range contracts {
		bridges.add(name, contract.Bridge)
	}
	tools, err := traeCNToolsFromResponses(specs)
	if err != nil {
		return nil, model, nil, nil, err
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if choice := root.Get("tool_choice"); choice.Exists() {
		converted, choiceErr := traeCNToolChoiceToChat(choice)
		if choiceErr != nil {
			return nil, model, nil, nil, choiceErr
		}
		if converted != nil {
			body["tool_choice"] = converted
		}
	}
	if parallel := root.Get("parallel_tool_calls"); parallel.Exists() {
		body["parallel_tool_calls"] = parallel.Bool()
	}
	if effort := strings.TrimSpace(root.Get("reasoning.effort").String()); effort != "" {
		body["reasoning_effort"] = effort
	}
	if maxTokens := root.Get("max_output_tokens"); maxTokens.Exists() {
		body["max_tokens"] = maxTokens.Value()
	} else if maxTokens := root.Get("max_completion_tokens"); maxTokens.Exists() {
		body["max_tokens"] = maxTokens.Value()
	} else if maxTokens := root.Get("max_tokens"); maxTokens.Exists() {
		body["max_tokens"] = maxTokens.Value()
	}
	for _, field := range []string{"temperature", "top_p", "presence_penalty", "frequency_penalty"} {
		if value := root.Get(field); value.Exists() {
			body[field] = value.Value()
		}
	}
	if stop := root.Get("stop"); stop.Exists() {
		if stop.Type == gjson.String {
			body["stop"] = []string{stop.String()}
		} else if stop.IsArray() {
			body["stop"] = stop.Value()
		}
	}
	if seed := root.Get("seed"); seed.Exists() && seed.Int() != 0 {
		body["seed"] = seed.Value()
	}
	if n := root.Get("n"); n.Exists() && n.Int() > 1 {
		body["n"] = n.Value()
	}
	encoded, err := json.Marshal(body)
	return encoded, model, bridges, contracts, err
}

func traeCanonicalFailure(id, model, code, message string, usage map[string]any, providerErrors ...gjson.Result) []byte {
	if strings.TrimSpace(code) == "" || code == "0" {
		code = ErrorCodeUpstreamError
	}
	if strings.TrimSpace(message) == "" {
		message = "Trae CN upstream request failed"
	}
	detail := map[string]any{"code": code, "message": message}
	switch traeCNLimitKind(code, message) {
	case "usage_limit":
		detail["type"], detail["status_code"] = "insufficient_quota", http.StatusTooManyRequests
	case "rate_limited":
		detail["type"], detail["status_code"] = "rate_limit_exceeded", http.StatusTooManyRequests
	}
	if len(providerErrors) > 0 && detail["status_code"] == http.StatusTooManyRequests {
		for _, key := range []string{"resets_at", "resets_in_seconds", "retry_after"} {
			if value := traeFirstText(providerErrors[0], "error."+key, key); value != "" {
				detail[key] = value
			}
		}
	}
	payload, _ := json.Marshal(map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"id": id, "object": "response", "status": "failed", "model": model,
			"output": []any{}, "usage": usage,
			"error": detail,
		},
	})
	return payload
}

func parseTraePayload(data []byte) (gjson.Result, string) {
	parsed := gjson.ParseBytes(data)
	if parsed.Type == gjson.String {
		candidate := strings.TrimSpace(parsed.String())
		if gjson.Valid(candidate) {
			parsed = gjson.Parse(candidate)
		}
	}
	eventType := ""
	cursor := parsed
	for depth := 0; depth < 4; depth++ {
		if eventType == "" {
			eventType = strings.TrimSpace(cursor.Get("type").String())
		}
		nested := cursor.Get("data")
		if !nested.Exists() || !nested.IsObject() {
			break
		}
		cursor = nested
	}
	if eventType == "" {
		eventType = strings.TrimSpace(cursor.Get("type").String())
	}
	return cursor, eventType
}

func traeProviderError(root gjson.Result) (string, string) {
	code := traeFirstText(root, "error.code", "error.type", "code", "error_code", "errorCode")
	message := traeFirstText(root, "error.message", "error", "message", "msg", "extra.message", "detail")
	return strings.TrimSpace(code), strings.TrimSpace(message)
}

const (
	traeCNModelsPath             = "/v1/models"
	traeCNModelsDetailCompatPath = "/v1/models/detail?function=chat_v3"
	traeCNModelsDetailPath       = "/api/ide/v1/get_detail_param"
	// 桌面客户端真正在用的是批量接口：单数 get_detail_param 返回的目录会落后
	// （实测 chat_v3 少 9 个模型，包括 glm-5.3-flash / kimi-k3 / qwen3.8-*）。
	traeCNModelsBatchDetailPath = "/api/ide/v1/batch_get_detail_param"
)

// traeCNBatchDetailBody 构造客户端同款批量目录请求。functions 只取对话与 Agent
// 两个分组：其它分组（code_reviewer / refactor / inline_chat 等）是插件内部用途，
// 混进来会暴露根本无法对话的 config。
func traeCNBatchDetailBody() []byte {
	payload, _ := json.Marshal(map[string]any{
		"functions":                 []string{"chat_v3", "solo_agent"},
		"agent_type":                "solo_agent",
		"current_config_info":       map[string]any{"config_name": "", "is_custom_model": false},
		"mode_type":                 0,
		"access_type":               0,
		"ab_force_vids":             "",
		"ab_autotest_advanced_mode": 0,
		"show_custom_model":         true,
	})
	return payload
}

// FetchTraeCNModels fetches the logical model IDs exposed by a Trae-compatible
// upstream. The preferred endpoint is the OpenAI-compatible GET /v1/models
// exposed by trae-local-api; direct Trae deployments are supported through
// the desktop get_detail_param endpoint as a fallback. No static model list is
// returned on success: callers can decide whether to cache it or use their
// compatibility catalog when the provider endpoint is unavailable.
func FetchTraeCNModels(ctx context.Context, account *auth.Account, proxyOverride string) ([]string, error) {
	return fetchTraeCNModels(ctx, nil, account, proxyOverride)
}

// FetchTraeCNModelsWithStore is the durable request-path variant. A rotating
// Trae RT is refreshed and persisted before model discovery, and discovery
// requests use the same Resin/proxy egress selected for inference.
func FetchTraeCNModelsWithStore(ctx context.Context, store *auth.Store, account *auth.Account, proxyOverride string) ([]string, error) {
	return fetchTraeCNModels(ctx, store, account, proxyOverride)
}

func fetchTraeCNModels(ctx context.Context, store *auth.Store, account *auth.Account, proxyOverride string) ([]string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if account == nil || !account.IsTraeCNAPI() {
		return nil, fmt.Errorf("traecn account is unavailable")
	}
	if proxyOverride = strings.TrimSpace(proxyOverride); proxyOverride == "" {
		account.Mu().RLock()
		proxyOverride = strings.TrimSpace(account.ProxyURL)
		account.Mu().RUnlock()
	}
	var err error
	if store != nil {
		err = store.EnsureTraeCNAccountWithProxy(ctx, account, proxyOverride)
	} else {
		err = account.EnsureTraeCNAccessToken(ctx, proxyOverride, false)
	}
	if err != nil {
		return nil, fmt.Errorf("refresh Trae CN token: %w", err)
	}
	host, accessToken := account.TraeCNCredentials()
	if host == "" || accessToken == "" {
		return nil, fmt.Errorf("traecn credentials are incomplete")
	}

	viaResin := IsResinEnabledForContext(ctx) && account.ID() > 0
	resinPlatform := ResinPlatformFromContext(ctx)
	if viaResin && resinPlatform == "" {
		resinPlatform = ResinPlatformForSessionFromContext(ctx, "")
	}
	client := getPooledClient(account, proxyOverride)
	buildEndpoint := func(path string) string {
		endpoint := strings.TrimRight(host, "/") + path
		if viaResin {
			return BuildReverseProxyURLForContext(ctx, endpoint, resinPlatform)
		}
		return endpoint
	}
	doRequest := func(method, path string, body []byte, providerAuth bool) ([]byte, int, error) {
		requestCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		endpoint := buildEndpoint(path)
		req, requestErr := http.NewRequestWithContext(requestCtx, method, endpoint, bytes.NewReader(body))
		if requestErr != nil {
			return nil, 0, requestErr
		}
		var headers http.Header
		if providerAuth {
			headers = auth.TraeCNRequestHeaders(account, accessToken, uuid.NewString())
			headers.Set("Referer", endpoint)
		} else {
			headers = auth.TraeCNRequestHeaders(account, accessToken, uuid.NewString())
			headers.Set("Referer", endpoint)
			apiKey := strings.TrimSpace(accessToken)
			account.Mu().RLock()
			if strings.TrimSpace(account.APIKey) != "" {
				apiKey = strings.TrimSpace(account.APIKey)
			}
			account.Mu().RUnlock()
			headers.Set("Authorization", "Bearer "+apiKey)
			headers.Set("X-API-Key", apiKey)
		}
		headers.Set("Accept", "application/json")
		if method == http.MethodPost {
			headers.Set("Content-Type", "application/json")
		}
		if viaResin {
			headers.Set("X-Resin-Account", ResinAccountID(account))
		}
		req.Header = headers
		resp, requestErr := func() (*http.Response, error) {
			if viaResin {
				return getResinHTTPClient(account).Do(req)
			}
			return client.Do(req)
		}()
		if requestErr != nil {
			return nil, 0, requestErr
		}
		defer resp.Body.Close()
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if readErr != nil {
			return nil, resp.StatusCode, readErr
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			message := strings.TrimSpace(string(raw))
			if len(message) > 512 {
				message = message[:512]
			}
			return raw, resp.StatusCode, fmt.Errorf("HTTP %d %s", resp.StatusCode, message)
		}
		return raw, resp.StatusCode, nil
	}

	var failures []string
	// 批量目录接口优先：客户端就是用它渲染模型选择器的，单数接口与兼容面
	// （/v1/models）给的目录都可能落后，新上线的模型只有这里能看到。
	for _, providerAuth := range []bool{true, false} {
		raw, _, requestErr := doRequest(http.MethodPost, traeCNModelsBatchDetailPath, traeCNBatchDetailBody(), providerAuth)
		if requestErr == nil {
			if models := extractTraeCNModelIDs(raw); len(models) > 0 {
				return models, nil
			}
			failures = append(failures, "POST /api/ide/v1/batch_get_detail_param returned no model IDs")
		} else {
			failures = append(failures, fmt.Sprintf("POST /api/ide/v1/batch_get_detail_param: %v", requestErr))
		}
	}

	// First try the OpenAI-compatible surface. It is the canonical endpoint
	// exposed by the integrated trae-local-api project.
	for _, providerAuth := range []bool{false, true} {
		raw, _, requestErr := doRequest(http.MethodGet, traeCNModelsPath, nil, providerAuth)
		if requestErr == nil {
			if models := extractTraeCNModelIDs(raw); len(models) > 0 {
				return models, nil
			}
			failures = append(failures, "GET /v1/models returned no model IDs")
		} else {
			failures = append(failures, fmt.Sprintf("GET /v1/models: %v", requestErr))
		}
	}

	// trae-local-api also exposes the direct Trae detail response through a
	// normalized GET endpoint. Prefer it before contacting a raw Trae host so
	// the integration follows the target project's public API contract.
	for _, providerAuth := range []bool{false, true} {
		raw, _, requestErr := doRequest(http.MethodGet, traeCNModelsDetailCompatPath, nil, providerAuth)
		if requestErr == nil {
			if models := extractTraeCNModelIDs(raw); len(models) > 0 {
				return models, nil
			}
			failures = append(failures, "GET /v1/models/detail returned no model IDs")
		} else {
			failures = append(failures, fmt.Sprintf("GET /v1/models/detail: %v", requestErr))
		}
	}

	// Direct Trae hosts expose model capabilities through get_detail_param.
	detailBody, _ := json.Marshal(map[string]any{
		"function":            "chat_v3",
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	})
	raw, _, requestErr := doRequest(http.MethodPost, traeCNModelsDetailPath, detailBody, true)
	if requestErr == nil {
		if models := extractTraeCNModelIDs(raw); len(models) > 0 {
			return models, nil
		}
		failures = append(failures, "POST /api/ide/v1/get_detail_param returned no model IDs")
	} else {
		failures = append(failures, fmt.Sprintf("POST /api/ide/v1/get_detail_param: %v", requestErr))
	}
	return nil, fmt.Errorf("Trae CN model catalog unavailable (%s)", strings.Join(failures, "; "))
}

func traeCNDetailConfigIsInternal(configName, usage string) bool {
	name := strings.ToLower(strings.TrimSpace(configName))
	if name == "" {
		return true
	}
	switch name {
	case "summary", "fast_apply", "fast_apply_new", "title_generation", "input_optimization":
		return true
	}
	// Advisor configs and utility configs can share a model_name with a real
	// chat config, but are not selectable models themselves.
	if strings.Contains(name, "advisor") || strings.HasPrefix(name, "fast_") || strings.HasPrefix(name, "title_") || strings.HasPrefix(name, "input_") {
		return true
	}
	if usage = strings.ToLower(strings.TrimSpace(usage)); usage != "" && usage != "chat_completion" && usage != "custom_model" && usage != "chat" {
		return true
	}
	return false
}

// traeCNPublicIDsForConfig converts a config_name from get_detail_param into
// IDs that clients can send to this gateway.  There is no built-in alias table
// any more: a provider config is exposed under its own name, byte for byte.
// Trae picks the backend by config_name and it is case-sensitive
// (deepseek-v4-pro is rejected with 4001 while DeepSeek-V4-Pro works), so any
// renaming here would break routing.  Friendly names come from the
// administrator's TRAECN model mapping.
func traeCNPublicIDsForConfig(configName string) []string {
	configName = strings.TrimSpace(configName)
	if configName == "" {
		return nil
	}
	// Generic custom_model_* entries are implementation placeholders; leaking
	// them makes them appear routable while their provider identity is
	// account-specific. Map them explicitly if a deployment needs them.
	if strings.HasPrefix(strings.ToLower(configName), "custom_model_") {
		return nil
	}
	return []string{configName}
}

// traeCNResolveConfigName 把请求里的模型名校正成账号目录里的上游 config_name。
// provider 名字区分大小写（DeepSeek-V4-Pro / Doubao_1_6），而客户端和旧配置里常见
// 归一化写法（deepseek-v4-pro / doubao-1-6），用宽松匹配找出真名后原样返回；
// 目录里没有这个名字时返回空串。
func traeCNResolveConfigName(model string, known []string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	for _, candidate := range known {
		if strings.TrimSpace(candidate) == model {
			return candidate
		}
	}
	for _, candidate := range known {
		if auth.TraeCNModelsEquivalent(candidate, model) {
			return strings.TrimSpace(candidate)
		}
	}
	return ""
}

// traeCNFunctionConfigInfoListRaw 把 batch_get_detail_param 的
// function_configs[].config_info_list 合并成一个 JSON 数组；没有该结构时返回空串。
func traeCNFunctionConfigInfoListRaw(root gjson.Result) string {
	functions := root.Get("function_configs")
	if !functions.IsArray() {
		return ""
	}
	var b strings.Builder
	b.WriteByte('[')
	first := true
	functions.ForEach(func(_, entry gjson.Result) bool {
		if !entry.IsObject() {
			return true
		}
		list := entry.Get("config_info_list")
		if !list.IsArray() {
			list = entry.Get("configInfoList")
		}
		if !list.IsArray() {
			return true
		}
		list.ForEach(func(_, item gjson.Result) bool {
			if !first {
				b.WriteByte(',')
			}
			first = false
			b.WriteString(item.Raw)
			return true
		})
		return true
	})
	if first {
		return ""
	}
	b.WriteByte(']')
	return b.String()
}

func traeCNConfigInfoList(root gjson.Result) gjson.Result {
	if !root.Exists() || root.Type == gjson.Null {
		return gjson.Result{}
	}
	if root.IsObject() {
		for _, key := range []string{"config_info_list", "configInfoList"} {
			if child := root.Get(key); child.Exists() && child.IsArray() {
				return child
			}
		}
		// Different desktop builds wrap the detail payload under one of these
		// envelopes. Keep the search bounded to avoid walking encrypted metadata.
		for _, key := range []string{"data", "response", "result", "payload"} {
			if child := root.Get(key); child.Exists() {
				if found := traeCNConfigInfoList(child); found.Exists() {
					return found
				}
			}
		}
	}
	return gjson.Result{}
}

// extractTraeCNModelIDs accepts both OpenAI /v1/models payloads and the
// several get_detail_param envelopes observed across Trae desktop versions.
func extractTraeCNModelIDs(body []byte) []string {
	if len(bytes.TrimSpace(body)) == 0 || !gjson.ValidBytes(body) {
		return nil
	}
	root := gjson.ParseBytes(body)
	// batch 响应把目录按 function 分组（function_configs[].config_info_list）。
	// 合并成一个扁平 config 列表后复用下面的过滤规则，避免两套解析分叉。
	if merged := traeCNFunctionConfigInfoListRaw(root); merged != "" {
		root = gjson.Parse(`{"config_info_list":` + merged + `}`)
	}
	seen := make(map[string]struct{})
	result := make([]string, 0)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || strings.ContainsAny(value, "\r\n\t") {
			return
		}
		lower := strings.ToLower(value)
		switch lower {
		case "chat_v3", "inline_chat", "builder_v3", "solo_coder", "system_diagnosis":
			return
		}
		if _, ok := seen[lower]; ok {
			return
		}
		seen[lower] = struct{}{}
		result = append(result, value)
	}

	// The direct Trae endpoint is authoritative when this field is present.
	// Parse it structurally so utility configs and encrypted model_name fields
	// cannot accidentally become advertised models.
	if configList := traeCNConfigInfoList(root); configList.Exists() {
		configList.ForEach(func(_, entry gjson.Result) bool {
			if !entry.IsObject() {
				return true
			}
			configName := strings.TrimSpace(entry.Get("config_name").String())
			if configName == "" {
				configName = strings.TrimSpace(entry.Get("configName").String())
			}
			usage := entry.Get("usage").String()
			if traeCNDetailConfigIsInternal(configName, usage) {
				return true
			}
			if switchValue := entry.Get("config_switch"); switchValue.Exists() && switchValue.Type != gjson.Null && !switchValue.Bool() {
				return true
			}
			if switchValue := entry.Get("configSwitch"); switchValue.Exists() && switchValue.Type != gjson.Null && !switchValue.Bool() {
				return true
			}
			for _, publicID := range traeCNPublicIDsForConfig(configName) {
				add(publicID)
			}
			return true
		})
		if len(result) > 0 {
			add("auto")
			sort.Strings(result)
			return result
		}
		// A valid but empty detail list is authoritative; do not infer model IDs
		// from unrelated metadata in that response.
		return nil
	}

	var walk func(gjson.Result, string)
	walk = func(value gjson.Result, keyHint string) {
		if !value.Exists() || value.Type == gjson.Null {
			return
		}
		if value.Type == gjson.String {
			if keyHint == "id" || keyHint == "model" || keyHint == "model_id" || keyHint == "modelid" || keyHint == "slug" || keyHint == "name" {
				add(value.String())
			}
			return
		}
		if value.IsArray() {
			value.ForEach(func(_, item gjson.Result) bool {
				if item.IsObject() {
					for _, key := range []string{"id", "model", "model_id", "modelId", "slug", "name"} {
						walk(item.Get(key), strings.ToLower(key))
					}
					// Some model endpoints use an object keyed by the model ID.
					item.ForEach(func(k, v gjson.Result) bool {
						if k.String() != "" && (v.IsObject() || v.IsArray()) {
							add(k.String())
						}
						return true
					})
				} else {
					walk(item, keyHint)
				}
				return true
			})
			return
		}
		if value.IsObject() {
			for _, key := range []string{"data", "models", "model_list", "modelList", "config_infos", "configInfos", "configs", "model_ids", "modelIds"} {
				if child := value.Get(key); child.Exists() {
					walk(child, strings.ToLower(key))
				}
			}
			value.ForEach(func(k, v gjson.Result) bool {
				key := strings.ToLower(strings.TrimSpace(k.String()))
				if key == "id" || key == "model" || key == "model_id" || key == "modelid" || key == "slug" || key == "name" {
					walk(v, key)
				}
				return true
			})
		}
	}
	walk(root, "")
	if len(result) > 0 {
		add("auto")
	}
	sort.Strings(result)
	return result
}

type traeCNOutputKind uint8

const (
	traeCNOutputReasoning traeCNOutputKind = iota + 1
	traeCNOutputMessage
	traeCNOutputTool
)

type traeCNOutputRef struct {
	kind traeCNOutputKind
	tool *traeCNToolCall
}

type traeCNToolCall struct {
	providerID   string
	id           string
	callID       string
	name         string
	arguments    strings.Builder
	outputIndex  int
	emittedBytes int
	itemAdded    bool
	custom       bool
	// bridge 非空表示该调用的参数要还原成托管 item（local_shell_call / shell_call）
	// 而不是原生 function_call。
	bridge string
}

type traeCNCanonicalState struct {
	responseID   string
	model        string
	terminal     bool
	finishReason string
	usage        map[string]any
	output       []traeCNOutputRef
	messageID    string
	messageIdx   int
	text         strings.Builder
	reasoningID  string
	reasoningIdx int
	reasoning    strings.Builder
	tools        []*traeCNToolCall
	toolByKey    map[string]*traeCNToolCall
	// bridges 是「Trae 函数名 -> 客户端能执行的 item 类型」：上游只能回 function
	// 调用，这些名字的输出项要还原成 custom_tool_call / local_shell_call / shell_call。
	bridges traeCNBridges
	// contracts 记录每个函数的声明契约（顶层必填字段），用于修复模型多包一层的参数。
	contracts traeCNContracts
}

func newTraeCNCanonicalState(model string) *traeCNCanonicalState {
	return &traeCNCanonicalState{
		responseID: "resp_" + uuid.NewString(),
		model:      model,
		usage:      map[string]any{"input_tokens": int64(0), "output_tokens": int64(0), "total_tokens": int64(0)},
		toolByKey:  make(map[string]*traeCNToolCall),
		messageIdx: -1, reasoningIdx: -1,
	}
}

// newTraeCNCanonicalStateWithBridges 带上「函数名 -> item 类型」的还原表，用于把
// 上游的 function 调用还原成客户端能执行的 custom_tool_call / local_shell_call 等。
func newTraeCNCanonicalStateWithBridges(model string, bridges traeCNBridges, contracts traeCNContracts) *traeCNCanonicalState {
	state := newTraeCNCanonicalState(model)
	if len(bridges) > 0 {
		state.bridges = bridges
	}
	if len(contracts) > 0 {
		state.contracts = contracts
	}
	return state
}

func writeTraeCanonicalEvent(writer io.Writer, payload []byte) error {
	if len(payload) == 0 {
		return nil
	}
	_, err := writer.Write(append(append([]byte("data: "), payload...), '\n', '\n'))
	return err
}

func marshalTraeCanonicalEvent(eventType string, fields map[string]any) []byte {
	if fields == nil {
		fields = make(map[string]any)
	}
	fields["type"] = eventType
	payload, _ := json.Marshal(fields)
	return payload
}

func (s *traeCNCanonicalState) emitCreated(writer io.Writer) error {
	return writeTraeCanonicalEvent(writer, marshalTraeCanonicalEvent("response.created", map[string]any{
		"response": map[string]any{"id": s.responseID, "object": "response", "status": "in_progress", "model": s.model},
	}))
}

func (s *traeCNCanonicalState) ensureMessage(writer io.Writer) error {
	if s.messageID != "" {
		return nil
	}
	s.messageID = "msg_" + uuid.NewString()
	s.messageIdx = len(s.output)
	s.output = append(s.output, traeCNOutputRef{kind: traeCNOutputMessage})
	return writeTraeCanonicalEvent(writer, marshalTraeCanonicalEvent("response.output_item.added", map[string]any{
		"response_id": s.responseID, "output_index": s.messageIdx,
		"item": map[string]any{"id": s.messageID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}},
	}))
}

func (s *traeCNCanonicalState) ensureReasoning(writer io.Writer) error {
	if s.reasoningID != "" {
		return nil
	}
	s.reasoningID = "rs_" + uuid.NewString()
	s.reasoningIdx = len(s.output)
	s.output = append(s.output, traeCNOutputRef{kind: traeCNOutputReasoning})
	return writeTraeCanonicalEvent(writer, marshalTraeCanonicalEvent("response.output_item.added", map[string]any{
		"response_id": s.responseID, "output_index": s.reasoningIdx,
		"item": map[string]any{"id": s.reasoningID, "type": "reasoning", "status": "in_progress", "summary": []any{}},
	}))
}

func (s *traeCNCanonicalState) emitText(writer io.Writer, text string) error {
	if text == "" || strings.HasPrefix(text, "Building prompt:") || strings.HasPrefix(text, "Completed building prompt") {
		return nil
	}
	if err := s.ensureMessage(writer); err != nil {
		return err
	}
	s.text.WriteString(text)
	return writeTraeCanonicalEvent(writer, marshalTraeCanonicalEvent("response.output_text.delta", map[string]any{
		"response_id": s.responseID, "item_id": s.messageID, "output_index": s.messageIdx, "content_index": 0, "delta": text,
	}))
}

func (s *traeCNCanonicalState) emitReasoning(writer io.Writer, text string) error {
	if text == "" {
		return nil
	}
	// TRAECN_REASONING_DISABLED=1 时不把推理摘要发给客户端：Trae 模型思维链很长，
	// 客户端会一直显示"思考中"；关掉后客户端只看到正文与工具调用。
	if traeCNReasoningDisabled() {
		return nil
	}
	if err := s.ensureReasoning(writer); err != nil {
		return err
	}
	s.reasoning.WriteString(text)
	return writeTraeCanonicalEvent(writer, marshalTraeCanonicalEvent("response.reasoning_summary_text.delta", map[string]any{
		"response_id": s.responseID, "item_id": s.reasoningID, "output_index": s.reasoningIdx, "summary_index": 0, "delta": text,
	}))
}

// traeCNReasoningDisabled 判断是否关闭推理摘要下发（TRAECN_REASONING_DISABLED=1）。
func traeCNReasoningDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TRAECN_REASONING_DISABLED"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func traeToolCalls(root gjson.Result, eventName string) []gjson.Result {
	for _, path := range []string{"tool_calls", "message.tool_calls", "choices.0.delta.tool_calls", "choices.0.message.tool_calls"} {
		value := root.Get(path)
		if value.IsArray() {
			return value.Array()
		}
		if value.IsObject() {
			return []gjson.Result{value}
		}
	}
	typ := strings.TrimSpace(root.Get("type").String())
	if typ == "" {
		typ = eventName
	}
	switch strings.ToLower(typ) {
	case "tool_call", "function_call", "custom_tool_call":
		return []gjson.Result{root}
	}
	return nil
}

func traeToolField(root gjson.Result, paths ...string) gjson.Result {
	for _, path := range paths {
		if value := root.Get(path); value.Exists() && value.Type != gjson.Null {
			return value
		}
	}
	return gjson.Result{}
}

func (s *traeCNCanonicalState) mergeToolCall(writer io.Writer, raw gjson.Result, position int) error {
	if !raw.IsObject() {
		return nil
	}
	id := strings.TrimSpace(traeFirstText(raw, "id", "call_id", "tool_call_id"))
	callID := strings.TrimSpace(traeFirstText(raw, "call_id", "id", "tool_call_id"))
	// 原生工具调用使用 function_call，兼容层可能使用 function；两者也可能编码成 JSON 字符串。
	function := traeToolField(raw, "function_call", "function")
	if function.Type == gjson.String {
		function = gjson.Parse(function.String())
	}
	name := strings.TrimSpace(traeFirstText(function, "name"))
	if name == "" {
		name = strings.TrimSpace(traeFirstText(raw, "name", "tool_name"))
	}
	argument := traeArgumentString(traeToolField(function, "arguments", "input"))
	if argument == "" {
		argument = traeArgumentString(traeToolField(raw, "arguments", "input", "params", "arguments_delta"))
	}
	// 空数组占位和只有 index 的心跳不构成工具调用，避免创建无名称的输出项。
	if id == "" && name == "" && argument == "" {
		return nil
	}
	index := position
	if raw.Get("index").Exists() {
		index = int(raw.Get("index").Int())
	}
	key := id
	indexKey := "index:" + strconv.Itoa(index)
	if key == "" {
		key = indexKey
	}
	tool := s.toolByKey[key]
	if tool == nil && key != indexKey {
		candidate := s.toolByKey[indexKey]
		// 数组位置会在下一批调用中复用；不同的上游 ID 不能合并成同一调用。
		if candidate != nil && (candidate.providerID == "" || candidate.providerID == id) {
			tool = candidate
		}
	}
	if tool == nil {
		providerID := id
		if id == "" {
			id = "call_" + uuid.NewString()
		}
		if callID == "" {
			callID = id
		}
		tool = &traeCNToolCall{providerID: providerID, id: id, callID: callID, name: name, outputIndex: len(s.output), custom: strings.EqualFold(raw.Get("type").String(), "custom_tool_call")}
		s.toolByKey[key] = tool
		s.toolByKey[indexKey] = tool
		s.tools = append(s.tools, tool)
		s.output = append(s.output, traeCNOutputRef{kind: traeCNOutputTool, tool: tool})
	} else {
		// 名称、ID 和参数可能分批到达；发出输出项后保持下游调用标识稳定。
		s.toolByKey[indexKey] = tool
		if id != "" {
			s.toolByKey[id] = tool
			tool.providerID = id
			if !tool.itemAdded {
				tool.id = id
				tool.callID = callID
			}
		}
	}
	if tool.name == "" && name != "" {
		tool.name = name
	}
	if bridge, declared := s.bridges[tool.name]; declared {
		tool.bridge = bridge
		tool.custom = bridge == traeCNBridgeCustomTool
	}
	if argument != "" {
		current := tool.arguments.String()
		switch {
		case current == "":
			tool.arguments.WriteString(argument)
		case strings.HasPrefix(argument, current):
			tool.arguments.WriteString(argument[len(current):])
		default:
			// A few models stream argument fragments without an explicit delta
			// marker. Preserve those fragments; terminal JSON validation fences a
			// genuinely divergent provider response.
			tool.arguments.WriteString(argument)
		}
	}
	return s.emitToolProgress(writer, tool)
}

func (s *traeCNCanonicalState) emitToolProgress(writer io.Writer, tool *traeCNToolCall) error {
	if tool == nil || tool.name == "" {
		return nil
	}
	if tool.custom || tool.bridge != "" {
		// 降级工具的载荷要等完整后才能脱壳/还原成客户端契约的 item，不能按增量
		// 下发：输出项统一在终态补齐。
		return nil
	}
	if !tool.itemAdded {
		if err := writeTraeCanonicalEvent(writer, marshalTraeCanonicalEvent("response.output_item.added", map[string]any{
			"response_id": s.responseID, "output_index": tool.outputIndex,
			"item": s.toolItem(tool, "function_call", "", "in_progress"),
		})); err != nil {
			return err
		}
		tool.itemAdded = true
	}
	arguments := tool.arguments.String()
	if tool.emittedBytes >= len(arguments) {
		return nil
	}
	delta := arguments[tool.emittedBytes:]
	tool.emittedBytes = len(arguments)
	eventType := "response.function_call_arguments.delta"
	if tool.custom {
		eventType = "response.custom_tool_call_input.delta"
	}
	return writeTraeCanonicalEvent(writer, marshalTraeCanonicalEvent(eventType, map[string]any{
		"response_id": s.responseID, "output_index": tool.outputIndex, "item_id": tool.id, "call_id": tool.callID, "delta": delta,
	}))
}

func traeUsageInt(root gjson.Result, paths ...string) (int64, bool) {
	for _, path := range paths {
		if value := root.Get(path); value.Exists() && value.Type != gjson.Null {
			return value.Int(), true
		}
	}
	return 0, false
}

func (s *traeCNCanonicalState) updateUsage(root gjson.Result) {
	if input, ok := traeUsageInt(root,
		"prompt_tokens", "input_tokens",
		"usage.prompt_tokens", "usage.input_tokens",
		"token_usage.prompt_tokens", "token_usage.input_tokens",
		"token_usage.data.prompt_tokens", "token_usage.data.input_tokens",
		"data.prompt_tokens", "data.input_tokens",
	); ok {
		s.usage["input_tokens"] = input
	}
	if output, ok := traeUsageInt(root,
		"completion_tokens", "output_tokens",
		"usage.completion_tokens", "usage.output_tokens",
		"token_usage.completion_tokens", "token_usage.output_tokens",
		"token_usage.data.completion_tokens", "token_usage.data.output_tokens",
		"data.completion_tokens", "data.output_tokens",
	); ok {
		s.usage["output_tokens"] = output
	}
	if total, ok := traeUsageInt(root,
		"total_tokens", "usage.total_tokens", "token_usage.total_tokens",
		"token_usage.data.total_tokens", "data.total_tokens",
	); ok {
		s.usage["total_tokens"] = total
	} else {
		s.usage["total_tokens"] = s.usage["input_tokens"].(int64) + s.usage["output_tokens"].(int64)
	}
	inputDetails := make(map[string]any)
	if cached, ok := traeUsageInt(root,
		"cached_tokens", "cache_read_input_tokens",
		"prompt_tokens_details.cached_tokens", "input_tokens_details.cached_tokens",
		"usage.cached_tokens", "usage.cache_read_input_tokens",
		"usage.prompt_tokens_details.cached_tokens", "usage.input_tokens_details.cached_tokens",
		"token_usage.cached_tokens", "token_usage.cache_read_input_tokens",
		"token_usage.data.cached_tokens", "token_usage.data.cache_read_input_tokens",
		"data.cached_tokens", "data.cache_read_input_tokens",
	); ok {
		// Trae's prompt_tokens already includes cache reads (as confirmed by its
		// total_tokens field), so this is a billing detail rather than an amount
		// to add to input_tokens.
		inputDetails["cached_tokens"] = cached
	}
	if created, ok := traeUsageInt(root,
		"cache_creation_input_tokens", "usage.cache_creation_input_tokens",
		"token_usage.cache_creation_input_tokens", "token_usage.data.cache_creation_input_tokens",
		"data.cache_creation_input_tokens",
	); ok {
		// OpenAI has no first-class cache-write field. Keep it as an additive
		// detail for observability; billing continues to count it at the normal
		// input rate because it is already included in input_tokens.
		inputDetails["cache_creation_tokens"] = created
	}
	if len(inputDetails) > 0 {
		s.usage["input_tokens_details"] = inputDetails
	}
	if reasoning, ok := traeUsageInt(root,
		"reasoning_tokens", "completion_tokens_details.reasoning_tokens", "output_tokens_details.reasoning_tokens",
		"usage.reasoning_tokens", "usage.completion_tokens_details.reasoning_tokens", "usage.output_tokens_details.reasoning_tokens",
		"token_usage.reasoning_tokens", "token_usage.data.reasoning_tokens", "data.reasoning_tokens",
	); ok {
		s.usage["output_tokens_details"] = map[string]any{"reasoning_tokens": reasoning}
	}
}

func traeIncompleteReason(finishReason string) string {
	switch strings.ToLower(strings.TrimSpace(finishReason)) {
	case "length", "max_tokens", "max_output_tokens", "token_limit":
		return "max_output_tokens"
	case "content_filter":
		return "content_filter"
	default:
		return ""
	}
}

func (s *traeCNCanonicalState) terminalOutput(writer io.Writer, status string) ([]any, error) {
	output := make([]any, 0, len(s.output))
	for _, ref := range s.output {
		switch ref.kind {
		case traeCNOutputReasoning:
			if s.reasoning.Len() == 0 {
				continue
			}
			item := map[string]any{"id": s.reasoningID, "type": "reasoning", "status": status, "summary": []any{map[string]any{"type": "summary_text", "text": s.reasoning.String()}}}
			output = append(output, item)
			if status == "completed" {
				if err := writeTraeCanonicalEvent(writer, marshalTraeCanonicalEvent("response.reasoning_summary_text.done", map[string]any{"response_id": s.responseID, "item_id": s.reasoningID, "output_index": s.reasoningIdx, "summary_index": 0, "text": s.reasoning.String()})); err != nil {
					return nil, err
				}
				if err := writeTraeCanonicalEvent(writer, marshalTraeCanonicalEvent("response.output_item.done", map[string]any{"response_id": s.responseID, "output_index": s.reasoningIdx, "item": item})); err != nil {
					return nil, err
				}
			}
		case traeCNOutputMessage:
			if s.text.Len() == 0 {
				continue
			}
			item := map[string]any{"id": s.messageID, "type": "message", "role": "assistant", "status": status, "content": []any{map[string]any{"type": "output_text", "text": s.text.String(), "annotations": []any{}}}}
			output = append(output, item)
			if status == "completed" {
				if err := writeTraeCanonicalEvent(writer, marshalTraeCanonicalEvent("response.output_text.done", map[string]any{"response_id": s.responseID, "item_id": s.messageID, "output_index": s.messageIdx, "content_index": 0, "text": s.text.String()})); err != nil {
					return nil, err
				}
				if err := writeTraeCanonicalEvent(writer, marshalTraeCanonicalEvent("response.output_item.done", map[string]any{"response_id": s.responseID, "output_index": s.messageIdx, "item": item})); err != nil {
					return nil, err
				}
			}
		case traeCNOutputTool:
			tool := ref.tool
			if tool == nil {
				continue
			}
			if tool.name == "" {
				if status == "incomplete" {
					continue
				}
				return nil, fmt.Errorf("Trae CN returned a tool call without a name")
			}
			if tool.arguments.Len() == 0 {
				tool.arguments.WriteString("{}")
			}
			if status == "completed" && !tool.custom && !json.Valid([]byte(tool.arguments.String())) {
				return nil, fmt.Errorf("Trae CN returned invalid JSON arguments for tool %q", tool.name)
			}
			if tool.bridge == traeCNBridgeToolSearch {
				item, err := s.emitToolSearch(writer, tool, status)
				if err != nil {
					return nil, err
				}
				output = append(output, item)
				continue
			}
			if tool.bridge == traeCNBridgeLocalShell || tool.bridge == traeCNBridgeShellCall {
				// 托管 shell：还原成 local_shell_call / shell_call，客户端才会执行。
				item, err := s.emitBridgedHostedTool(writer, tool, status)
				if err != nil {
					return nil, err
				}
				output = append(output, item)
				continue
			}
			if err := s.emitToolProgress(writer, tool); err != nil {
				return nil, err
			}
			itemType := "function_call"
			doneEvent := "response.function_call_arguments.done"
			// 终态里给出修正后的参数：模型若把参数多包了一层（{"args":{...}}），
			// 客户端只会看到解开的版本，done/item/completed 三处一致。
			arguments := traeCNUnwrapArguments(tool.arguments.String(), s.contracts[tool.name])
			if tool.custom {
				itemType = "custom_tool_call"
				doneEvent = "response.custom_tool_call_input.done"
				// 降级后的 function 参数是 {"input": "..."}；客户端要的是原始文本。
				arguments = traeCNCustomToolInput(arguments)
			}
			// 校验放在任何事件写出之前：空载荷必须以整轮失败结束，不能先把半截
			// 调用发给客户端再去失败。
			if status == "completed" {
				if err := s.emptyToolPayloadError(tool, arguments); err != nil {
					return nil, err
				}
			}
			if tool.custom {
				// added 事件在终态补齐（降级工具的载荷要完整后才能脱壳）。
				if err := writeTraeCanonicalEvent(writer, marshalTraeCanonicalEvent("response.output_item.added", map[string]any{
					"response_id": s.responseID, "output_index": tool.outputIndex,
					"item": s.toolItem(tool, itemType, "", "in_progress"),
				})); err != nil {
					return nil, err
				}
				if err := writeTraeCanonicalEvent(writer, marshalTraeCanonicalEvent("response.custom_tool_call_input.delta", map[string]any{
					"response_id": s.responseID, "output_index": tool.outputIndex, "item_id": tool.id, "call_id": tool.callID, "delta": arguments,
				})); err != nil {
					return nil, err
				}
			}
			item := s.toolItem(tool, itemType, arguments, status)
			output = append(output, item)
			if status == "completed" {
				if err := writeTraeCanonicalEvent(writer, marshalTraeCanonicalEvent(doneEvent, map[string]any{"response_id": s.responseID, "output_index": tool.outputIndex, "item_id": tool.id, "call_id": tool.callID, "arguments": arguments, "input": arguments})); err != nil {
					return nil, err
				}
				if err := writeTraeCanonicalEvent(writer, marshalTraeCanonicalEvent("response.output_item.done", map[string]any{"response_id": s.responseID, "output_index": tool.outputIndex, "item": item})); err != nil {
					return nil, err
				}
			}
		}
	}
	return output, nil
}

// emitBridgedHostedTool 把降级过的托管 shell 调用还原成客户端能执行的 item：
// 输出项事件（added + done）都按 item 契约发出，action 由模型给的 {"command": ...} 还原。
func (s *traeCNCanonicalState) emitBridgedHostedTool(writer io.Writer, tool *traeCNToolCall, status string) (map[string]any, error) {
	action := traeCNShellAction(tool.arguments.String(), tool.bridge)
	if err := writeTraeCanonicalEvent(writer, marshalTraeCanonicalEvent("response.output_item.added", map[string]any{
		"response_id": s.responseID, "output_index": tool.outputIndex,
		"item": map[string]any{"id": tool.id, "type": tool.bridge, "call_id": tool.callID, "status": "in_progress", "action": action},
	})); err != nil {
		return nil, err
	}
	item := map[string]any{"id": tool.id, "type": tool.bridge, "call_id": tool.callID, "status": status, "action": action}
	if status == "completed" {
		if err := writeTraeCanonicalEvent(writer, marshalTraeCanonicalEvent("response.output_item.done", map[string]any{
			"response_id": s.responseID, "output_index": tool.outputIndex, "item": item,
		})); err != nil {
			return nil, err
		}
	}
	return item, nil
}

func (s *traeCNCanonicalState) emitCompleted(writer io.Writer, finishReason string) error {
	if s.terminal {
		return nil
	}
	if finishReason == "" {
		finishReason = traeCNFirstNonEmpty(s.finishReason, "stop")
	}
	s.finishReason = finishReason
	if (finishReason == "tool_calls" || finishReason == "function_call") && len(s.tools) == 0 {
		return s.emitFailure(writer, "missing_tool_calls", "Trae CN ended with a tool-call finish reason but returned no tool calls")
	}
	incompleteReason := traeIncompleteReason(finishReason)
	status := "completed"
	eventType := "response.completed"
	if incompleteReason != "" {
		status = "incomplete"
		eventType = "response.incomplete"
	}
	// 思考摘要不构成一次交付；空结果报 completed 会让 Codex 安静结束本轮。
	// 长度截断和内容过滤仍保留原 incomplete 原因，工具调用由终态转换校验。
	if status == "completed" && strings.TrimSpace(s.text.String()) == "" && len(s.tools) == 0 {
		return s.emitFailure(writer, "upstream_empty_output", "Trae CN ended before returning an answer or tool call")
	}
	output, err := s.terminalOutput(writer, status)
	if err != nil {
		return s.emitFailure(writer, "malformed_tool_call", err.Error())
	}
	response := map[string]any{"id": s.responseID, "object": "response", "status": status, "model": s.model, "output": output, "usage": s.usage}
	if incompleteReason != "" {
		response["incomplete_details"] = map[string]any{"reason": incompleteReason}
	}
	if finishReason != "" {
		response["stop_reason"] = finishReason
	}
	s.logTerminal(eventType, incompleteReason)
	s.terminal = true
	return writeTraeCanonicalEvent(writer, marshalTraeCanonicalEvent(eventType, map[string]any{"response": response}))
}

// emptyToolPayloadError 判定一次已完成的工具调用有没有"真的带内容"。
//
// 模型没把参数填出来时（特别是被降级成单个 input 字符串的自由文本工具，以及必填
// 参数被留空的普通 function 工具），网关原样交付会让客户端拿着空参数去执行：
// 多智能体模式下就表现为"子智能体被拉起来了但收到空任务"，客户端还会继续重试，
// 用户看到的是模型"不会用多智能体"。这类调用既不可用也不可修，按 malformed_tool_call
// 结束本轮，让客户端重试整轮（重试时可能会补上参数）。
//
// 只判"内容确实为空"的两类，不判"少传了字段"：required 字段缺失的处理留给客户端，
// 避免对宽松 schema 的工具误报（例如客户端允许用默认值补全的字段）。
func (s *traeCNCanonicalState) emptyToolPayloadError(tool *traeCNToolCall, arguments string) error {
	if tool == nil {
		return nil
	}
	if tool.custom || tool.bridge == traeCNBridgeCustomTool {
		switch strings.TrimSpace(arguments) {
		case "", "{}", "null":
			return fmt.Errorf("Trae CN returned an empty payload for freeform tool %q", tool.name)
		}
		return nil
	}
	contract := s.contracts[tool.name]
	if len(contract.Required) == 0 {
		return nil
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal([]byte(strings.TrimSpace(arguments)), &payload) != nil {
		return nil // 非对象形状交给既有校验与客户端处理。
	}
	// 必填字段在、但值是空串：典型是"把 target 填了、message 留空"，客户端会拿着
	// 空参数去执行（多智能体模式下就是空任务）。
	for _, field := range contract.Required {
		raw, ok := payload[field]
		if !ok {
			continue
		}
		var text string
		if json.Unmarshal(raw, &text) == nil && strings.TrimSpace(text) == "" {
			return fmt.Errorf("Trae CN left required field %q of tool %q empty", field, tool.name)
		}
	}
	// 字段全空（含完全没有字段）＝这次调用什么都没带。只判"整体为空"：参数被多包
	// 一层但内容还在的情况保持原样透传，让客户端报它自己的字段错误。
	for _, raw := range payload {
		if rawMessageHasContent(raw) {
			return nil
		}
	}
	return fmt.Errorf("Trae CN returned a call for tool %q without any payload", tool.name)
}

// rawMessageHasContent 判断一个 JSON 字段值是否携带实际内容（空串、null、空对象、
// 空数组，以及递归下去全为空的容器，都视为没有内容）。
func rawMessageHasContent(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return false
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text) != ""
	}
	switch trimmed {
	case "null", "{}", "[]":
		return false
	}
	var nested map[string]json.RawMessage
	if json.Unmarshal(raw, &nested) == nil {
		for _, inner := range nested {
			if rawMessageHasContent(inner) {
				return true
			}
		}
		return false
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) == nil {
		for _, inner := range list {
			if rawMessageHasContent(inner) {
				return true
			}
		}
		return false
	}
	return true
}

func (s *traeCNCanonicalState) emitFailure(writer io.Writer, code, message string, providerErrors ...gjson.Result) error {
	if s.terminal {
		return nil
	}
	s.terminal = true
	s.logTerminal("response.failed", code)
	return writeTraeCanonicalEvent(writer, traeCanonicalFailure(s.responseID, s.model, code, message, s.usage, providerErrors...))
}

// 只记录终态和内容长度，便于定位模型收尾、断流和转换错误，不记录思考或正文。
func (s *traeCNCanonicalState) logTerminal(event, reason string) {
	log.Printf("[TRAECN] stage=terminal model=%q response_id=%q event=%q finish_reason=%q reason=%q declared_tools=%d tool_calls=%d reasoning_chars=%d output_chars=%d",
		s.model, s.responseID, event, responsesIdentityLogValue(s.finishReason), responsesIdentityLogValue(reason), len(s.contracts), len(s.tools), len([]rune(s.reasoning.String())), len([]rune(s.text.String())))
}

func (s *traeCNCanonicalState) consume(writer io.Writer, eventName string, data []byte) error {
	if s.terminal {
		return nil
	}
	if strings.TrimSpace(string(data)) == "[DONE]" {
		return s.emitCompleted(writer, "")
	}
	parsed, payloadEventName := parseTraePayload(data)
	if reason := traeFirstText(parsed, "finish_reason", "stop_reason", "choices.0.finish_reason"); reason != "" {
		s.finishReason = reason
	}
	name := strings.ToLower(strings.TrimSpace(eventName))
	if name == "" || name == "data" {
		name = strings.ToLower(payloadEventName)
	}
	if name == "" && parsed.Get("done").Bool() {
		name = "done"
	}
	if name == "error" || name == "failed" {
		code, message := traeProviderError(parsed)
		return s.emitFailure(writer, code, message, parsed)
	}
	if parsed.IsObject() {
		code, message := traeProviderError(parsed)
		if (parsed.Get("success").Exists() && !parsed.Get("success").Bool()) || (code != "" && code != "0" && message != "" && name != "token_usage") {
			return s.emitFailure(writer, code, message, parsed)
		}
	}
	switch name {
	case "output", "message", "text", "output_text", "delta", "":
		content := traeFirstText(parsed, "response", "content", "text", "message.content", "choices.0.delta.content")
		if err := s.emitText(writer, content); err != nil {
			return err
		}
		reasoning := traeFirstText(parsed, "reasoning_content", "reasoning", "message.reasoning_content", "choices.0.delta.reasoning_content", "choices.0.delta.reasoning")
		if err := s.emitReasoning(writer, reasoning); err != nil {
			return err
		}
		for index, tool := range traeToolCalls(parsed, payloadEventName) {
			if err := s.mergeToolCall(writer, tool, index); err != nil {
				return err
			}
		}
	case "token_usage", "usage":
		s.updateUsage(parsed)
	case "done", "end", "finish", "message_end":
		// 结束事件可能携带最终的工具名称或完整参数，先合并再执行完整性校验。
		for index, tool := range traeToolCalls(parsed, payloadEventName) {
			if err := s.mergeToolCall(writer, tool, index); err != nil {
				return err
			}
		}
		finishReason := traeFirstText(parsed, "finish_reason", "stop_reason", "reason")
		return s.emitCompleted(writer, finishReason)
	case "metadata", "timing_cost", "extra_info", "progress_notice", "queue_begin", "request_wait_in_queue", "queue_end":
		// Provider lifecycle/diagnostic events do not carry model output.
	default:
		// Forward-compatible fallback for a renamed output event.
		content := traeFirstText(parsed, "response", "content", "text", "message.content")
		reasoning := traeFirstText(parsed, "reasoning_content", "reasoning")
		toolEventName := payloadEventName
		if toolEventName == "" {
			toolEventName = name
		}
		calls := traeToolCalls(parsed, toolEventName)
		if content != "" || reasoning != "" || len(calls) > 0 {
			if err := s.emitText(writer, content); err != nil {
				return err
			}
			if err := s.emitReasoning(writer, reasoning); err != nil {
				return err
			}
			for index, tool := range calls {
				if err := s.mergeToolCall(writer, tool, index); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// traeCNCanonicalStream projects the provider's named SSE events onto the
// canonical Responses stream consumed by all three existing HTTP handlers.
func traeCNCanonicalStream(source io.ReadCloser, model string) io.ReadCloser {
	return traeCNCanonicalStreamForTools(source, model, nil, nil)
}

// traeCNCanonicalStreamForTools 额外接收客户端声明为 custom 的工具名，把这些工具的
// 上游 function 调用还原成 custom_tool_call。
// traeCNKeepaliveInterval 是下游保活间隔。Trae 侧模型长思考时上游可能几十秒不发
// 任何帧（我们还会丢弃 progress_notice/metadata 这类生命周期帧），而链路上的反代
// （Cloudflare / New API 等）空闲读超时通常 60~125s，会把连接拦腰掐断——客户端表现
// 就是"正在重新连接 N/5"并整轮重试，任务永远跑不完。SSE 注释行是协议内容，客户端
// 解析器一律忽略，但能刷新每一跳的空闲计时器。
const traeCNKeepaliveInterval = 15 * time.Second

const traeCNKeepaliveComment = ": keepalive\n\n"

// traeCNKeepaliveWriter 串行化"保活注释 + 正常事件"的写入，避免交错。
type traeCNKeepaliveWriter struct {
	mu     sync.Mutex
	target io.Writer
	stop   chan struct{}
	once   sync.Once
}

func startTraeCNKeepalive(target io.Writer) *traeCNKeepaliveWriter {
	writer := &traeCNKeepaliveWriter{target: target, stop: make(chan struct{})}
	if target == nil {
		return writer
	}
	interval := traeCNKeepaliveInterval
	if raw := strings.TrimSpace(os.Getenv("TRAECN_KEEPALIVE_SECONDS")); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds <= 0 {
			return writer
		} else if err == nil {
			interval = time.Duration(seconds) * time.Second
		}
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-writer.stop:
				return
			case <-ticker.C:
				writer.mu.Lock()
				_, err := io.WriteString(writer.target, traeCNKeepaliveComment)
				writer.mu.Unlock()
				if err != nil {
					return
				}
			}
		}
	}()
	return writer
}

func (w *traeCNKeepaliveWriter) Write(p []byte) (int, error) {
	if w == nil {
		return len(p), nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.target.Write(p)
}

func (w *traeCNKeepaliveWriter) Close() error {
	if w == nil {
		return nil
	}
	w.once.Do(func() { close(w.stop) })
	return nil
}

func traeCNCanonicalStreamForTools(source io.ReadCloser, model string, bridges traeCNBridges, contracts traeCNContracts) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		defer source.Close()
		defer writer.Close()
		// 保活写入器包住 pipe writer：长思考期间持续发 SSE 注释，避免反代按空闲
		// 超时掐断连接（客户端会显示"正在重新连接 N/5"并整轮重试）。
		pipeWriter := writer
		keepalive := startTraeCNKeepalive(pipeWriter)
		defer keepalive.Close()
		state := newTraeCNCanonicalStateWithBridges(model, bridges, contracts)
		if err := state.emitCreated(keepalive); err != nil {
			return
		}
		br := bufio.NewReader(source)
		var eventName string
		var data bytes.Buffer
		flush := func() error {
			if data.Len() == 0 {
				eventName = ""
				return nil
			}
			payload := bytes.Clone(bytes.TrimSpace(data.Bytes()))
			data.Reset()
			name := eventName
			eventName = ""
			return state.consume(keepalive, name, payload)
		}
		for {
			line, readErr := br.ReadString('\n')
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "event:"):
				if data.Len() > 0 {
					if err := flush(); err != nil {
						return
					}
				}
				eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				value := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(value)
			case strings.HasPrefix(line, ":"), strings.HasPrefix(line, "id:"), strings.HasPrefix(line, "retry:"):
				// SSE comment/control field.
			case strings.TrimSpace(line) == "":
				if err := flush(); err != nil {
					return
				}
			default:
				if strings.TrimSpace(line) != "" {
					if data.Len() > 0 {
						data.WriteByte('\n')
					}
					data.WriteString(strings.TrimSpace(line))
				}
			}
			if state.terminal {
				return
			}
			if readErr != nil {
				if data.Len() > 0 {
					if err := flush(); err != nil {
						return
					}
				}
				if !state.terminal {
					message := "Trae CN upstream stream ended before a done event"
					if readErr != io.EOF {
						message = readErr.Error()
					}
					_ = state.emitFailure(writer, ErrorCodeUpstreamStreamBreak, message)
				}
				return
			}
		}
	}()
	return reader
}

func traeCNCanonicalResponse(source io.ReadCloser) ([]byte, error) {
	defer source.Close()
	reader := bufio.NewReader(source)
	for {
		data, err := readSSEDataLine(reader)
		if len(data) > 0 {
			parsed := gjson.ParseBytes(data)
			switch parsed.Get("type").String() {
			case "response.completed", "response.incomplete", "response.failed":
				response := parsed.Get("response")
				if response.Exists() && response.IsObject() {
					return []byte(response.Raw), nil
				}
				return bytes.Clone(data), nil
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil, fmt.Errorf("Trae CN canonical stream ended without a terminal response")
			}
			return nil, err
		}
	}
}

func traeCNInboundStreaming(inbound GrokProtocol, inboundBody, responsesBody []byte) bool {
	body := inboundBody
	if len(body) == 0 {
		body = responsesBody
	}
	stream := gjson.GetBytes(body, "stream")
	return stream.Exists() && stream.Bool()
}

// ExecuteTraeCNRequest sends one canonical request to Trae CN and converts its
// provider SSE stream to the Responses representation consumed by the existing
// Chat/Responses/Messages translators.  The legacy entry point intentionally
// remains store-independent for isolated callers/tests; production handlers
// should use ExecuteTraeCNRequestWithStore so a lazy RT rotation is persisted.
func ExecuteTraeCNRequest(ctx context.Context, account *auth.Account, inbound GrokProtocol, inboundBody, responsesBody []byte, proxyOverride string, downstreamHeaders http.Header) (*http.Response, error) {
	return executeTraeCNRequest(ctx, nil, account, inbound, inboundBody, responsesBody, proxyOverride, downstreamHeaders)
}

// ExecuteTraeCNRequestWithStore is the durable request-path variant. It keeps
// lazy token refresh and inference on the same selected egress: the account's
// Resin lease when Resin is enabled, otherwise the resolved forward proxy.
// Rotated RTs are durably published before inference begins.
func ExecuteTraeCNRequestWithStore(ctx context.Context, store *auth.Store, account *auth.Account, inbound GrokProtocol, inboundBody, responsesBody []byte, proxyOverride string, downstreamHeaders http.Header) (*http.Response, error) {
	return executeTraeCNRequest(ctx, store, account, inbound, inboundBody, responsesBody, proxyOverride, downstreamHeaders)
}

func executeTraeCNRequest(ctx context.Context, store *auth.Store, account *auth.Account, inbound GrokProtocol, inboundBody, responsesBody []byte, proxyOverride string, downstreamHeaders http.Header) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if account == nil || !account.IsTraeCNAPI() {
		return nil, ErrNoAvailableAccount()
	}
	// 取连耗时（与 Codex WS 侧同口径）：本 attempt 内把账号变成"可以发请求"的
	// 花费 = 请求转换 + 懒刷新令牌/代理租约 + 客户端池选择 + 拿到可用连接。
	// 不含上游生成首内容的时间，因此 first_token_ms - ws_acquire_ms 就是纯粹的
	// 生成首内容耗时；此前 HTTP 中转路径恒为 0，TRAE 的首字看起来只能和 Codex
	// 的含取连口径对比。
	resetWsAcquireAudit(ctx)
	acquireStart := time.Now()
	canonical, err := canonicalGrokResponsesBody(inbound, inboundBody, responsesBody)
	if err != nil {
		return nil, ErrBadRequest("Trae CN request conversion failed: " + err.Error())
	}
	// PrepareResponsesBody adds the hosted image tool to ordinary Responses
	// requests. Trae CN has no image-generation surface, so remove that
	// gateway-injected capability while preserving an explicit image request as
	// a validation error from the cross-protocol tool checker below.
	if auth.NormalizeGrokProtocol(string(inbound)) == GrokProtocolResponses &&
		!responsesBodyHasImageGenerationTool(inboundBody) &&
		!responsesBodyRequestsImageGeneration(inboundBody) {
		canonical = stripResponsesImageGenerationCapabilities(canonical)
	}
	body, model, bridges, contracts, err := traeCNRequestBodyPlan(canonical, account.TraeCNEffectiveModels())
	if err != nil {
		return nil, ErrBadRequest("Trae CN request conversion failed: " + err.Error())
	}
	// Code 池见底、Work 池还有额度时切到 Work 端点（access_type=1），两个池分开消耗。
	body = applyTraeCNAccessType(body, traeCNAccessTypeForAccount(account))
	proxyURL := strings.TrimSpace(proxyOverride)
	if proxyURL == "" {
		account.Mu().RLock()
		proxyURL = strings.TrimSpace(account.ProxyURL)
		account.Mu().RUnlock()
	}
	var ensureErr error
	if store != nil {
		ensureErr = store.EnsureTraeCNAccountWithProxy(ctx, account, proxyURL)
	} else {
		ensureErr = account.EnsureTraeCNAccessToken(ctx, proxyURL, false)
	}
	if ensureErr != nil {
		return nil, ErrUpstream(http.StatusUnauthorized, "Trae CN token refresh failed", ensureErr)
	}
	host, accessToken := account.TraeCNCredentials()
	requestID := uuid.NewString()
	endpoint := host + auth.TraeCNChatPath
	// Persisted accounts have a stable DBID and therefore a stable Resin lease.
	// Store-independent fixtures with DBID=0 stay on their explicit proxy/direct
	// route instead of collapsing unrelated identities into one shared "0" lease.
	viaResin := IsResinEnabledForContext(ctx) && account.ID() > 0
	resinPlatform := ResinPlatformFromContext(ctx)
	if viaResin && resinPlatform == "" {
		sessionBody := inboundBody
		if len(sessionBody) == 0 {
			sessionBody = responsesBody
		}
		identity := resolveRequestSessionIdentity(downstreamHeaders, sessionBody)
		if identity.hasStableAffinity {
			resinPlatform = ResinPlatformForSessionFromContext(ctx, identity.affinityID)
		} else {
			resinPlatform = ResinPlatformForSessionFromContext(ctx, "")
		}
	}
	var client *http.Client
	if viaResin {
		endpoint = BuildReverseProxyURLForContext(ctx, endpoint, resinPlatform)
		client = getResinHTTPClient(account)
	} else {
		client = getPooledClient(account, proxyURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, ErrInternalError("创建 Trae CN 请求失败", err)
	}
	req.Header = auth.TraeCNRequestHeaders(account, accessToken, requestID)
	// 真实客户端会带上同一条上游 URL 作为 Referer（抓包实测），补上以免少一个客户端特征。
	req.Header.Set("Referer", endpoint)
	// Do not forward the downstream client User-Agent. Trae includes it in its
	// desktop-client fingerprint and returns application code 4011 for generic
	// Go/Codex user agents even when the token and request body are valid.
	account.Mu().RLock()
	customHeaders := make(map[string]string, len(account.CustomHeaders))
	for key, value := range account.CustomHeaders {
		customHeaders[key] = value
	}
	account.Mu().RUnlock()
	for key, value := range customHeaders {
		if strings.EqualFold(key, "Authorization") || strings.EqualFold(key, "X-Cloudide-Token") {
			continue
		}
		if !strings.ContainsAny(value, "\r\n") {
			req.Header.Set(key, value)
		}
	}
	// Resin owns the sticky egress identity. Set this after account custom
	// headers so an imported header cannot move the request to another lease.
	if viaResin {
		req.Header.Set("X-Resin-Account", ResinAccountID(account))
	}
	// 到这里连接参数、凭据和客户端都已就绪：这段是执行器侧的取连成本。
	AddWsAcquireDuration(ctx, time.Since(acquireStart))
	// 连接本身的获取（连接池命中约等于 0；冷连接含拨号/TLS/代理 CONNECT）由
	// net/http 的连接回调单独计时。uTLS 传输不走 net/http 连接池，改由
	// utlsRoundTripper.RoundTrip 记录，两条路径互斥，不会重复累加。
	dispatchStart := time.Now()
	var connectedAt time.Time
	trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) {
		if connectedAt.IsZero() {
			connectedAt = time.Now()
		}
	}}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	resp, err := client.Do(req)
	if !connectedAt.IsZero() {
		AddWsAcquireDuration(ctx, connectedAt.Sub(dispatchStart))
	}
	if err != nil {
		if !viaResin && shouldRecyclePooledClient(err) {
			recyclePooledClient(account, proxyURL)
		}
		return nil, ErrUpstream(0, "请求 Trae CN 上游失败", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		normalizeTraeCNLimitHTTPResponse(resp)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			// 上层日志按通用策略省略非 JSON 错误体，Trae 的 4xx 却常常只有十几个
			// 字节的纯文本。这里补上"上游说了什么"和"我们发了什么"，否则模型不在
			// 目录、请求体过大、item 形态被拒这三种 400 在日志里完全无法区分。
			preview := ""
			if prefix := traeCNErrorBodyPrefix(resp); len(prefix) > 0 && len(prefix) <= 128 && !json.Valid(prefix) {
				preview = security.SanitizeLog(strings.Join(strings.Fields(string(prefix)), " "))
			}
			log.Printf("[TRAECN] stage=upstream_reject status=%d via_resin=%t model=%q config_name=%q wire_bytes=%d messages=%d tools=%d body=%q request_id=%q",
				resp.StatusCode, viaResin, gjson.GetBytes(body, "model").String(), gjson.GetBytes(body, "config_name").String(),
				len(body), len(gjson.GetBytes(body, "messages").Array()), len(gjson.GetBytes(body, "tools").Array()), preview, requestID)
		}
		return resp, nil
	}
	upstreamBody := wrapTraeCNCreditsRemainScanner(wrapTraeCNResumeUpstream(ctx, client, req, resp.Body), account)
	canonicalStream := traeCNCanonicalStreamForTools(upstreamBody, model, bridges, contracts)
	// Chat and Messages handlers deliberately aggregate canonical SSE for their
	// non-stream response types. Native Responses non-stream instead expects one
	// response JSON object, so aggregate only that inbound protocol here.
	if auth.NormalizeGrokProtocol(string(inbound)) == GrokProtocolResponses && !traeCNInboundStreaming(inbound, inboundBody, responsesBody) {
		payload, aggregateErr := traeCNCanonicalResponse(canonicalStream)
		if aggregateErr != nil {
			return nil, ErrUpstream(0, "读取 Trae CN 响应失败", aggregateErr)
		}
		resp.Body = io.NopCloser(bytes.NewReader(payload))
		resp.Header.Set("Content-Type", "application/json")
		resp.Header.Set("Content-Length", strconv.Itoa(len(payload)))
		resp.ContentLength = int64(len(payload))
		return resp, nil
	}
	resp.Header.Set("Content-Type", "text/event-stream")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Body = canonicalStream
	return resp, nil
}
