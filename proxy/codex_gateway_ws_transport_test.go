package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/harvest"
)

// Synthetic process-local routing state; no background acquisition or account traffic.
func newGatewayWSUnitFixture(t *testing.T) (*CodexHarvestManager, *auth.Account, *auth.Account) {
	t.Helper()
	m, target := newHarvestManagerTest(t)
	settings := auth.ConfiguredCodexTicketSettings()
	settings.TargetLength, settings.FailClosed = 292, false
	auth.SetConfiguredCodexTicketSettings(settings)
	id, err := m.DB.InsertAccountWithCredentials(t.Context(), "gateway-ws-offline-source", map[string]any{"upstream_type": "codex", "access_token": "offline-source", "plan_type": "plus"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Store.LoadAccountByID(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	v := harvest.DefaultGatewaySettings()
	v.CookiePool.Enabled = true
	v.CookiePool.SourceAccountIDs = []int64{id}
	v.CookiePool.TargetAccountIDs = []int64{target.ID()}
	v.WSSession.Enabled = true
	v.WSSession.AccountIDs = []int64{target.ID()}
	v.Revision, v.RouteRevision = "offline-cas", "offline-route"
	raw, _ := json.Marshal(v)
	if err = m.Repository.Set(t.Context(), harvest.GatewaySettingsKey, string(raw)); err != nil {
		t.Fatal(err)
	}
	g := m.gatewayManager()
	g.mu.Lock()
	g.loaded = false
	g.mu.Unlock()
	if _, err = m.GatewaySettings(t.Context()); err != nil {
		t.Fatal(err)
	}
	return m, m.Store.FindByID(id), target
}

func TestGatewayWSCompletedNullErrorIsSuccess(t *testing.T) {
	for _, tc := range []struct {
		terminal string
		retain   bool
	}{
		{`{"type":"response.completed","response":{"id":"r","model":"gpt-6-astra","status":"completed","error":null}}`, true},
		{`{"type":"response.done","response":{"id":"r","model":"gpt-6-astra","status":"completed"}}`, true},
		{`{"type":"response.completed","response":{"id":"r","model":"gpt-6-astra","status":"completed","error":{"code":"failed"}}}`, false},
		{`{"type":"response.failed","response":{"id":"r"}}`, false},
	} {
		turn := &GatewayWSTurn{anchor: &gatewayAnchor{expires: time.Now().Add(time.Minute)}}
		turn.Observe([]byte(tc.terminal))
		if turn.CanRetain() != tc.retain {
			t.Fatalf("retain=%v for %s", turn.CanRetain(), tc.terminal)
		}
	}
}

func TestGatewayWSExplicitScopeAndExecutionLanes(t *testing.T) {
	_, _, target := newGatewayWSUnitFixture(t)
	firstHeaders := http.Header{"Session-Id": {"shared"}, "Thread-Id": {"thread-a"}}
	first, err := BeginCodexGatewayWSTurn(t.Context(), target, []byte(`{"model":"gpt-6-astra"}`), firstHeaders, "key", "")
	if err != nil || first == nil {
		t.Fatalf("first: %v", err)
	}
	defer first.Finish(true)
	for _, tc := range []struct {
		name    string
		headers http.Header
		body    string
	}{
		{"different thread", http.Header{"Session-Id": {"shared"}, "Thread-Id": {"thread-b"}}, `{"model":"gpt-6-astra"}`},
		{"memory lane", http.Header{"Session-Id": {"shared"}, "Thread-Id": {"thread-a"}, "X-Codex-Turn-Metadata": {`{"thread_id":"thread-a","request_kind":"memory"}`}}, `{"model":"gpt-6-astra"}`},
		{"guardian lane", http.Header{"Session-Id": {"shared"}, "Thread-Id": {"thread-a"}, "X-Openai-Subagent": {"guardian"}}, `{"model":"gpt-6-astra"}`},
		{"body-only thread", http.Header{"Session-Id": {"shared"}}, `{"model":"gpt-6-astra","client_metadata":{"thread_id":"thread-body"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			turn, err := BeginCodexGatewayWSTurn(t.Context(), target, []byte(tc.body), tc.headers, "key", "")
			if err != nil || turn == nil {
				t.Fatalf("isolated request collided: %v", err)
			}
			defer turn.Finish(true)
			if turn.PoolKey() == first.PoolKey() {
				t.Fatal("distinct scope shares a pool key")
			}
		})
	}
	ctx := withCodexGatewayWSClientScope(t.Context(), nil, []byte(`{"model":"gpt-6-astra"}`))
	if turn, err := BeginCodexGatewayWSTurn(ctx, target, []byte(`{"model":"gpt-6-astra","prompt_cache_key":"server-generated"}`), nil, "key", ""); err == nil {
		if turn != nil {
			turn.Finish(true)
		}
		t.Fatal("generated cache key became an explicit client identity")
	}
}

func TestGatewayWSContextUsesAnchorDeadline(t *testing.T) {
	_, _, target := newGatewayWSUnitFixture(t)
	turn, err := BeginCodexGatewayWSTurn(t.Context(), target, []byte(`{"model":"gpt-6-astra"}`), http.Header{"Session-Id": {"deadline"}}, "key", "")
	if err != nil {
		t.Fatal(err)
	}
	defer turn.Finish(true)
	turn.anchor.expires = time.Now().Add(30 * time.Millisecond)
	ctx := turn.Context(context.Background())
	deadline, ok := ctx.Deadline()
	if !ok || !deadline.Equal(turn.Deadline()) {
		t.Fatal("lost fixed deadline")
	}
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatal(ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("anchor deadline did not cancel")
	}
}

func TestGatewayOptionalPinFailureFallsBack(t *testing.T) {
	m, source, target := newGatewayWSUnitFixture(t)
	g := m.gatewayManager()
	g.mu.Lock()
	g.routes[source.ID()] = gatewayRoute{sourceID: source.ID(), identity: gatewayIdentity(source), cookie: "offline-cookie", expires: time.Now().Add(time.Minute), nodeID: "removed-node", poolID: "removed-pool", managed: true}
	g.mu.Unlock()
	ctx, actual, release, err := prepareCodexGatewayHTTP(t.Context(), target, []byte(`{"model":"gpt-6-astra"}`), "http://127.0.0.1:12345")
	defer release()
	if err != nil || actual != "http://127.0.0.1:12345" {
		t.Fatalf("optional pin gated traffic: proxy=%q err=%v", actual, err)
	}
	if _, bound, _ := CodexGatewayEgressFromContext(ctx); bound {
		t.Fatal("failed pin retained route binding")
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, finish, err := prepareCodexGatewayHTTP(cancelled, target, []byte(`{"model":"gpt-6-astra"}`), "")
	finish()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was swallowed: %v", err)
	}
}

func TestGatewayWSOrdinaryEntryBindsCookieAndExitBeforeExecutor(t *testing.T) {
	m, source, target := newGatewayWSUnitFixture(t)
	g := m.gatewayManager()
	g.mu.Lock()
	g.settings.WSSession.Enabled = false
	g.settings.CookiePool.IPAffinity = true
	g.loadedUntil = time.Now().Add(time.Hour)
	g.routes[source.ID()] = gatewayRoute{sourceID: source.ID(), identity: gatewayIdentity(source), cookie: "source-only-route", proxy: "http://127.0.0.1:17925", expires: time.Now().Add(time.Minute)}
	g.mu.Unlock()
	previous := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previous })
	calls := 0
	WebsocketExecuteFunc = func(ctx context.Context, a *auth.Account, body []byte, session, proxyURL, key string, cfg *DeviceProfileConfig, headers http.Header, pool string) (*http.Response, error) {
		calls++
		bound, ok, _ := CodexGatewayEgressFromContext(ctx)
		if !ok || bound != "http://127.0.0.1:17925" || proxyURL != bound {
			t.Fatalf("ordinary WS reached executor without route binding: %q %q %v", bound, proxyURL, ok)
		}
		egress := ResolveCodexWebsocketEgressForContext(ctx, a, "wss://chatgpt.com/backend-api/codex/responses", proxyURL)
		if egress.ViaResin() || egress.DialProxyURL != bound {
			t.Fatal("Resin replaced the borrowed WS exit")
		}
		headers = headers.Clone()
		ApplyCodexGatewayCookie(ctx, headers)
		if headers.Get("Cookie") != "__oailb=source-only-route" {
			t.Fatal("missing borrowed Cookie")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	ctx := WithResinConfig(t.Context(), &ResinConfig{BaseURL: "http://127.0.0.1:1", PlatformName: "must-not-use"})
	response, err := ExecuteRequest(ctx, target, []byte(`{"model":"gpt-6-astra","input":"offline"}`), "", "http://127.0.0.1:12345", "offline-key", nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if calls != 1 {
		t.Fatalf("executor calls=%d", calls)
	}
}

func TestGatewayCookieBindsActualExitAndTicketWins(t *testing.T) {
	m, source, target := newGatewayWSUnitFixture(t)
	g := m.gatewayManager()
	g.mu.Lock()
	g.routes[source.ID()] = gatewayRoute{sourceID: source.ID(), identity: gatewayIdentity(source), cookie: "route", proxy: "http://source.invalid:1", expires: time.Now().Add(time.Minute)}
	g.mu.Unlock()
	for _, fallback := range []string{"http://target.invalid:2", ""} {
		ctx, actual, release, err := prepareCodexGatewayHTTP(t.Context(), target, []byte(`{"model":"gpt-6-astra"}`), fallback)
		if err != nil {
			t.Fatal(err)
		}
		pinned, bound, fresh := CodexGatewayEgressFromContext(ctx)
		release()
		if !bound || fresh || actual != fallback || pinned != fallback {
			t.Fatalf("IP affinity disabled changed target exit: %q %q %v %v", actual, pinned, bound, fresh)
		}
	}
	ctx := withCodexTurnStateInjection(t.Context(), "complete-ticket-state")
	ctx = context.WithValue(ctx, codexTicketInjectionKey{}, &auth.CodexTicket{Cookie: "__cflb=own; __oailb=own", State: "complete-ticket-state"})
	ctx, _, release, err := prepareCodexGatewayHTTP(ctx, target, []byte(`{"model":"gpt-6-astra"}`), "")
	defer release()
	if err != nil {
		t.Fatal(err)
	}
	if _, bound, _ := CodexGatewayEgressFromContext(ctx); bound {
		t.Fatal("route overlaid complete ticket")
	}
	h := http.Header{"Cookie": {"__cflb=own; __oailb=own"}}
	turn := &GatewayWSTurn{anchor: &gatewayAnchor{cookie: "foreign-route"}}
	turn.Apply(h, ctx)
	if h.Get("Cookie") != "__cflb=own; __oailb=own" {
		t.Fatal("continued turn mixed complete ticket and old route")
	}
}
