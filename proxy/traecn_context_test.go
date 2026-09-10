package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func newTraeCNContextTestHandler(t *testing.T, serve http.HandlerFunc) *Handler {
	t.Helper()
	upstream := httptest.NewServer(serve)
	t.Cleanup(upstream.Close)
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, MaxRetries: 0})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{
		DBID: 91080, UpstreamType: auth.UpstreamTraeCN,
		AccessToken: "AT", RefreshToken: "RT", ExpiresAt: time.Now().Add(time.Hour),
		TraeCNHost: upstream.URL,
	})
	return NewHandler(store, nil, nil, nil)
}

func invokeTraeCNContextTestRequest(t *testing.T, handler *Handler, keyID int64, channel string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return invokeResponsesHandlerWithContext(t, func(c *gin.Context) {
		c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: keyID, Limits: database.APIKeyLimits{UpstreamChannel: channel}})
	}, handler.Responses, raw)
}

func traeCNContextTestResponse(t *testing.T, recorder *httptest.ResponseRecorder, stream bool) gjson.Result {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	response := gjson.ParseBytes(recorder.Body.Bytes())
	if stream {
		event, ok := findCanonicalEvent(canonicalSSEEvents(t, recorder.Body.Bytes()), "response.completed")
		if !ok {
			t.Fatalf("missing completed response: %s", recorder.Body.String())
		}
		response = event.Get("response")
	}
	if response.Get("id").String() == "" {
		t.Fatalf("missing response ID: %s", recorder.Body.String())
	}
	return response
}

// 连续三轮只发送新增输入，验证请求真正到达上游时仍包含双方的全部前文。
func TestTraeCNResponsesContinuationPreservesConversation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, stream := range []bool{false, true} {
		for _, channel := range []string{database.UpstreamChannelTraeCN, ""} {
			t.Run(fmt.Sprintf("stream=%t/channel=%s", stream, channel), func(t *testing.T) {
				resetResponseCacheStateForTest(testResponseCacheConfig())
				t.Cleanup(func() { resetResponseCacheStateForTest(defaultResponseCacheConfig()) })
				var seenBodies [][]byte
				handler := newTraeCNContextTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					seenBodies = append(seenBodies, body)
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "event: output\ndata: {\"type\":\"text\",\"content\":\"reply %d\"}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n", len(seenBodies))
				})
				previousID := ""
				for turn := 1; turn <= 3; turn++ {
					request := map[string]any{"model": "deepseek-v3", "stream": stream, "input": fmt.Sprintf("question %d", turn)}
					if previousID != "" {
						request["previous_response_id"] = previousID
					}
					response := traeCNContextTestResponse(t, invokeTraeCNContextTestRequest(t, handler, 91081, channel, request), stream)
					previousID = response.Get("id").String()
					messages := gjson.GetBytes(seenBodies[turn-1], "messages").Array()
					if len(messages) != turn*2-1 {
						t.Fatalf("turn %d sent %d messages, want %d: %s", turn, len(messages), turn*2-1, seenBodies[turn-1])
					}
					for index, message := range messages {
						role, prefix := "user", "question"
						if index%2 == 1 {
							role, prefix = "assistant", "reply"
						}
						wantText := fmt.Sprintf("%s %d", prefix, index/2+1)
						if message.Get("role").String() != role || message.Get("content.0.text").String() != wantText {
							t.Fatalf("turn %d message %d = %s, want %s %q", turn, index, message.Raw, role, wantText)
						}
					}
				}
			})
		}
	}
}

func TestTraeCNResponsesMissingContextDoesNotSilentlyStartNewConversation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetResponseCacheStateForTest(testResponseCacheConfig())
	t.Cleanup(func() { resetResponseCacheStateForTest(defaultResponseCacheConfig()) })
	calls := 0
	handler := newTraeCNContextTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: output\ndata: {\"type\":\"text\",\"content\":\"lost context\"}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
	})
	recorder := invokeTraeCNContextTestRequest(t, handler, 91082, database.UpstreamChannelTraeCN, map[string]any{
		"model": "deepseek-v3", "previous_response_id": "resp_unavailable", "input": "继续上面的方案", "stream": false,
	})
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "response_context_unavailable") {
		t.Fatalf("status = %d, want context error: %s", recorder.Code, recorder.Body.String())
	}
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want 0 without prior context", calls)
	}
}

