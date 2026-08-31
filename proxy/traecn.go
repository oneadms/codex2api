package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/codex2api/auth"
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
func traeCNMessagesFromResponses(body []byte) ([]map[string]any, error) {
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return nil, fmt.Errorf("Trae CN request must be a JSON object")
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
			message["tool_calls"] = calls.Value()
		}
		messages = append(messages, message)
	}

	input := root.Get("input")
	knownCalls := make(map[string]struct{})
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
				var summary strings.Builder
				item.Get("summary").ForEach(func(_, part gjson.Result) bool {
					if text := traeFirstText(part, "text", "content"); text != "" {
						if summary.Len() > 0 {
							summary.WriteByte('\n')
						}
						summary.WriteString(text)
					}
					return true
				})
				if summary.Len() > 0 {
					messages = append(messages, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": summary.String()}}})
				}
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
				messages = append(messages, map[string]any{
					"role": "assistant", "content": []any{},
					"tool_calls": []any{map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": item.Get("name").String(), "arguments": arguments}}},
				})
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
			case "":
				// Ignore empty input items emitted by some Responses clients.
			default:
				conversionErr = fmt.Errorf("Responses input item type %q cannot be represented by Trae CN", typ)
				return false
			}
			return true
		})
		if conversionErr != nil {
			return nil, conversionErr
		}
	} else if messagesInput := root.Get("messages"); messagesInput.IsArray() {
		// Defensive support for direct tests/callers; production handlers provide
		// canonical Responses input.
		messagesInput.ForEach(func(_, item gjson.Result) bool { appendMessage(item); return true })
	}
	if len(messages) == 0 {
		messages = append(messages, map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": ""}}})
	}
	return messages, nil
}

