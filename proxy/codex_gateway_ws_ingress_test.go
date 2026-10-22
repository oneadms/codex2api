package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestGatewayWSNativeIngressNeverReplaysFixedContinuation(t *testing.T) {
	for _, failure := range []string{"continuation_lost", "fixed_error", "upstream_not_found", "message_too_big"} {
		t.Run(failure, func(t *testing.T) {
			m, source, target := newGatewayWSUnitFixture(t)
			m.Store.ApplyAccountEnabled(source.ID(), false)
			target.Mu().Lock()
			target.CodexTurnState = "offline-manual-state"
			target.Mu().Unlock()
			resetResponseCacheForTest()
			oldExecute := WebsocketExecuteFunc
			globalWSSizeRouter.mu.Lock()
			oldMin, oldLearned := globalWSSizeRouter.minTooBig, globalWSSizeRouter.learnedAt
			globalWSSizeRouter.minTooBig, globalWSSizeRouter.learnedAt = 0, time.Time{}
			globalWSSizeRouter.mu.Unlock()
			t.Cleanup(func() {
				WebsocketExecuteFunc = oldExecute
				globalWSSizeRouter.mu.Lock()
				globalWSSizeRouter.minTooBig, globalWSSizeRouter.learnedAt = oldMin, oldLearned
				globalWSSizeRouter.mu.Unlock()
				resetResponseCacheForTest()
			})
			var attempts atomic.Int32
			WebsocketExecuteFunc = func(ctx context.Context, a *auth.Account, body []byte, session, proxyURL, key string, cfg *DeviceProfileConfig, headers http.Header, pool string) (*http.Response, error) {
				n := attempts.Add(1)
				if n == 1 {
					sse := wsContextTestSSE("fixed_seed", `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first"}]}`)
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(sse))}, nil
				}
				if gjson.GetBytes(body, "previous_response_id").String() != "fixed_seed" {
					t.Error("fixed continuation was replayed without its ID")
				}
				switch failure {
				case "continuation_lost":
					return nil, &ResponsesContinuationLostError{Reason: "original_connection_unavailable"}
				case "fixed_error":
					return nil, &CodexGatewayWSError{Reason: "original_connection_unavailable"}
				case "message_too_big":
					return nil, errors.New("websocket: close 1009 (message too big)")
				default:
					return &http.Response{StatusCode: 400, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":"previous_response_not_found","message":"not found"}}`))}, nil
				}
			}
			gin.SetMode(gin.TestMode)
			handler := NewHandler(m.Store, nil, &config.Config{AllowAnonymousV1: true}, nil)
			router := gin.New()
			handler.RegisterRoutes(router)
			server := httptest.NewServer(router)
			t.Cleanup(server.Close)
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Session-Id": {"native-fixed"}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			for i, body := range []string{
				`{"type":"response.create","model":"gpt-6-astra","input":"first","prompt_cache_key":"native-fixed"}`,
				`{"type":"response.create","model":"gpt-6-astra","input":"next","prompt_cache_key":"native-fixed","previous_response_id":"fixed_seed"}`,
			} {
				if err := conn.WriteMessage(websocket.TextMessage, []byte(body)); err != nil {
					t.Fatal(err)
				}
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				terminal := readResponsesWSTerminalEvent(t, conn)
				if i == 0 && gjson.GetBytes(terminal, "type").String() != "response.completed" {
					t.Fatalf("first turn failed: %s", terminal)
				}
				if i == 1 && gjson.GetBytes(terminal, "type").String() == "response.completed" {
					t.Fatalf("lost continuation was replayed: %s", terminal)
				}
			}
			if attempts.Load() != 2 {
				t.Fatalf("fixed continuation retried: %d attempts", attempts.Load())
			}
		})
	}
}
