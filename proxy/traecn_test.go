package proxy

import (
	"io"
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
	if root.Get("config_name").Exists() {
		t.Fatalf("config_name must not be sent: %s", body)
	}
	if root.Get("function").String() != "chat_v3" || !root.Get("stream").Bool() {
		t.Fatalf("unexpected request mode: %s", body)
	}
	if root.Get("messages.0.role").String() != "system" || root.Get("messages.0.content.0.text").String() != "follow the system instruction" {
		t.Fatalf("instructions were not preserved: %s", body)
	}
	if root.Get("messages.1.content.1.image_url.url").String() != "https://example.test/image.png" {
		t.Fatalf("image URL was not preserved: %s", body)
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
		if r.Header.Get("Authorization") != "Cloud-IDE-JWT AT" || r.Header.Get("X-Cloudide-Token") != "AT" {
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
	inbound := []byte(`{"model":"deepseek-v3","input":"hi","stream":false}`)
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
	if gjson.GetBytes(requestBody, "model").String() != "deepseek-v3" || gjson.GetBytes(requestBody, "config_name").Exists() || !gjson.GetBytes(requestBody, "stream").Bool() {
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
	for _, want := range []string{"deepseek-v4-pro", "doubao-1-6", "doubao-seed-code", "new-provider-model", "auto"} {
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
			model:      "deepseek-v3",
			body:       `{"model":"deepseek-v3","input":"hello","stream":true}`,
			invoke:     func(h *Handler, c *gin.Context) { h.Responses(c) },
			wireModel:  "deepseek-v3",
			outputMark: `"type":"response.output_text.delta"`,
		},
		{
			name:       "chat completions",
			path:       "/v1/chat/completions",
			model:      "deepseek-v3",
			body:       `{"model":"deepseek-v3","messages":[{"role":"user","content":"hello"}],"stream":true}`,
			invoke:     func(h *Handler, c *gin.Context) { h.ChatCompletions(c) },
			wireModel:  "deepseek-v3",
			outputMark: `"content":"ok"`,
		},
		{
			name:       "anthropic messages",
			path:       "/v1/messages",
			model:      "claude-opus-4-7",
			body:       `{"model":"claude-opus-4-7","max_tokens":64,"messages":[{"role":"user","content":"hello"}],"stream":true}`,
			invoke:     func(h *Handler, c *gin.Context) { h.Messages(c) },
			wireModel:  "claude-opus-4-7",
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
			model:     "deepseek-v3",
			body:      `{"model":"deepseek-v3","messages":[{"role":"user","content":"hello"}],"stream":false}`,
			invoke:    func(h *Handler, c *gin.Context) { h.ChatCompletions(c) },
			wireModel: "deepseek-v3", contentPath: "choices.0.message.content",
		},
		{
			name:      "anthropic messages",
			path:      "/v1/messages",
			model:     "claude-opus-4-7",
			body:      `{"model":"claude-opus-4-7","max_tokens":64,"messages":[{"role":"user","content":"hello"}],"stream":false}`,
			invoke:    func(h *Handler, c *gin.Context) { h.Messages(c) },
			wireModel: "claude-opus-4-7", contentPath: "content.0.text",
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
