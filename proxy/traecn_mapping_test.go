package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// 从 HTTP 入口验证映射和工具回传，防止只修转换函数却遗漏选号或模型校验。
func TestTraeCNModelMappingAllProtocols(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := auth.ConfiguredTraeCNSettings()
	t.Cleanup(func() { auth.SetConfiguredTraeCNSettings(previous) })
	auth.SetConfiguredTraeCNSettings(auth.TraeCNSettings{ModelMapping: map[string]string{"my-code": "doubao-seed-code", "claude-opus-4-6": "doubao-seed-code"}})
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			path   string
			body   string
			invoke func(*Handler, *gin.Context)
		}{
			{"/v1/responses", `{"input":"read file"}`, (*Handler).Responses},
			{"/v1/chat/completions", `{"messages":[{"role":"user","content":"read file"}]}`, (*Handler).ChatCompletions},
			{"/v1/messages", `{"max_tokens":64,"messages":[{"role":"user","content":"read file"}]}`, (*Handler).Messages},
		} {
			for _, model := range []string{"my-code", "claude-opus-4-6"} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", tc.path, model, stream), func(t *testing.T) {
					resetResponseCacheStateForTest(testResponseCacheConfig())
					t.Cleanup(func() { resetResponseCacheStateForTest(defaultResponseCacheConfig()) })
					upstreamCalls := 0
					handler := newTraeCNContextTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
						upstreamCalls++
						body, _ := io.ReadAll(r.Body)
						if gjson.GetBytes(body, "model").String() != "Doubao_1_6" {
							t.Errorf("wrong upstream model: %s", body)
						}
						w.Header().Set("Content-Type", "text/event-stream")
						io.WriteString(w, "event: tool_call\ndata: {\"index\":0,\"id\":\"call_read\",\"function\":{\"name\":\"read_file\"}}\n\nevent: output\ndata: {\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"path\\\":\\\"a.go\\\"}\"}}]}\n\nevent: done\ndata: {\"finish_reason\":\"tool_calls\"}\n\n")
					})
					handler.store.SetCodexModelMapping(`{"my-code":"gpt-5.5","claude-opus-4-6":"gpt-5.5"}`)
					handler.store.SetModelMapping(`{"my-code":"gpt-5.5","claude-opus-4-6":"gpt-5.5"}`)
					body := strings.TrimSuffix(tc.body, "}") + fmt.Sprintf(`,"model":%q,"stream":%t}`, model, stream)
					recorder := httptest.NewRecorder()
					ctx, _ := gin.CreateTestContext(recorder)
					ctx.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
					ctx.Request.Header.Set("Content-Type", "application/json")
					ctx.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 91088, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelTraeCN}})
					tc.invoke(handler, ctx)
					if recorder.Code != http.StatusOK || upstreamCalls != 1 || !strings.Contains(recorder.Body.String(), `"read_file"`) || strings.Contains(recorder.Body.String(), "malformed_tool_call") {
						t.Fatalf("status=%d calls=%d body=%s", recorder.Code, upstreamCalls, recorder.Body.String())
					}
					if !strings.Contains(recorder.Body.String(), `"model":"`+model+`"`) {
						t.Fatalf("public model not preserved: %s", recorder.Body.String())
					}
				})
			}
		}
	}
}

func TestTraeCNModelMappingCatalogAndChannelIsolation(t *testing.T) {
	previous := auth.ConfiguredTraeCNSettings()
	t.Cleanup(func() { auth.SetConfiguredTraeCNSettings(previous) })
	auth.SetConfiguredTraeCNSettings(auth.TraeCNSettings{ModelMapping: map[string]string{"my-code": "doubao-seed-code", "gpt-5.5": "doubao-seed-code"}})
	handler := newTraeCNContextTestHandler(t, func(w http.ResponseWriter, r *http.Request) { t.Error("catalog lookup must not contact upstream") })
	if !modelIDInList("my-code", handler.traeCNChannelModels()) {
		t.Fatal("alias missing from TRAECN catalog")
	}
	for _, channel := range []string{database.UpstreamChannelTraeCN, database.UpstreamChannelCodex} {
		records := handler.scopedModelRecords(t.Context(), &database.APIKeyRow{ID: 91088, Limits: database.APIKeyLimits{UpstreamChannel: channel}})
		_, found := records["my-code"]
		if found != (channel == database.UpstreamChannelTraeCN) {
			t.Errorf("alias leaked or missing for channel=%s: %v", channel, records)
		}
	}
	body, _, model, _ := handler.applyConfiguredModelMappingToBody([]byte(`{"model":"gpt-5.5","input":"hello"}`), []string{"gpt-5.5"})
	if model != "gpt-5.5" || gjson.GetBytes(body, "model").String() != "gpt-5.5" {
		t.Fatalf("TRAE mapping changed Codex: %s", body)
	}
}