func buildTraeCNRequestBody(canonical []byte) ([]byte, string, error) {
	root := gjson.ParseBytes(canonical)
	if !root.IsObject() {
		return nil, "", fmt.Errorf("invalid canonical request")
	}
	model := strings.TrimSpace(root.Get("model").String())
	messages, err := traeCNMessagesFromResponses(canonical)
	if err != nil {
		return nil, model, err
	}
	functionName := "chat_v3"
	if model == "" || strings.EqualFold(model, "auto") {
		functionName = "inline_chat"
	}
	body := map[string]any{"messages": messages, "function": functionName, "stream": true}
	if model != "" && !strings.EqualFold(model, "auto") {
		body["model"] = auth.TraeCNWireModel(model)
	}
	if err := validateCrossProtocolTools(root.Get("tools"), "Trae CN"); err != nil {
		return nil, model, err
	}
	if tools := responsesToolsToChat(root.Get("tools")); len(tools) > 0 {
		body["tools"] = tools
	}
	if choice := root.Get("tool_choice"); choice.Exists() {
		converted, choiceErr := responsesToolChoiceToChat(choice)
		if choiceErr != nil {
			return nil, model, choiceErr
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
	return encoded, model, err
}

func traeCanonicalFailure(id, model, code, message string) []byte {
	if strings.TrimSpace(code) == "" || code == "0" {
		code = ErrorCodeUpstreamError
	}
	if strings.TrimSpace(message) == "" {
		message = "Trae CN upstream request failed"
	}
	payload, _ := json.Marshal(map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"id": id, "object": "response", "status": "failed", "model": model,
			"error": map[string]any{"code": code, "message": message},
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
)

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

	viaResin := IsResinEnabled() && account.ID() > 0
	resinPlatform := ResinPlatformFromContext(ctx)
	if viaResin && resinPlatform == "" {
		resinPlatform = ResinPlatformForSession("")
	}
	client := getPooledClient(account, proxyOverride)
	buildEndpoint := func(path string) string {
		endpoint := strings.TrimRight(host, "/") + path
		if viaResin {
			return BuildReverseProxyURLForPlatform(endpoint, resinPlatform)
		}
		return endpoint
	}
	doRequest := func(method, path string, body []byte, providerAuth bool) ([]byte, int, error) {
		requestCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		req, requestErr := http.NewRequestWithContext(requestCtx, method, buildEndpoint(path), bytes.NewReader(body))
		if requestErr != nil {
			return nil, 0, requestErr
		}
		var headers http.Header
		if providerAuth {
			headers = auth.TraeCNRequestHeaders(account, accessToken, uuid.NewString())
		} else {
			headers = auth.TraeCNRequestHeaders(account, accessToken, uuid.NewString())
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

// traeCNModelTokenKey is used only for matching a provider config name to a
// known public alias.  It deliberately drops punctuation so names such as
// Doubao_1_6 and doubao-1-6 can share the same compatibility ID.
func traeCNModelTokenKey(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
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
// IDs that clients can send to this gateway.  Known configs use the aliases
// shared with TraeCNWireModel; unknown non-custom configs remain selectable by
// their exact provider name so a newly introduced model is not discarded.
func traeCNPublicIDsForConfig(configName string) []string {
	configName = strings.TrimSpace(configName)
	if configName == "" {
		return nil
	}
	if aliases := auth.TraeCNPublicModelIDsForWire(configName); len(aliases) > 0 {
		return aliases
	}
	configKey := traeCNModelTokenKey(configName)
	if configKey != "" {
		for _, publicID := range auth.TraeCNDefaultModelIDs() {
			if strings.EqualFold(publicID, "auto") {
				continue
			}
			if configKey == traeCNModelTokenKey(publicID) || configKey == traeCNModelTokenKey(auth.TraeCNWireModel(publicID)) {
				return []string{publicID}
			}
		}
	}
	name := strings.ToLower(configName)
	// Generic custom_model_* entries are implementation placeholders.  Only
	// the stable aliases in traeCNWireModels are exposed (for example gpt-4o or
	// deepseek-r1); leaking arbitrary placeholders makes them appear routable
	// while their provider identity is account-specific.
	if strings.HasPrefix(name, "custom_model_") {
		return nil
	}
	return []string{configName}
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

// IsTraeCNRateLimitError recognizes Trae's application-level throttling. The
// provider returns these failures inside a successful HTTP 200 SSE stream, so
// callers must not rely on the transport status alone.
func IsTraeCNRateLimitError(payload []byte) bool {
	if len(bytes.TrimSpace(payload)) == 0 || !gjson.ValidBytes(payload) {
		return false
	}
	root := gjson.ParseBytes(payload)
	code := strings.ToLower(strings.TrimSpace(traeFirstText(root,
		"response.error.code", "response.status_details.error.code", "error.code", "code", "error_code", "errorCode")))
	switch code {
	case "4011", "429", "rate_limit", "rate_limited", "rate_limit_exceeded":
		return true
	}
	message := strings.ToLower(strings.TrimSpace(traeFirstText(root,
		"response.error.message", "response.status_details.error.message", "error.message", "message", "msg")))
	return strings.Contains(message, "exceeded the rate limit") ||
		strings.Contains(message, "too many requests") ||
		strings.Contains(message, "rate limit exceeded")
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
	key          string
	id           string
	callID       string
	name         string
	arguments    strings.Builder
	outputIndex  int
	emittedBytes int
	itemAdded    bool
	custom       bool
}

type traeCNCanonicalState struct {
	responseID   string
	model        string
	terminal     bool
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
	if err := s.ensureReasoning(writer); err != nil {
		return err
	}
	s.reasoning.WriteString(text)
	return writeTraeCanonicalEvent(writer, marshalTraeCanonicalEvent("response.reasoning_summary_text.delta", map[string]any{
		"response_id": s.responseID, "item_id": s.reasoningID, "output_index": s.reasoningIdx, "summary_index": 0, "delta": text,
	}))
}

func traeToolCalls(root gjson.Result) []gjson.Result {
	for _, path := range []string{"tool_calls", "message.tool_calls", "choices.0.delta.tool_calls", "choices.0.message.tool_calls"} {
		value := root.Get(path)
		if value.IsArray() {
			return value.Array()
		}
		if value.IsObject() {
			return []gjson.Result{value}
		}
	}
	switch strings.ToLower(strings.TrimSpace(root.Get("type").String())) {
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
	id := strings.TrimSpace(traeFirstText(raw, "id", "call_id", "tool_call_id"))
	callID := strings.TrimSpace(traeFirstText(raw, "call_id", "id", "tool_call_id"))
	name := strings.TrimSpace(traeFirstText(raw, "function.name", "name", "tool_name"))
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
		tool = s.toolByKey[indexKey]
	}
	if tool == nil {
		if id == "" {
			id = "call_" + uuid.NewString()
		}
		if callID == "" {
			callID = id
		}
		tool = &traeCNToolCall{key: key, id: id, callID: callID, name: name, outputIndex: len(s.output), custom: strings.EqualFold(raw.Get("type").String(), "custom_tool_call")}
		s.toolByKey[key] = tool
		s.toolByKey[indexKey] = tool
		s.tools = append(s.tools, tool)
		s.output = append(s.output, traeCNOutputRef{kind: traeCNOutputTool, tool: tool})
	} else {
		// Providers commonly include id/name only in the first tool delta and
		// identify later argument fragments solely by index. Alias both keys so
		// those fragments continue the same canonical output item.
		s.toolByKey[indexKey] = tool
		if id != "" {
			s.toolByKey[id] = tool
		}
	}
	if tool.name == "" && name != "" {
		tool.name = name
	}
	argument := traeArgumentString(traeToolField(raw, "function.arguments", "arguments", "input", "params", "function.input", "arguments_delta"))
	if argument != "" {
		current := tool.arguments.String()
		switch {
		case current == "":
			tool.arguments.WriteString(argument)
		case strings.HasPrefix(argument, current):
			tool.arguments.WriteString(argument[len(current):])
		case strings.HasPrefix(current, argument):
			// Repeated complete tool call; nothing new to emit.
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
	if !tool.itemAdded {
		itemType := "function_call"
		if tool.custom {
			itemType = "custom_tool_call"
		}
		if err := writeTraeCanonicalEvent(writer, marshalTraeCanonicalEvent("response.output_item.added", map[string]any{
			"response_id": s.responseID, "output_index": tool.outputIndex,
			"item": map[string]any{"id": tool.id, "type": itemType, "call_id": tool.callID, "name": tool.name, "arguments": "", "input": "", "status": "in_progress"},
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
				return nil, fmt.Errorf("Trae CN returned a tool call without a name")
			}
			if tool.arguments.Len() == 0 {
				tool.arguments.WriteString("{}")
			}
			if !tool.custom && !json.Valid([]byte(tool.arguments.String())) {
				return nil, fmt.Errorf("Trae CN returned invalid JSON arguments for tool %q", tool.name)
			}
			if err := s.emitToolProgress(writer, tool); err != nil {
				return nil, err
			}
			itemType := "function_call"
			doneEvent := "response.function_call_arguments.done"
			if tool.custom {
				itemType = "custom_tool_call"
				doneEvent = "response.custom_tool_call_input.done"
			}
			item := map[string]any{"id": tool.id, "type": itemType, "call_id": tool.callID, "name": tool.name, "arguments": tool.arguments.String(), "input": tool.arguments.String(), "status": status}
			output = append(output, item)
			if status == "completed" {
				if err := writeTraeCanonicalEvent(writer, marshalTraeCanonicalEvent(doneEvent, map[string]any{"response_id": s.responseID, "output_index": tool.outputIndex, "item_id": tool.id, "call_id": tool.callID, "arguments": tool.arguments.String(), "input": tool.arguments.String()})); err != nil {
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

func (s *traeCNCanonicalState) emitCompleted(writer io.Writer, finishReason string) error {
	if s.terminal {
		return nil
	}
	incompleteReason := traeIncompleteReason(finishReason)
	status := "completed"
	eventType := "response.completed"
	if incompleteReason != "" {
		status = "incomplete"
		eventType = "response.incomplete"
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
	s.terminal = true
	return writeTraeCanonicalEvent(writer, marshalTraeCanonicalEvent(eventType, map[string]any{"response": response}))
}

func (s *traeCNCanonicalState) emitFailure(writer io.Writer, code, message string) error {
	if s.terminal {
		return nil
	}
	s.terminal = true
	return writeTraeCanonicalEvent(writer, traeCanonicalFailure(s.responseID, s.model, code, message))
}

func (s *traeCNCanonicalState) consume(writer io.Writer, eventName string, data []byte) error {
	if s.terminal {
		return nil
	}
	if strings.TrimSpace(string(data)) == "[DONE]" {
		return s.emitCompleted(writer, "stop")
	}
	parsed, payloadEventName := parseTraePayload(data)
	name := strings.ToLower(strings.TrimSpace(eventName))
	if name == "" || name == "data" {
		name = strings.ToLower(payloadEventName)
	}
	if name == "" && parsed.Get("done").Bool() {
		name = "done"
	}
	if name == "error" || name == "failed" {
		code, message := traeProviderError(parsed)
		return s.emitFailure(writer, code, message)
	}
	if parsed.IsObject() {
		code, message := traeProviderError(parsed)
		if (parsed.Get("success").Exists() && !parsed.Get("success").Bool()) || (code != "" && code != "0" && message != "" && name != "token_usage") {
			return s.emitFailure(writer, code, message)
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
		for index, tool := range traeToolCalls(parsed) {
			if err := s.mergeToolCall(writer, tool, index); err != nil {
				return err
			}
		}
	case "token_usage", "usage":
		s.updateUsage(parsed)
	case "done", "end", "finish", "message_end":
		finishReason := traeFirstText(parsed, "finish_reason", "stop_reason", "reason")
		if finishReason == "" {
			finishReason = "stop"
		}
		return s.emitCompleted(writer, finishReason)
	case "metadata", "timing_cost", "extra_info", "progress_notice", "queue_begin", "request_wait_in_queue", "queue_end":
		// Provider lifecycle/diagnostic events do not carry model output.
	default:
		// Forward-compatible fallback for a renamed output event.
		content := traeFirstText(parsed, "response", "content", "text", "message.content")
		reasoning := traeFirstText(parsed, "reasoning_content", "reasoning")
		calls := traeToolCalls(parsed)
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
	reader, writer := io.Pipe()
	go func() {
		defer source.Close()
		defer writer.Close()
		state := newTraeCNCanonicalState(model)
		if err := state.emitCreated(writer); err != nil {
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
			return state.consume(writer, name, payload)
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
	body, model, err := buildTraeCNRequestBody(canonical)
	if err != nil {
		return nil, ErrBadRequest("Trae CN request conversion failed: " + err.Error())
	}
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
	viaResin := IsResinEnabled() && account.ID() > 0
	resinPlatform := ResinPlatformFromContext(ctx)
	if viaResin && resinPlatform == "" {
		sessionBody := inboundBody
		if len(sessionBody) == 0 {
			sessionBody = responsesBody
		}
		identity := resolveRequestSessionIdentity(downstreamHeaders, sessionBody)
		if identity.hasStableAffinity {
			resinPlatform = ResinPlatformForSession(identity.affinityID)
		} else {
			resinPlatform = ResinPlatformForSession("")
		}
	}
	var client *http.Client
	if viaResin {
		endpoint = BuildReverseProxyURLForPlatform(endpoint, resinPlatform)
		client = getResinHTTPClient(account)
	} else {
		client = getPooledClient(account, proxyURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, ErrInternalError("创建 Trae CN 请求失败", err)
	}
	req.Header = auth.TraeCNRequestHeaders(account, accessToken, requestID)
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
	resp, err := client.Do(req)
	if err != nil {
		if !viaResin && shouldRecyclePooledClient(err) {
			recyclePooledClient(account, proxyURL)
		}
		return nil, ErrUpstream(0, "请求 Trae CN 上游失败", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, nil
	}
	canonicalStream := traeCNCanonicalStream(resp.Body, model)
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
