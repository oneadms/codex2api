package proxy

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestBuildTraeCNRequestBodyPreservesCanonicalSemantics(t *testing.T) {
	t.Parallel()
	canonical := []byte(`{
  "model":"deepseek-v3",
  "instructions":"follow the system instruction",
  "input":[
    {"type":"message","role":"user","content":[
      {"type":"input_text","text":"hello"},
      {"type":"input_image","image_url":{"url":"https://example.test/image.png"}}
    ]},
    {"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"id\":1}"},
    {"type":"function_call_output","call_id":"call_1","output":{"ok":true}}
  ],
  "tools":[{"type":"function","name":"lookup","description":"Lookup","parameters":{"type":"object"}}],
  "tool_choice":"auto",
  "parallel_tool_calls":false,
  "reasoning":{"effort":"high"},
  "max_output_tokens":123,
  "temperature":0.2,
  "top_p":0.9,
  "stop":"END",
  "seed":7
}`)
	body, model, err := buildTraeCNRequestBody(canonical)
	if err != nil {
		t.Fatalf("buildTraeCNRequestBody() error = %v", err)
	}
	if model != "deepseek-v3" {
		t.Fatalf("model = %q", model)
	}
	root := gjson.ParseBytes(body)
	if root.Get("model").String() != "deepseek-v3" {
		t.Fatalf("wire model = %q, body=%s", root.Get("model").String(), body)
	}
	// Trae 按 config_name 选后端，必须与 model 一起发送，否则回落到默认模型。
	if root.Get("config_name").String() != "deepseek-v3" || root.Get("model").String() != "deepseek-v3" {
		t.Fatalf("config_name/model必须同时给出目标模型: %s", body)
	}
	if root.Get("function").String() != "chat_v3" || !root.Get("stream").Bool() {
		t.Fatalf("unexpected request mode: %s", body)
	}
	if root.Get("messages.0.role").String() != "system" || !strings.Contains(root.Get("messages.0.content.0.text").String(), "follow the system instruction") {
		t.Fatalf("instructions were not preserved: %s", body)
	}
	if root.Get("messages.1.content.0.text").String() != "hello" || root.Get("messages.1.content.1.image_url.url").String() != "https://example.test/image.png" {
		t.Fatalf("user content shifted: %s", body)
	}
	if root.Get("messages.2.tool_calls.0.function_call.name").String() != "lookup" || root.Get("messages.3.tool_call_id").String() != "call_1" {
		t.Fatalf("tool history was not preserved: %s", body)
	}
	if root.Get("messages.3.content.0.text").String() != `{"ok":true}` {
		t.Fatalf("structured tool output was not preserved: %s", body)
	}
	if root.Get("tools.0.function.name").String() != "lookup" || root.Get("tool_choice").String() != "auto" {
		t.Fatalf("tools were not converted: %s", body)
	}
	if root.Get("max_tokens").Int() != 123 || root.Get("reasoning_effort").String() != "high" || root.Get("stop.0").String() != "END" {
		t.Fatalf("generation parameters were not preserved: %s", body)
	}
}

func TestIsTraeCNRateLimitErrorRecognizesApplicationLevel4011(t *testing.T) {
	t.Parallel()
	tests := []struct {
		payload string
		want    bool
	}{
		{payload: `{"type":"response.failed","response":{"error":{"code":"4011","message":"We're sorry, your requests have exceeded the rate limit."}}}`, want: true},
		{payload: `{"type":"error","code":429,"message":"too many requests"}`, want: true},
		{payload: `{"type":"response.failed","response":{"error":{"code":"3003","message":"all models failed"}}}`, want: false},
		{payload: `not-json`, want: false},
	}
	for _, test := range tests {
		if got := IsTraeCNRateLimitError([]byte(test.payload)); got != test.want {
			t.Errorf("IsTraeCNRateLimitError(%s) = %v, want %v", test.payload, got, test.want)
		}
	}
}