func TestTraeCNResponsesToolContinuationPreservesEarlierMessages(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, echoCall := range []bool{false, true} {
		t.Run(fmt.Sprintf("echo_call=%t", echoCall), func(t *testing.T) {
			resetResponseCacheStateForTest(testResponseCacheConfig())
			t.Cleanup(func() { resetResponseCacheStateForTest(defaultResponseCacheConfig()) })
			var seenBodies [][]byte
			handler := newTraeCNContextTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				seenBodies = append(seenBodies, body)
				w.Header().Set("Content-Type", "text/event-stream")
				if len(seenBodies) == 1 {
					io.WriteString(w, "event: output\ndata: {\"type\":\"text\",\"content\":\"checking the project\",\"tool_calls\":[{\"id\":\"call_lookup\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]}\n\nevent: done\ndata: {\"finish_reason\":\"tool_calls\"}\n\n")
				} else {
					io.WriteString(w, "event: output\ndata: {\"type\":\"text\",\"content\":\"done\"}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
				}
			})
			first := traeCNContextTestResponse(t, invokeTraeCNContextTestRequest(t, handler, 91083, database.UpstreamChannelTraeCN, map[string]any{
				"model": "deepseek-v3", "input": "project is cedar", "stream": true,
			}), true)
			var input []any
			if echoCall {
				input = append(input, map[string]any{"type": "function_call", "call_id": "call_lookup", "name": "lookup", "arguments": "{}"})
			}
			input = append(input, map[string]any{"type": "function_call_output", "call_id": "call_lookup", "output": "found cedar"})
			traeCNContextTestResponse(t, invokeTraeCNContextTestRequest(t, handler, 91083, database.UpstreamChannelTraeCN, map[string]any{
				"model": "deepseek-v3", "previous_response_id": first.Get("id").String(), "input": input, "stream": false,
			}), false)
			messages := gjson.GetBytes(seenBodies[1], "messages").Array()
			if len(messages) != 4 || messages[0].Get("content.0.text").String() != "project is cedar" ||
				messages[1].Get("content.0.text").String() != "checking the project" ||
				messages[2].Get("tool_calls.0.id").String() != "call_lookup" ||
				messages[3].Get("tool_call_id").String() != "call_lookup" {
				t.Fatalf("tool continuation lost or duplicated prior context: %s", seenBodies[1])
			}
		})
	}
}

func TestTraeCNResponseContextBoundsAndIsolation(t *testing.T) {
	for _, scenario := range []string{"on_demand", "long_history", "different_owner", "oversize", "expired", "incomplete"} {
		t.Run(scenario, func(t *testing.T) {
			config := testResponseCacheConfig()
			if scenario == "on_demand" {
				config.writePolicy = database.ResponseCacheWritePolicyOnDemand
			}
			if scenario == "oversize" {
				config.maxEntryBytes = 1
			}
			resetResponseCacheStateForTest(config)
			t.Cleanup(func() { resetResponseCacheStateForTest(defaultResponseCacheConfig()) })
			input := []any{map[string]any{"role": "user", "content": "remember this first message"}}
			if scenario == "long_history" {
				for index := 0; index < responseCacheMaxPerItem+1; index++ {
					input = append(input, map[string]any{"role": "user", "content": fmt.Sprintf("message %d", index)})
				}
			}
			request, _ := json.Marshal(map[string]any{"input": input})
			response := []byte(`{"id":"resp_context","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"remembered"}]}]}`)
			if scenario == "incomplete" {
				response = []byte(strings.ReplaceAll(string(response), `"completed"`, `"incomplete"`))
			}
			cacheTraeCNResponseContext("key:91084", request, response)
			if scenario == "expired" {
				respCache.mu.Lock()
				respCache.store[responseCacheStoreKey(traeCNResponseCacheOwner("key:91084"), "resp_context")].expiresAt = time.Now().Add(-time.Second)
				respCache.mu.Unlock()
			}
			owner := "key:91084"
			if scenario == "different_owner" {
				owner = "key:91085"
			}
			prepared := prepareTraeCNResponsesContext([]byte(`{"model":"deepseek-v3","previous_response_id":"resp_context","input":"continue"}`), owner)
			status, _, unavailable := responseCachePreparationFailure(prepared)
			if scenario == "oversize" || scenario == "different_owner" || scenario == "expired" || scenario == "incomplete" {
				if !unavailable || status != http.StatusConflict {
					t.Fatalf("status=%d unavailable=%t, want 409", status, unavailable)
				}
				return
			}
			if unavailable || prepared.CacheLookup.Kind != responseCacheLookupHit {
				t.Fatalf("history unavailable: status=%d kind=%v", status, prepared.CacheLookup.Kind)
			}
			if got := gjson.GetBytes(prepared.Body, "input.#").Int(); got != int64(len(input)+2) {
				t.Fatalf("retained %d items, want %d", got, len(input)+2)
			}
			if gjson.GetBytes(prepared.Body, "input.0.content").String() != "remember this first message" {
				t.Fatalf("oldest message was dropped: %s", prepared.Body)
			}
		})
	}
}

