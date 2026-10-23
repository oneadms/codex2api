package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestDeletedContinuationAccountDoesNotWaitForSelectionTimeout(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, FastSchedulerEnabled: true})
	t.Cleanup(store.Stop)
	deleted := &auth.Account{DBID: 1, AccountID: "deleted", AccessToken: "offline-deleted", PlanType: "plus", Status: auth.StatusReady}
	remaining := &auth.Account{DBID: 2, AccountID: "remaining", AccessToken: "offline-remaining", PlanType: "plus", Status: auth.StatusReady}
	store.AddAccount(deleted)
	store.AddAccount(remaining)
	store.BindSessionAffinity("deleted-session", deleted, "")
	store.RemoveAccount(deleted.ID())
	h := &Handler{store: store}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	account, _, err := h.nextRetryAccountForContinuationWithDispatch(ctx, "deleted-session", 0, newRetryAccountExclusions(), accountFilterForModel("gpt-6-astra"), auth.DispatchPolicyStandard)
	if account != nil {
		store.Release(account)
		t.Fatal("stateful continuation was silently moved to another account")
	}
	var lost *ResponsesContinuationLostError
	if !errors.As(err, &lost) || lost.Reason != "bound_account_unavailable" {
		t.Fatalf("deleted bound account must fail as unavailable context instead of selection timeout: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("deleted account consumed the whole selection deadline")
	}
	// A fresh request remains schedulable on the surviving account.
	account, _, _, err = h.nextRetryAccountForSessionWithDispatchGuard(t.Context(), "fresh-session", 0, newRetryAccountExclusions(), accountFilterForModel("gpt-6-astra"), auth.DispatchPolicyStandard)
	if err != nil || account != remaining {
		t.Fatalf("fresh request cannot use surviving account: %v %v", account, err)
	}
	store.Release(account)
}

func TestExistingContinuationAccountStillWaitsForCapacity(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, FastSchedulerEnabled: true})
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: 1, AccountID: "busy", AccessToken: "offline-busy", PlanType: "plus", Status: auth.StatusReady}
	store.AddAccount(account)
	store.BindSessionAffinity("busy-session", account, "")
	lease, _ := store.NextForContinuationWithDispatch("busy-session", 0, nil, nil, auth.DispatchPolicyStandard)
	if lease == nil {
		t.Fatal("fixture did not acquire the original account")
	}
	defer store.Release(lease)
	store.AddAccount(&auth.Account{DBID: 2, AccountID: "other", AccessToken: "offline-other", PlanType: "plus", Status: auth.StatusReady})
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	h := &Handler{store: store}
	got, _, err := h.nextRetryAccountForContinuationWithDispatch(ctx, "busy-session", 0, newRetryAccountExclusions(), nil, auth.DispatchPolicyStandard)
	if got != nil {
		store.Release(got)
		t.Fatal("busy continuation escaped to another account")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("existing busy account must retain normal queue semantics: %v", err)
	}
}