func TestFetchTraeCNModelsUsesWrapperDetailEndpoint(t *testing.T) {
	t.Parallel()
	var detailCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			http.NotFound(w, r)
		case "/v1/models/detail":
			detailCalls++
			if r.URL.Query().Get("function") != "chat_v3" {
				t.Errorf("detail function = %q, want chat_v3", r.URL.Query().Get("function"))
			}
			if got := r.Header.Get("Authorization"); got != "Bearer wrapper-key" {
				t.Errorf("detail authorization = %q, want wrapper key", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"config_info_list":[{"config_name":"DeepSeek-V4-Pro","usage":"chat_completion","config_switch":true}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	account := &auth.Account{
		DBID:         0,
		UpstreamType: auth.UpstreamTraeCN,
		AccessToken:  "wrapper-key",
		ExpiresAt:    time.Now().Add(time.Hour),
		TraeCNHost:   server.URL,
	}
	models, err := FetchTraeCNModels(t.Context(), account, "")
	if err != nil {
		t.Fatalf("FetchTraeCNModels() error = %v", err)
	}
	if detailCalls != 1 {
		t.Fatalf("detail calls = %d, want 1", detailCalls)
	}
	// 别名表删除后，provider 的 config 名称就是对外 ID（按目录里的写法归一大小写）。
	if len(models) != 2 || !containsFold(models, "deepseek-v4-pro") || !containsFold(models, "auto") {
		t.Fatalf("models = %#v, want the provider config name plus auto", models)
	}
}

func canonicalSSEEvents(t *testing.T, raw []byte) []gjson.Result {
	t.Helper()
	lines := strings.Split(string(raw), "\n")
	events := make([]gjson.Result, 0)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if !gjson.Valid(payload) {
			t.Fatalf("invalid canonical SSE payload %q", payload)
		}
		events = append(events, gjson.Parse(payload))
	}
	return events
}

func findCanonicalEvent(events []gjson.Result, typ string) (gjson.Result, bool) {
	for _, event := range events {
		if event.Get("type").String() == typ {
			return event, true
		}
	}
	return gjson.Result{}, false
}

func TestTraeCNCanonicalStreamTextReasoningToolsUsageAndDone(t *testing.T) {
	t.Parallel()
	provider := strings.Join([]string{
		`data: {"data":{"type":"output","data":{"type":"text","reasoning":"think "}}}`,
		"",
		`event: output`,
		`data: {"type":"text","content":"answer"}`,
		"",
		`event: output`,
		`data: {"type":"text","tool_calls":[{"index":0,"id":"call_weather","function":{"name":"weather","arguments":"{\"city\""}}]}`,
		"",
		`event: output`,
		`data: {"type":"text","tool_calls":[{"index":0,"function":{"arguments":":\"Shanghai\"}"}}]}`,
		"",
		`data: {"type":"token_usage","data":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8,"reasoning_tokens":2}}`,
		"",
		`data: {"type":"done","data":{"finish_reason":"tool_calls"}}`,
		"",
	}, "\n")
	stream := traeCNCanonicalStream(io.NopCloser(strings.NewReader(provider)), "deepseek-v3")
	raw, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read canonical stream: %v", err)
	}
	events := canonicalSSEEvents(t, raw)
	if _, ok := findCanonicalEvent(events, "response.created"); !ok {
		t.Fatalf("missing response.created: %s", raw)
	}
	if event, ok := findCanonicalEvent(events, "response.reasoning_summary_text.delta"); !ok || event.Get("delta").String() != "think " {
		t.Fatalf("missing reasoning delta: %s", raw)
	}
	if event, ok := findCanonicalEvent(events, "response.output_text.delta"); !ok || event.Get("delta").String() != "answer" {
		t.Fatalf("missing text delta: %s", raw)
	}
	if _, ok := findCanonicalEvent(events, "response.function_call_arguments.done"); !ok {
		t.Fatalf("missing tool arguments done event: %s", raw)
	}
	completed, ok := findCanonicalEvent(events, "response.completed")
	if !ok {
		t.Fatalf("missing response.completed: %s", raw)
	}
	response := completed.Get("response")
	if response.Get("usage.input_tokens").Int() != 3 || response.Get("usage.output_tokens").Int() != 5 || response.Get("usage.output_tokens_details.reasoning_tokens").Int() != 2 {
		t.Fatalf("usage mismatch: %s", response.Raw)
	}
	if response.Get("output.#(type==\"reasoning\").summary.0.text").String() != "think " {
		t.Fatalf("reasoning output mismatch: %s", response.Raw)
	}
	if response.Get("output.#(type==\"message\").content.0.text").String() != "answer" {
		t.Fatalf("message output mismatch: %s", response.Raw)
	}
	tool := response.Get("output.#(type==\"function_call\")")
	if tool.Get("name").String() != "weather" || tool.Get("arguments").String() != `{"city":"Shanghai"}` {
		t.Fatalf("tool output mismatch: %s", response.Raw)
	}
}

func TestTraeCNCanonicalStreamNestedUsageAndCacheBilling(t *testing.T) {
	t.Parallel()
	provider := strings.Join([]string{
		`event: output`,
		`data: {"content":"ok"}`,
		``,
		`event: token_usage`,
		`data: {"type":"token_usage","data":{"token_usage":{"data":{"prompt_tokens":379,"completion_tokens":3,"total_tokens":382,"cache_creation_input_tokens":12,"cache_read_input_tokens":256,"reasoning_tokens":1}}}}`,
		``,
		`event: done`,
		`data: {"finish_reason":"stop"}`,
		``,
	}, "\n")
	raw, err := io.ReadAll(traeCNCanonicalStream(io.NopCloser(strings.NewReader(provider)), "deepseek-v3"))
	if err != nil {
		t.Fatal(err)
	}
	completed, ok := findCanonicalEvent(canonicalSSEEvents(t, raw), "response.completed")
	if !ok {
		t.Fatalf("missing response.completed: %s", raw)
	}
	usage := completed.Get("response.usage")
	if usage.Get("input_tokens").Int() != 379 || usage.Get("output_tokens").Int() != 3 || usage.Get("total_tokens").Int() != 382 {
		t.Fatalf("nested usage mismatch: %s", usage.Raw)
	}
	if usage.Get("input_tokens_details.cached_tokens").Int() != 256 || usage.Get("input_tokens_details.cache_creation_tokens").Int() != 12 {
		t.Fatalf("cache usage mismatch: %s", usage.Raw)
	}
	if usage.Get("output_tokens_details.reasoning_tokens").Int() != 1 {
		t.Fatalf("reasoning usage mismatch: %s", usage.Raw)
	}

	eventBytes := []byte(completed.Raw)
	billing := extractUsage(eventBytes)
	if billing == nil || billing.PromptTokens != 379 || billing.CompletionTokens != 3 || billing.CachedTokens != 256 || billing.ReasoningTokens != 1 {
		t.Fatalf("billing usage = %+v, want input=379 output=3 cached=256 reasoning=1", billing)
	}
}

func TestTraeCNCanonicalStreamProviderErrorAndUnexpectedEOF(t *testing.T) {
	t.Parallel()
	t.Run("provider error", func(t *testing.T) {
		provider := "event: error\ndata: {\"code\":\"quota_exhausted\",\"message\":\"quota exhausted\"}\n\n"
		raw, err := io.ReadAll(traeCNCanonicalStream(io.NopCloser(strings.NewReader(provider)), "model"))
		if err != nil {
			t.Fatal(err)
		}
		event, ok := findCanonicalEvent(canonicalSSEEvents(t, raw), "response.failed")
		if !ok || event.Get("response.error.code").String() != "quota_exhausted" || event.Get("response.error.message").String() != "quota exhausted" {
			t.Fatalf("unexpected failure event: %s", raw)
		}
	})
	t.Run("unexpected eof", func(t *testing.T) {
		provider := "event: output\ndata: {\"content\":\"partial\"}\n\n"
		raw, err := io.ReadAll(traeCNCanonicalStream(io.NopCloser(strings.NewReader(provider)), "model"))
		if err != nil {
			t.Fatal(err)
		}
		event, ok := findCanonicalEvent(canonicalSSEEvents(t, raw), "response.failed")
		if !ok || event.Get("response.error.code").String() != ErrorCodeUpstreamStreamBreak {
			t.Fatalf("unexpected EOF was not reported: %s", raw)
		}
		if _, completed := findCanonicalEvent(canonicalSSEEvents(t, raw), "response.completed"); completed {
			t.Fatalf("unexpected EOF was marked completed: %s", raw)
		}
	})
}

func TestExecuteTraeCNRequestAggregatesNonStreamingResponses(t *testing.T) {
	t.Parallel()
	var requestBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != auth.TraeCNChatPath {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("x-ide-token") != "AT" {
			t.Errorf("unexpected auth headers: %#v", r.Header)
		}
		if got := r.Header.Get("User-Agent"); got != auth.TraeCNDefaultUserAgent {
			t.Errorf("User-Agent = %q, want %q (downstream UA must not leak)", got, auth.TraeCNDefaultUserAgent)
		}
		requestBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: output\ndata: {\"type\":\"text\",\"content\":\"hello\"}\n\nevent: token_usage\ndata: {\"prompt_tokens\":2,\"completion_tokens\":1}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
	}))
	defer server.Close()

	account := &auth.Account{
		DBID:         time.Now().UnixNano(),
		UpstreamType: auth.UpstreamTraeCN,
		AccessToken:  "AT",
		RefreshToken: "RT",
		ExpiresAt:    time.Now().Add(time.Hour),
		TraeCNHost:   server.URL,
	}
	inbound := []byte(`{"model":"DeepSeek-V4-Pro","input":"hi","stream":false}`)
	resp, err := ExecuteTraeCNRequest(t.Context(), account, GrokProtocolResponses, inbound, inbound, "", http.Header{"User-Agent": []string{"traecn-test"}})
	if err != nil {
		t.Fatalf("ExecuteTraeCNRequest() error = %v", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("Content-Type") != "application/json" || gjson.GetBytes(payload, "status").String() != "completed" {
		t.Fatalf("unexpected response: content-type=%q body=%s", resp.Header.Get("Content-Type"), payload)
	}
	if gjson.GetBytes(payload, "output.0.content.0.text").String() != "hello" || gjson.GetBytes(payload, "usage.total_tokens").Int() != 3 {
		t.Fatalf("unexpected aggregated response: %s", payload)
	}
	if gjson.GetBytes(requestBody, "model").String() != "DeepSeek-V4-Pro" || gjson.GetBytes(requestBody, "config_name").String() != "DeepSeek-V4-Pro" || !gjson.GetBytes(requestBody, "stream").Bool() {
		t.Fatalf("unexpected upstream request: %s", requestBody)
	}
}

// 内置兼容别名表已删除：默认把请求里的模型名原样发给 Trae，只有管理员在 TRAECN
// 设置里配置的映射才会改写上游模型名。
func TestTraeCNUpstreamModelUsesRequestedOrMappedName(t *testing.T) {
	previous := auth.ConfiguredTraeCNSettings()
	t.Cleanup(func() { auth.SetConfiguredTraeCNSettings(previous) })
	auth.SetConfiguredTraeCNSettings(auth.TraeCNSettings{})

	for _, model := range []string{"claude-opus-4-7", "deepseek-v3", "glm-5.2"} {
		body, got, err := buildTraeCNRequestBody([]byte(`{"model":"` + model + `","input":"hi"}`))
		if err != nil {
			t.Fatal(err)
		}
		if got != model || gjson.GetBytes(body, "model").String() != model {
			t.Fatalf("model %q was rewritten to %q: %s", model, gjson.GetBytes(body, "model").String(), body)
		}
	}

	// 管理员映射（对外名 -> 上游模型名）是唯一的改写来源。
	auth.SetConfiguredTraeCNSettings(auth.TraeCNSettings{ModelMapping: map[string]string{"claude-opus-4-7": "DeepSeek-V4-Pro"}})
	body, _, err := buildTraeCNRequestBody([]byte(`{"model":"claude-opus-4-7","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(body, "model").String(); got != "DeepSeek-V4-Pro" {
		t.Fatalf("configured mapping was not applied: %q", got)
	}
	// 未配置映射的模型不受影响。
	body, _, err = buildTraeCNRequestBody([]byte(`{"model":"glm-5.2","input":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(body, "model").String(); got != "glm-5.2" {
		t.Fatalf("unmapped model changed: %q", got)
	}
}

func TestExtractTraeCNModelIDsFromConfigInfoList(t *testing.T) {
	t.Parallel()
	body := []byte(`{
  "config_info_list": [
    {"config_name":"DeepSeek-V4-Pro","usage":"chat_completion","config_switch":true},
    {"config_name":"Doubao_1_6","usage":"chat_completion","config_switch":true},
    {"config_name":"Doubao-Seed-Code","usage":"chat_completion","config_switch":true},
    {"config_name":"custom_model_gpt-5","usage":"custom_model","config_switch":true},
    {"config_name":"glm-5.2_advisor_doubao","usage":"chat_completion","config_switch":true},
    {"config_name":"summary","usage":"summary","config_switch":true},
    {"config_name":"fast_apply","usage":"fast_apply","config_switch":true},
    {"config_name":"disabled-model","usage":"chat_completion","config_switch":false},
    {"config_name":"new-provider-model","usage":"chat_completion","config_switch":true},
    {"config_name":"custom_model_unknown","usage":"custom_model","config_switch":true}
  ]
}`)
	got := extractTraeCNModelIDs(body)
	joined := strings.Join(got, "\n")
	for _, want := range []string{"DeepSeek-V4-Pro", "Doubao_1_6", "Doubao-Seed-Code", "new-provider-model", "auto"} {
		if !modelIDInList(want, got) {
			t.Errorf("catalog missing %q: %v", want, got)
		}
	}
	for _, unwanted := range []string{"glm-5.2_advisor_doubao", "summary", "fast_apply", "disabled-model", "custom_model_unknown"} {
		if modelIDInList(unwanted, got) {
			t.Errorf("catalog unexpectedly contains %q: %s", unwanted, joined)
		}
	}
}

func TestExtractTraeCNModelIDsFromOpenAIModelsPayload(t *testing.T) {
	t.Parallel()
	body := []byte(`{"object":"list","data":[{"id":"deepseek-v3","object":"model"},{"id":"glm-5.2","object":"model"}]}`)
	got := extractTraeCNModelIDs(body)
	if !modelIDInList("deepseek-v3", got) || !modelIDInList("glm-5.2", got) || !modelIDInList("auto", got) {
		t.Fatalf("unexpected OpenAI catalog: %v", got)
	}
}

func TestTraeCNAPIKeyRoutesAllProtocolsToTraeUpstream(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name       string
		path       string
		model      string
		body       string
		invoke     func(*Handler, *gin.Context)
		wireModel  string
		outputMark string
	}{
		{
			name:       "responses",
			path:       "/v1/responses",
			model:      "DeepSeek-V4-Pro",
			body:       `{"model":"DeepSeek-V4-Pro","input":"hello","stream":true}`,
			invoke:     func(h *Handler, c *gin.Context) { h.Responses(c) },
			wireModel:  "DeepSeek-V4-Pro",
			outputMark: `"type":"response.output_text.delta"`,
		},
		{
			name:       "chat completions",
			path:       "/v1/chat/completions",
			model:      "DeepSeek-V4-Pro",
			body:       `{"model":"DeepSeek-V4-Pro","messages":[{"role":"user","content":"hello"}],"stream":true}`,
			invoke:     func(h *Handler, c *gin.Context) { h.ChatCompletions(c) },
			wireModel:  "DeepSeek-V4-Pro",
			outputMark: `"content":"ok"`,
		},
		{
			name:       "anthropic messages",
			path:       "/v1/messages",
			model:      "glm-5.3-flash",
			body:       `{"model":"glm-5.3-flash","max_tokens":64,"messages":[{"role":"user","content":"hello"}],"stream":true}`,
			invoke:     func(h *Handler, c *gin.Context) { h.Messages(c) },
			wireModel:  "glm-5.3-flash",
			outputMark: `"text":"ok"`,
		},
	}
	for index, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			var gotBody []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: output\ndata: {\"type\":\"text\",\"content\":\"ok\"}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
			}))
			defer upstream.Close()

			store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, MaxRetries: 0})
			defer store.Stop()
			store.AddAccount(&auth.Account{
				DBID:         int64(100 + index),
				UpstreamType: auth.UpstreamTraeCN,
				AccessToken:  "AT",
				RefreshToken: "RT",
				ExpiresAt:    time.Now().Add(time.Hour),
				TraeCNHost:   upstream.URL,
			})
			handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			ctx.Request.Header.Set("Content-Type", "application/json")
			ctx.Request.Header.Set("Authorization", "Bearer test-traecn-key")
			ctx.Set(contextAPIKeyRow, &database.APIKeyRow{ID: int64(200 + index), Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelTraeCN}})

			tc.invoke(handler, ctx)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
			}
			if gotPath != auth.TraeCNChatPath {
				t.Fatalf("upstream path = %q, want %q", gotPath, auth.TraeCNChatPath)
			}
			if got := gjson.GetBytes(gotBody, "model").String(); got != tc.wireModel {
				t.Fatalf("wire model = %q, want %q; body=%s", got, tc.wireModel, gotBody)
			}
			if strings.Contains(string(gotBody), tc.model) && tc.model != tc.wireModel {
				t.Fatalf("logical model leaked to provider body: %s", gotBody)
			}
			if !strings.Contains(recorder.Body.String(), tc.outputMark) || !strings.Contains(recorder.Body.String(), "ok") {
				t.Fatalf("converted %s response missing output: %s", tc.name, recorder.Body.String())
			}
		})
	}
}

