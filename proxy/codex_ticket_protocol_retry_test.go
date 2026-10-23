package proxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/harvest"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestCodexTicketUnavailableProtocolError(t *testing.T) {
	for _, websocket := range []bool{false, true} {
		for _, model := range []string{"", " gpt-6-astra "} {
			err := ErrCodexTicketUnavailable(model, websocket)
			protocol := "SSE"
			if websocket {
				protocol = "WebSocket"
			}
			if err.Code != ErrorCodeCodexTicketUnavailable || err.HTTPStatus != http.StatusServiceUnavailable || !err.Retryable || err.Type != ErrorTypeServerError || !strings.Contains(err.Message, protocol) {
				t.Fatalf("unexpected local admission error: %+v", err)
			}
			if model != "" && !strings.Contains(err.Message, `"gpt-6-astra"`) {
				t.Fatalf("model not normalized: %s", err.Message)
			}
			apiErr := api.NewAPIError(api.ErrorCode(err.Code), err.Message, api.ErrorTypeServer)
			if responsesWSErrorStatus(apiErr) != http.StatusServiceUnavailable {
				t.Fatal("local ticket error lost its 503 status")
			}
		}
	}
}

func TestNativeWSTicketProtocolFailureIsNotSelectionTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name                                                           string
		alternative, alternativeSSE, fixed, continuation, disableRetry bool
	}{
		{name: "SSE ticket only"},
		{name: "another account has WS ticket", alternative: true},
		{name: "all accounts have only SSE tickets", alternative: true, alternativeSSE: true},
		{name: "fixed session cannot switch accounts", alternative: true, fixed: true},
		{name: "pinned continuation cannot switch accounts", alternative: true, continuation: true},
		{name: "silent retry disabled", alternative: true, disableRetry: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withCodexTicketGate(t, gatewayModel)
			settings := auth.ConfiguredCodexTicketSettings()
			settings.TargetLength = 780
			auth.SetConfiguredCodexTicketSettings(settings)
			store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, FastSchedulerEnabled: true})
			t.Cleanup(store.Stop)
			newAccount := func(id int64, protocol string) *auth.Account {
				a := &auth.Account{DBID: id, AccountID: fmt.Sprint(id), AccessToken: "offline-token", PlanType: "plus", Status: auth.StatusReady}
				now := time.Now()
				claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, now.Add(time.Hour).Unix())))
				state := testTicketState(now, 33)
				a.CodexTickets = map[string]*auth.CodexTicket{gatewayModel: {
					Model: gatewayModel, State: state, Length: len(state), Cookie: "__cflb=offline; __oailb=h." + claims + ".s",
					ProbeVerified: true, Transport: protocol, Identity: gatewayIdentity(a),
					IssuedAt: now, CapturedAt: now, CookieCapturedAt: now, ExpiresAt: now.Add(2 * time.Minute),
					HarvestSessionID: "offline-session",
				}}
				store.AddAccount(a)
				if a.CodexTicketForModel(gatewayModel, now, 780, protocol) == nil {
					t.Fatal("fixture ticket is invalid")
				}
				return a
			}
			sseAccount := newAccount(1, "sse")
			var alternative *auth.Account
			if tc.alternative {
				protocol := "websocket"
				if tc.alternativeSSE {
					protocol = "sse"
				}
				alternative = newAccount(2, protocol)
			}
			store.BindSessionAffinity(sessionAffinityKey("protocol-session", 0), sseAccount, "")
			previousManager := codexHarvester.Swap(nil)
			t.Cleanup(func() { codexHarvester.Store(previousManager) })
			if tc.fixed {
				m := &CodexHarvestManager{Store: store}
				g := m.gatewayManager()
				v := harvest.DefaultGatewaySettings()
				v.CookiePool.Enabled = true
				v.CookiePool.SourceAccountIDs, v.CookiePool.TargetAccountIDs = []int64{99}, []int64{1}
				v.WSSession.Enabled, v.WSSession.AccountIDs = true, []int64{1}
				g.settings, g.loaded, g.loadedUntil = v, true, time.Now().Add(time.Hour)
				codexHarvester.Store(m)
			}
			oldRuntime, oldExecute, oldWait := CurrentRuntimeSettings(), WebsocketExecuteFunc, dispatchAccountWaitTimeout
			t.Cleanup(func() {
				ApplyRuntimeSettings(oldRuntime)
				WebsocketExecuteFunc = oldExecute
				dispatchAccountWaitTimeout = oldWait
			})
			// Keep the production selection deadline: the client deadline below
			// proves that exhausting locally rejected accounts never enters it.
			dispatchAccountWaitTimeout = accountSelectionTimeout + time.Second
			var calls atomic.Int32
			WebsocketExecuteFunc = func(ctx context.Context, a *auth.Account, _ []byte, _, _, _ string, _ *DeviceProfileConfig, _ http.Header, _ string) (*http.Response, error) {
				calls.Add(1)
				if a.ID() != 2 || codexTicketFromContext(ctx) == nil || codexTicketFromContext(ctx).Transport != "websocket" {
					t.Error("SSE-only or ticketless account reached the WS transport")
				}
				return nativeWSResponse("protocol-ok"), nil
			}
			h := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
			runtime := DefaultRuntimeSettings()
			runtime.CodexWSSilentRetry, runtime.CodexWSSilentRetries = !tc.disableRetry, 2
			runtime.CodexWSHideErrors = true
			ApplyRuntimeSettings(runtime)
			router := gin.New()
			h.RegisterRoutes(router)
			server := httptest.NewServer(router)
			defer server.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Session-Id": {"protocol-session"}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			body := `{"type":"response.create","model":"gpt-6-astra","input":"offline"}`
			if tc.continuation {
				body = `{"type":"response.create","model":"gpt-6-astra","input":"offline","client_metadata":{"x-codex-turn-state":"offline-continuation"}}`
			}
			if err = conn.WriteMessage(websocket.TextMessage, []byte(body)); err != nil {
				t.Fatal(err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			terminal := readResponsesWSTerminalEvent(t, conn)
			if tc.alternative && !tc.alternativeSSE && !tc.fixed && !tc.continuation && !tc.disableRetry {
				if gjson.GetBytes(terminal, "type").String() != "response.completed" || calls.Load() != 1 {
					t.Fatalf("matching-protocol alternative was not tried: calls=%d body=%s", calls.Load(), terminal)
				}
			} else if calls.Load() != 0 || gjson.GetBytes(terminal, "status").Int() != 503 || gjson.GetBytes(terminal, "error.code").String() != ErrorCodeCodexTicketUnavailable || !strings.Contains(string(terminal), "WebSocket") || !strings.Contains(string(terminal), "ticket") || strings.Contains(string(terminal), schedulerSelectionTimeoutMessage) {
				t.Fatalf("protocol ticket error was masked or reached upstream: calls=%d body=%s", calls.Load(), terminal)
			}
			// response.completed can reach the socket just before the handler's
			// final lease release; wait only for that successful attempt to unwind.
			if alternative != nil {
				deadline := time.Now().Add(time.Second)
				for alternative.GetActiveRequests() != 0 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
			}
			if sseAccount.CodexTicketForModel(gatewayModel, time.Now(), 780, "sse") == nil || sseAccount.GetActiveRequests() != 0 || (alternative != nil && alternative.GetActiveRequests() != 0) {
				t.Fatal("local rejection revoked the SSE ticket or leaked an account lease")
			}
			if tc.fixed || tc.continuation {
				if id, ok := store.SessionAffinityAccountID("protocol-session"); !ok || id != sseAccount.ID() {
					t.Fatal("local rejection lost the pinned affinity")
				}
			}
		})
	}
}