func TestNativeWSDeletedOpaqueContextOwnerCanStartFreshTurn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetResponseCacheForTest()
	t.Cleanup(resetResponseCacheForTest)
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, FastSchedulerEnabled: true})
	t.Cleanup(store.Stop)
	deleted := &auth.Account{DBID: 1, AccountID: "deleted", AccessToken: "offline-deleted", PlanType: "plus", Status: auth.StatusReady}
	store.AddAccount(deleted)
	var calls atomic.Int32
	oldWS := WebsocketExecuteFunc
	WebsocketExecuteFunc = func(_ context.Context, a *auth.Account, _ []byte, _, _, _ string, _ *DeviceProfileConfig, _ http.Header, _ string) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return nativeWSResponse("deleted-context", `{"type":"reasoning","encrypted_content":"offline-opaque"}`), nil
		}
		if a.ID() != 2 {
			t.Error("fresh request did not select the surviving account")
		}
		return nativeWSResponse("fresh-context"), nil
	}
	t.Cleanup(func() { WebsocketExecuteFunc = oldWS })
	h := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	h.RegisterRoutes(router)
	server := httptest.NewServer(router)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Session-Id": {"deleted-session"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	assertNativeWSSuccess(t, nativeWSTurn(t, conn, `{"input":[{"role":"user","content":"first"}]}`))
	store.RemoveAccount(deleted.ID())
	store.UnbindSessionAffinity(sessionAffinityKey("deleted-session", 0), deleted.ID())
	store.AddAccount(&auth.Account{DBID: 2, AccountID: "remaining", AccessToken: "offline-remaining", PlanType: "plus", Status: auth.StatusReady})
	terminal := nativeWSTurn(t, conn, `{"previous_response_id":"deleted-context","input":[{"role":"user","content":"next"}]}`)
	if gjson.GetBytes(terminal, "error.code").String() != "previous_response_not_found" || !strings.Contains(string(terminal), unavailableContinuationAccountMessage) || calls.Load() != 1 {
		t.Fatalf("deleted opaque context owner was not rejected before selection: calls=%d body=%s", calls.Load(), terminal)
	}
	assertNativeWSSuccess(t, nativeWSTurn(t, conn, `{"input":[{"role":"user","content":"new conversation"}]}`))
	if calls.Load() != 2 {
		t.Fatalf("fresh request did not reach upstream exactly once: %d", calls.Load())
	}
}

func TestDeletedContinuationAccountResponsesIngress(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, transport := range []string{"http", "websocket"} {
		t.Run(transport, func(t *testing.T) {
			store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, FastSchedulerEnabled: true})
			t.Cleanup(store.Stop)
			deleted := &auth.Account{DBID: 1, AccountID: "deleted", AccessToken: "offline-deleted", PlanType: "plus", Status: auth.StatusReady}
			store.AddAccount(deleted)
			store.AddAccount(&auth.Account{DBID: 2, AccountID: "remaining", AccessToken: "offline-remaining", PlanType: "plus", Status: auth.StatusReady})
			store.BindSessionAffinity(sessionAffinityKey("deleted-session", 0), deleted, "")
			store.RemoveAccount(deleted.ID())
			var upstreamCalls atomic.Int32
			oldWS := WebsocketExecuteFunc
			WebsocketExecuteFunc = func(context.Context, *auth.Account, []byte, string, string, string, *DeviceProfileConfig, http.Header, string) (*http.Response, error) {
				upstreamCalls.Add(1)
				return nil, errors.New("unexpected upstream call")
			}
			t.Cleanup(func() { WebsocketExecuteFunc = oldWS })
			h := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true, CodexUpstreamTransport: "ws"}, nil)
			body := `{"type":"response.create","model":"gpt-5.5","input":"continue","client_metadata":{"x-codex-turn-state":"old-turn"}}`
			var response []byte
			if transport == "http" {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)).WithContext(ctx)
				c.Request.Header.Set("Session-Id", "deleted-session")
				h.Responses(c)
				response = recorder.Body.Bytes()
				if recorder.Code != http.StatusConflict || recorder.Header().Get("Retry-After") != "" {
					t.Fatalf("lost context returned retryable selection failure: %d %s", recorder.Code, response)
				}
			} else {
				router := gin.New()
				h.RegisterRoutes(router)
				server := httptest.NewServer(router)
				defer server.Close()
				conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Session-Id": {"deleted-session"}})
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				if err = conn.WriteMessage(websocket.TextMessage, []byte(body)); err != nil {
					t.Fatal(err)
				}
				_ = conn.SetReadDeadline(time.Now().Add(time.Second))
				_, response, err = conn.ReadMessage()
				if err != nil {
					t.Fatal(err)
				}
				if gjson.GetBytes(response, "error.code").String() != "previous_response_not_found" {
					t.Fatalf("WS lost context not reported explicitly: %s", response)
				}
			}
			if !strings.Contains(string(response), unavailableContinuationAccountMessage) || strings.Contains(string(response), schedulerSelectionTimeoutMessage) || upstreamCalls.Load() != 0 {
				t.Fatalf("lost context recovery failed: calls=%d response=%s", upstreamCalls.Load(), response)
			}
		})
	}
}