func TestTraeCNAPIKeyRoutesNonStreamingChatAndMessages(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name        string
		path        string
		model       string
		body        string
		invoke      func(*Handler, *gin.Context)
		wireModel   string
		contentPath string
	}{
		{
			name:      "chat completions",
			path:      "/v1/chat/completions",
			model:     "DeepSeek-V4-Pro",
			body:      `{"model":"DeepSeek-V4-Pro","messages":[{"role":"user","content":"hello"}],"stream":false}`,
			invoke:    func(h *Handler, c *gin.Context) { h.ChatCompletions(c) },
			wireModel: "DeepSeek-V4-Pro", contentPath: "choices.0.message.content",
		},
		{
			name:      "anthropic messages",
			path:      "/v1/messages",
			model:     "glm-5.3-flash",
			body:      `{"model":"glm-5.3-flash","max_tokens":64,"messages":[{"role":"user","content":"hello"}],"stream":false}`,
			invoke:    func(h *Handler, c *gin.Context) { h.Messages(c) },
			wireModel: "glm-5.3-flash", contentPath: "content.0.text",
		},
	}
	for index, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var gotBody []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: output\ndata: {\"type\":\"text\",\"content\":\"ok\"}\n\nevent: token_usage\ndata: {\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
			}))
			defer upstream.Close()

			store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, MaxRetries: 0})
			defer store.Stop()
			store.AddAccount(&auth.Account{
				DBID: int64(300 + index), UpstreamType: auth.UpstreamTraeCN,
				AccessToken: "AT", RefreshToken: "RT", ExpiresAt: time.Now().Add(time.Hour), TraeCNHost: upstream.URL,
			})
			handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			ctx.Request.Header.Set("Content-Type", "application/json")
			ctx.Request.Header.Set("Authorization", "Bearer test-traecn-key")
			ctx.Set(contextAPIKeyRow, &database.APIKeyRow{ID: int64(400 + index), Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelTraeCN}})

			tc.invoke(handler, ctx)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
			}
			if got := gjson.GetBytes(gotBody, "model").String(); got != tc.wireModel {
				t.Fatalf("wire model = %q, want %q; body=%s", got, tc.wireModel, gotBody)
			}
			if got := gjson.GetBytes(recorder.Body.Bytes(), tc.contentPath).String(); got != "ok" {
				t.Fatalf("converted non-stream content = %q, want ok; body=%s", got, recorder.Body.String())
			}
			if tc.name == "chat completions" && gjson.GetBytes(recorder.Body.Bytes(), "usage.total_tokens").Int() != 3 {
				t.Fatalf("chat usage = %s, want total_tokens=3", gjson.GetBytes(recorder.Body.Bytes(), "usage").Raw)
			}
			if tc.name == "anthropic messages" && gjson.GetBytes(recorder.Body.Bytes(), "usage.output_tokens").Int() != 1 {
				t.Fatalf("anthropic usage = %s, want output_tokens=1", gjson.GetBytes(recorder.Body.Bytes(), "usage").Raw)
			}
		})
	}
}

