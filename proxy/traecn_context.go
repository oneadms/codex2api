package proxy

import (
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// TRAE 没有可供网关续接的服务端响应存储，完整对话与通用工具缓存分开保存。
// owner 仍包含下游 API Key，换上游账号不会断链，也不能跨 API Key 读取历史。
func traeCNResponseCacheOwner(owner string) string {
	return "traecn:" + owner
}

func prepareTraeCNResponsesContext(body []byte, owner string) responsesBodyPreparation {
	previousID := strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String())
	prepared := responsesBodyPreparation{Body: body, PreviousResponseID: previousID, RequiresLocalContext: previousID != ""}
	var input []json.RawMessage
	current := gjson.GetBytes(body, "input")
	if current.Type == gjson.String {
		message, _ := json.Marshal(map[string]any{"role": "user", "content": current.String()})
		input = append(input, message)
	} else if current.IsArray() {
		current.ForEach(func(_, item gjson.Result) bool {
			input = append(input, json.RawMessage(item.Raw))
			return true
		})
	}
	if previousID != "" {
		prepared.CacheLookup = getResponseCacheForReplay(traeCNResponseCacheOwner(owner), previousID)
		if prepared.CacheLookup.Kind != responseCacheLookupHit {
			return prepared
		}
		var history []json.RawMessage
		if len(prepared.CacheLookup.Items) != 1 || json.Unmarshal(prepared.CacheLookup.Items[0], &history) != nil || history == nil {
			prepared.CacheLookup.Kind = responseCacheLookupBackendCorrupt
			return prepared
		}
		input = mergeTraeCNResponseInput(history, input)
	}
	if input == nil {
		input = []json.RawMessage{}
	}
	rawInput, _ := json.Marshal(input)
	prepared.Body, _ = sjson.SetRawBytes(body, "input", rawInput)
	prepared.Body, _ = sjson.DeleteBytes(prepared.Body, "previous_response_id")
	var normalized map[string]any
	if json.Unmarshal(prepared.Body, &normalized) == nil && normalizeResponsesCompactionItems(normalized) {
		prepared.Body, _ = json.Marshal(normalized)
	}
	prepared.ExpandedInputRaw = gjson.GetBytes(prepared.Body, "input").Raw
	return prepared
}

// 部分客户端会连同工具结果回传原调用。按稳定标识去重，不能因为出现一条
// function_call 就跳过整段历史，也不能把内容相同但属于新一轮的用户消息删掉。
func mergeTraeCNResponseInput(history, current []json.RawMessage) []json.RawMessage {
	known := make(map[string]struct{}, len(history))
	for _, item := range history {
		if key := traeCNResponseItemKey(item); key != "" {
			known[key] = struct{}{}
		}
	}
	merged := append([]json.RawMessage(nil), history...)
	for _, item := range current {
		if key := traeCNResponseItemKey(item); key != "" {
			if _, exists := known[key]; exists {
				continue
			}
		}
		merged = append(merged, item)
	}
	return merged
}

func traeCNResponseItemKey(raw []byte) string {
	item := gjson.ParseBytes(raw)
	typ := item.Get("type").String()
	if strings.HasSuffix(typ, "_call") || strings.HasSuffix(typ, "_call_output") || typ == "tool_search_output" {
		if callID := item.Get("call_id").String(); callID != "" {
			return typ + ":" + callID
		}
	}
	if id := item.Get("id").String(); id != "" {
		return "id:" + id
	}
	return ""
}

func cacheTraeCNResponseContext(owner string, requestBody, responseBody []byte) {
	response := gjson.ParseBytes(responseBody)
	if response.Get("status").String() != "completed" || response.Get("id").String() == "" || !response.Get("output").IsArray() {
		return
	}
	var items []json.RawMessage
	for _, list := range []gjson.Result{gjson.GetBytes(requestBody, "input"), response.Get("output")} {
		list.ForEach(func(_, item gjson.Result) bool {
			items = append(items, json.RawMessage(item.Raw))
			return true
		})
	}
	if len(items) == 0 {
		return
	}
	history, err := json.Marshal(items)
	if err != nil {
		return
	}
	cacheOwner := traeCNResponseCacheOwner(owner)
	// 第一轮就必须可续接，不能等客户端首次携带 previous_response_id 后才开始写入。
	markResponseCacheChainOwnerIfOnDemand(cacheOwner)
	// 完整历史作为单个缓存值，避免通用工具缓存的尾部截取悄悄丢失早期对话。
	// 仍受现有字节预算、TTL、淘汰和共享后端重建上限约束；超限后续接明确报错。
	setResponseCache(cacheOwner, response.Get("id").String(), []json.RawMessage{history})
}