func TestTraeCNResponseContextSharedBackend(t *testing.T) {
	for _, scenario := range []string{"hit", "corrupt", "unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			resetResponseCacheStateForTest(testResponseCacheConfig())
			backend := newRecordingResponseContextBackend(true)
			SetResponseContextCache(backend)
			t.Cleanup(func() {
				resetResponseCacheStateForTest(defaultResponseCacheConfig())
				backend.TokenCache.Close()
			})
			cacheTraeCNResponseContext("key:91086", []byte(`{"input":[{"role":"user","content":"first question"}]}`),
				[]byte(`{"id":"resp_shared","status":"completed","output":[{"type":"message","role":"assistant","content":"first answer"}]}`))
			drainResponseCacheBackendWrites()
			backend.mu.Lock()
			items := backend.writes[responseCacheStoreKey(traeCNResponseCacheOwner("key:91086"), "resp_shared")]
			backend.mu.Unlock()
			if len(items) != 1 {
				t.Fatalf("shared backend did not receive complete history: %v", items)
			}
			// 清空本机缓存，模拟下一轮请求落到另一个实例。
			resetResponseCacheStateForTest(testResponseCacheConfig())
			SetResponseContextCache(backend)
			backend.bounded = cache.ResponseContextReadResult{Status: cache.ResponseContextReadFound, Items: items}
			wantStatus := 0
			if scenario == "corrupt" {
				backend.bounded.Items = []json.RawMessage{json.RawMessage(`{"unexpected":"format"}`)}
				wantStatus = http.StatusConflict
			} else if scenario == "unavailable" {
				backend.boundedErr = errSyntheticBackend
				wantStatus = http.StatusServiceUnavailable
			}
			prepared := prepareTraeCNResponsesContext([]byte(`{"model":"deepseek-v3","previous_response_id":"resp_shared","input":"second question"}`), "key:91086")
			status, _, _ := responseCachePreparationFailure(prepared)
			if status != wantStatus {
				t.Fatalf("status=%d, want %d", status, wantStatus)
			}
			if scenario == "hit" && (gjson.GetBytes(prepared.Body, "input.#").Int() != 3 ||
				gjson.GetBytes(prepared.Body, "input.0.content").String() != "first question" ||
				gjson.GetBytes(prepared.Body, "input.1.content").String() != "first answer") {
				t.Fatalf("shared replay lost conversation: %s", prepared.Body)
			}
		})
	}
}

func TestTraeCNProtocolsPreserveExplicitConversation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		path   string
		body   string
		invoke func(*Handler, *gin.Context)
	}{
		{"/v1/responses", `{"model":"deepseek-v3","instructions":"system rule","input":[{"role":"user","content":"same question"},{"role":"assistant","content":"earlier answer"},{"role":"user","content":"same question"}]}`, (*Handler).Responses},
		{"/v1/chat/completions", `{"model":"deepseek-v3","messages":[{"role":"system","content":"system rule"},{"role":"user","content":"same question"},{"role":"assistant","content":"earlier answer"},{"role":"user","content":"same question"}]}`, (*Handler).ChatCompletions},
		{"/v1/messages", `{"model":"deepseek-v3","max_tokens":64,"system":"system rule","messages":[{"role":"user","content":"same question"},{"role":"assistant","content":"earlier answer"},{"role":"user","content":"same question"}]}`, (*Handler).Messages},
	} {
		t.Run(tc.path, func(t *testing.T) {
			resetResponseCacheStateForTest(testResponseCacheConfig())
			t.Cleanup(func() { resetResponseCacheStateForTest(defaultResponseCacheConfig()) })
			var seenBody []byte
			handler := newTraeCNContextTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
				seenBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "event: output\ndata: {\"type\":\"text\",\"content\":\"ok\"}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
			})
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			ctx.Request.Header.Set("Content-Type", "application/json")
			ctx.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 91087, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelTraeCN}})
			tc.invoke(handler, ctx)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d: %s", recorder.Code, recorder.Body.String())
			}
			messages := gjson.GetBytes(seenBody, "messages").Array()
			wantRoles := []string{"system", "user", "assistant", "user"}
			wantText := []string{"system rule", "same question", "earlier answer", "same question"}
			if len(messages) != len(wantRoles) {
				t.Fatalf("history was dropped: %s", seenBody)
			}
			for index, message := range messages {
				if message.Get("role").String() != wantRoles[index] || message.Get("content.0.text").String() != wantText[index] {
					t.Fatalf("message %d changed: %s", index, seenBody)
				}
			}
		})
	}
}