// 取连耗时：TRAE 也必须记录"把账号变成可发请求"的花费，且不能把上游等首包的时间
// 算进去，否则 TRAE 的首字永远比 Codex 的含取连口径更难对比（issue #413 跟进）。
func TestTraeCNRequestRecordsAcquireWithoutUpstreamWait(t *testing.T) {
	const upstreamDelay = 300 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(upstreamDelay)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: output\ndata: {\"type\":\"text\",\"content\":\"hello\"}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
	}))
	defer server.Close()

	account := &auth.Account{
		DBID:         time.Now().UnixNano(),
		UpstreamType: auth.UpstreamTraeCN,
		AccessToken:  "AT",
		RefreshToken: "RT",
		ExpiresAt:    time.Now().Add(time.Hour),
		TraeCNHost:   server.URL,
	}
	ctx := withWsAcquireAudit(t.Context())
	inbound := []byte(`{"model":"DeepSeek-V4-Pro","input":"hi","stream":true}`)
	started := time.Now()
	resp, err := ExecuteTraeCNRequest(ctx, account, GrokProtocolResponses, inbound, inbound, "", http.Header{})
	if err != nil {
		t.Fatalf("ExecuteTraeCNRequest() error = %v", err)
	}
	defer resp.Body.Close()
	if elapsed := time.Since(started); elapsed < upstreamDelay {
		t.Fatalf("上游延迟未生效: %s", elapsed)
	}
	acquire := wsAcquireAuditTotal(ctx)
	if acquire <= 0 {
		t.Fatal("TRAE 取连耗时未记录")
	}
	if acquire >= upstreamDelay/2 {
		t.Fatalf("取连耗时把上游等待算进去了: %s", acquire)
	}
	if got := wsAcquireAuditMs(ctx); got != int(acquire.Milliseconds()) {
		t.Fatalf("ws_acquire_ms=%d 与取连总耗时 %s 不一致", got, acquire)
	}
}

// 每次 attempt 重新计时，避免前一次 attempt 的取连耗时累加到落库值上。
func TestTraeCNRequestResetsAcquireAuditPerAttempt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: output\ndata: {\"type\":\"text\",\"content\":\"hello\"}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
	}))
	defer server.Close()

	account := &auth.Account{
		DBID:         time.Now().UnixNano(),
		UpstreamType: auth.UpstreamTraeCN,
		AccessToken:  "AT",
		RefreshToken: "RT",
		ExpiresAt:    time.Now().Add(time.Hour),
		TraeCNHost:   server.URL,
	}
	ctx := withWsAcquireAudit(t.Context())
	AddWsAcquireDuration(ctx, 5*time.Second)
	inbound := []byte(`{"model":"DeepSeek-V4-Pro","input":"hi","stream":true}`)
	resp, err := ExecuteTraeCNRequest(ctx, account, GrokProtocolResponses, inbound, inbound, "", http.Header{})
	if err != nil {
		t.Fatalf("ExecuteTraeCNRequest() error = %v", err)
	}
	defer resp.Body.Close()
	if acquire := wsAcquireAuditTotal(ctx); acquire >= 5*time.Second {
		t.Fatalf("旧 attempt 的取连耗时被累加: %s", acquire)
	}
}

// Trae 的 4xx 常常只有十几个字节的纯文本；上层日志对非 JSON 体一律省略，导致这类
// 拒绝无法定位。断开续传的 409 被降级成真实错误之后，这一点必须先能看见。
func TestTraeCNUpstreamRejectLogsShortTextBody(t *testing.T) {
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	const rejectBody = "invalid request" // 15 字节，正是被省略的那种响应体
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, rejectBody)
	}))
	defer server.Close()

	account := &auth.Account{
		DBID:         time.Now().UnixNano(),
		UpstreamType: auth.UpstreamTraeCN,
		AccessToken:  "AT",
		RefreshToken: "RT",
		ExpiresAt:    time.Now().Add(time.Hour),
		TraeCNHost:   server.URL,
	}
	inbound := []byte(`{"model":"DeepSeek-V4-Pro","input":"hi","stream":true}`)
	resp, err := ExecuteTraeCNRequest(t.Context(), account, GrokProtocolResponses, inbound, inbound, "", http.Header{})
	if err != nil {
		t.Fatalf("ExecuteTraeCNRequest() error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	// 日志读取的是响应体前缀，必须原样放回给上层。
	payload, err := io.ReadAll(resp.Body)
	if err != nil || string(payload) != rejectBody {
		t.Fatalf("响应体被日志读取吃掉: %q err=%v", payload, err)
	}
	out := logs.String()
	for _, want := range []string{`stage=upstream_reject`, `status=400`, `body="invalid request"`, `model="DeepSeek-V4-Pro"`, `wire_bytes=`} {
		if !strings.Contains(out, want) {
			t.Fatalf("拒绝日志缺少 %q:\n%s", want, out)
		}
	}
}

// TRAECN「前置元数据立即下发」开关：默认关闭时上游 provider 通知被丢弃（既有行为，
// 字节级不变）；开启后它们作为 response.metadata 事件立即下发。
func TestTraeCNPreflightMetadataPassthrough(t *testing.T) {
	t.Parallel()
	provider := strings.Join([]string{
		`event: queue_begin`,
		`data: {"type":"queue_begin","data":{"position":3}}`,
		"",
		`event: progress_notice`,
		`data: {"type":"progress_notice","data":{"stage":"thinking"}}`,
		"",
		`event: metadata`,
		`data: {"type":"metadata","data":{"trace_id":"abc"}}`,
		"",
		`event: output`,
		`data: {"type":"text","content":"answer"}`,
		"",
		`event: done`,
		`data: {"type":"done","data":{"finish_reason":"stop"}}`,
		"",
	}, "\n")
	read := func(passthrough bool) ([]byte, []gjson.Result) {
		stream := traeCNCanonicalStreamWithPreflight(io.NopCloser(strings.NewReader(provider)), "deepseek-v3", nil, nil, passthrough)
		raw, err := io.ReadAll(stream)
		if err != nil {
			t.Fatalf("read canonical stream: %v", err)
		}
		return raw, canonicalSSEEvents(t, raw)
	}

	t.Run("off by default drops provider notices", func(t *testing.T) {
		raw, events := read(false)
		if _, ok := findCanonicalEvent(events, "response.metadata"); ok {
			t.Fatalf("disabled passthrough must not emit response.metadata: %s", raw)
		}
		if event, ok := findCanonicalEvent(events, "response.output_text.delta"); !ok || event.Get("delta").String() != "answer" {
			t.Fatalf("content missing: %s", raw)
		}
		if _, ok := findCanonicalEvent(events, "response.completed"); !ok {
			t.Fatalf("terminal missing: %s", raw)
		}
	})

	t.Run("enabled forwards real upstream notices before content", func(t *testing.T) {
		raw, events := read(true)
		metadata := make([]gjson.Result, 0, 3)
		firstContentIndex := -1
		for index, event := range events {
			switch event.Get("type").String() {
			case "response.metadata":
				metadata = append(metadata, event)
			case "response.output_text.delta":
				if firstContentIndex < 0 {
					firstContentIndex = index
				}
			}
		}
		if len(metadata) != 3 {
			t.Fatalf("metadata events = %d, want 3: %s", len(metadata), raw)
		}
		for index, want := range []string{"queue_begin", "progress_notice", "metadata"} {
			if got := metadata[index].Get("sse_event").String(); got != want {
				t.Fatalf("metadata[%d].sse_event = %q, want %q", index, got, want)
			}
			if !metadata[index].Get("metadata").IsObject() {
				t.Fatalf("metadata[%d] must carry the upstream payload: %s", index, metadata[index].Raw)
			}
		}
		if firstContentIndex < 0 || firstContentIndex < len(metadata) {
			t.Fatalf("notices must precede the first content event (content=%d metadata=%d): %s", firstContentIndex, len(metadata), raw)
		}
		if metadata[0].Get("metadata.data.position").Int() != 3 {
			t.Fatalf("queue payload not preserved: %s", metadata[0].Raw)
		}
		// 内容仍完整，开关只增加前置通知。
		if event, ok := findCanonicalEvent(events, "response.output_text.delta"); !ok || event.Get("delta").String() != "answer" {
			t.Fatalf("content missing: %s", raw)
		}
	})
}

// 开关的请求级快照优先于全局配置，保证一次请求内策略不随热更新切换。
func TestTraeCNPreflightPassthroughContextSnapshot(t *testing.T) {
	t.Parallel()
	previous := auth.ConfiguredTraeCNSettings()
	t.Cleanup(func() { auth.SetConfiguredTraeCNSettings(previous) })

	auth.SetConfiguredTraeCNSettings(auth.TraeCNSettings{PreflightSSEPassthrough: false})
	if traeCNPreflightPassthroughForContext(t.Context()) {
		t.Fatal("global default must be off")
	}
	if !traeCNPreflightPassthroughForContext(WithTraeCNPreflightPassthrough(t.Context(), true)) {
		t.Fatal("request snapshot must win over the global default")
	}

	// 反向：全局开启但本次请求固定关闭时，仍以请求快照为准。
	auth.SetConfiguredTraeCNSettings(auth.TraeCNSettings{PreflightSSEPassthrough: true})
	if !traeCNPreflightPassthroughForContext(t.Context()) {
		t.Fatal("global enable must be honored without a snapshot")
	}
	if traeCNPreflightPassthroughForContext(WithTraeCNPreflightPassthrough(t.Context(), false)) {
		t.Fatal("request snapshot must be able to disable passthrough")
	}
}

// withTraeCNPreflightPassthroughSnapshot 只在尚未固定时写入：断线续传 worker 在
// 受理任务时冻结的决策，不能被执行路径按当前全局配置覆盖。
func TestTraeCNPreflightSnapshotIsRetainedOnReapply(t *testing.T) {
	t.Parallel()
	previous := auth.ConfiguredTraeCNSettings()
	t.Cleanup(func() { auth.SetConfiguredTraeCNSettings(previous) })
	auth.SetConfiguredTraeCNSettings(auth.TraeCNSettings{PreflightSSEPassthrough: false})

	// 全局关闭、任务冻结为开启：执行路径再次经过时，覆盖写必须保留冻结值。
	frozen := WithTraeCNPreflightPassthrough(t.Context(), true)
	if !traeCNPreflightPassthroughForContext(frozen) {
		t.Fatal("frozen=true snapshot must be readable")
	}
	reapplied := withTraeCNPreflightPassthroughSnapshot(frozen, traeCNPreflightPassthroughForContext(t.Context()))
	if !traeCNPreflightPassthroughForContext(reapplied) {
		t.Fatal("reapplying with a different live value must not clobber the frozen snapshot")
	}

	// 没有快照时则写入——保证非续传路径仍能固定本次请求的决策。
	fresh := withTraeCNPreflightPassthroughSnapshot(t.Context(), true)
	if !traeCNPreflightPassthroughForContext(fresh) {
		t.Fatal("snapshot must be written when none is frozen yet")
	}
}

// 只有 response.metadata 在开关开启时绕开默认缓冲；生命周期帧与其余事件不受影响。
func TestForceFlushTraeCNPreflightMetadata(t *testing.T) {
	t.Parallel()
	cases := []struct {
		eventType   string
		passthrough bool
		want        bool
	}{
		{"response.metadata", true, true},
		{"response.metadata", false, false},
		{"response.created", true, false},
		{"response.in_progress", true, false},
		{"response.output_text.delta", true, false},
		{"response.failed", true, false},
	}
	for _, tc := range cases {
		if got := forceFlushTraeCNPreflightMetadata(tc.eventType, tc.passthrough); got != tc.want {
			t.Errorf("forceFlushTraeCNPreflightMetadata(%q, %t) = %t, want %t", tc.eventType, tc.passthrough, got, tc.want)
		}
	}
}

// TRAECN「前置元数据立即下发」必须真的在下游可见：上游在内容生成前发一条
// metadata、间隔后才发内容，metadata 必须早于首个内容帧到达下游，而不是被
// 首内容前的默认缓冲压到与内容同时下发。这条断言覆盖 handler 到转换器的整条
// 接线：只测转换器（见 TestTraeCNPreflightMetadataPassthrough）会漏掉
// 「快照写错分支 / 忘了强制冲刷」这类接线错误。
func TestTraeCNPreflightMetadataReachesClientBeforeContent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := auth.ConfiguredTraeCNSettings()
	t.Cleanup(func() { auth.SetConfiguredTraeCNSettings(previous) })
	auth.SetConfiguredTraeCNSettings(auth.TraeCNSettings{PreflightSSEPassthrough: true})

	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: metadata\ndata: {\"type\":\"metadata\",\"data\":{\"trace_id\":\"abc\"}}\n\n")
		flusher.Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "event: output\ndata: {\"type\":\"text\",\"content\":\"answer\"}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, MaxRetries: 0})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{
		DBID: 91950, UpstreamType: auth.UpstreamTraeCN,
		AccessToken: "AT", RefreshToken: "RT", ExpiresAt: time.Now().Add(time.Hour),
		TraeCNHost: upstream.URL,
	})
	handler := NewHandler(store, nil, nil, nil)
	t.Cleanup(func() { cleanupTraeCNResumeRegistry(t, &handler.traeCNResumeTasks) })

	router := gin.New()
	router.POST("/v1/responses", func(c *gin.Context) {
		c.Set(contextAPIKeyID, int64(91950))
		c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 91950, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelTraeCN}})
		handler.Responses(c)
	})
	server := httptest.NewServer(router)
	defer server.Close()

	// 内容在 metadata 之后放行：若 metadata 被缓冲，它只会与内容同时出现。
	time.AfterFunc(1200*time.Millisecond, func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses",
		strings.NewReader(`{"model":"DeepSeek-V4-Pro","stream":true,"input":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer traecn-preflight-key")
	req.Header.Set("Content-Type", "application/json")

	type frame struct {
		at     time.Duration
		detail string
	}
	frames := make(chan frame, 64)
	start := time.Now()
	go func() {
		resp, err := server.Client().Do(req)
		if err != nil {
			close(frames)
			return
		}
		defer resp.Body.Close()
		frames <- frame{at: time.Since(start), detail: "headers"}
		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				close(frames)
				return
			}
			trimmed := strings.TrimRight(line, "\r\n")
			if !strings.HasPrefix(trimmed, "data: ") {
				continue
			}
			payload := strings.TrimSpace(trimmed[6:])
			frames <- frame{at: time.Since(start), detail: gjson.Get(payload, "type").String()}
		}
	}()

	metadataAt := time.Duration(-1)
	contentAt := time.Duration(-1)
	for f := range frames {
		switch f.detail {
		case "response.metadata":
			if metadataAt < 0 {
				metadataAt = f.at
			}
		case "response.output_text.delta":
			if contentAt < 0 {
				contentAt = f.at
			}
		}
	}

	if metadataAt < 0 {
		t.Fatal("enabled passthrough must deliver response.metadata downstream")
	}
	if contentAt < 0 {
		t.Fatal("content never reached downstream")
	}
	// 内容被压后 1.2s；metadata 若与内容同时到达即说明它被缓冲，开关失效。
	if metadataAt >= contentAt-500*time.Millisecond {
		t.Fatalf("metadata must be dispatched before content (metadata=%s content=%s); it was buffered", metadataAt, contentAt)
	}
}
