package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

func TestResolveCodexEgressForContextKeepsResinSnapshot(t *testing.T) {
	const target = "https://chatgpt.com/backend-api/codex/responses"
	const wsTarget = "wss://chatgpt.com/backend-api/codex/responses"
	const proxyURL = "http://pool:8080"
	account := &auth.Account{DBID: 42}

	for _, tc := range []struct {
		name string
		next *ResinConfig
	}{
		{name: "disabled globally"},
		{name: "replaced globally", next: &ResinConfig{BaseURL: "http://new-resin/new-token", PlatformName: "new-platform"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withResin(t, &ResinConfig{BaseURL: "http://resin/token", PlatformName: "p1,p2"})
			ctx := WithResinPlatform(WithResinConfig(t.Context(), GetResinConfig()), "p2")
			SetResinConfig(tc.next)

			httpEgress := ResolveCodexEgressForContext(ctx, account, target, proxyURL)
			wsEgress := ResolveCodexWebsocketEgressForContext(ctx, account, wsTarget, proxyURL)
			for _, egress := range []CodexEgress{httpEgress, wsEgress} {
				if !egress.ViaResin() || egress.DialProxyURL != "" || egress.ProxyURL != proxyURL {
					t.Fatalf("snapshot route = %+v", egress)
				}
				h := http.Header{}
				egress.ApplyHeaders(h)
				if got := h.Get("X-Resin-Account"); got != "42" {
					t.Fatalf("X-Resin-Account = %q, want 42", got)
				}
			}
			if want := "http://resin/token/p2/https/chatgpt.com/backend-api/codex/responses"; httpEgress.URL != want {
				t.Fatalf("HTTP URL = %q, want %q", httpEgress.URL, want)
			}
			if want := "ws://resin/token/p2/https/chatgpt.com/backend-api/codex/responses"; wsEgress.URL != want {
				t.Fatalf("WS URL = %q, want %q", wsEgress.URL, want)
			}
			if got := CodexDialProxyURLForContext(ctx, account, proxyURL); got != "" {
				t.Fatalf("WS dial proxy = %q, want empty for pinned Resin route", got)
			}
			url, client, viaResin := resinMaintenanceTargetForContext(ctx, account, target, "")
			if url != httpEgress.URL || !viaResin || client != httpEgress.Client() {
				t.Fatalf("maintenance route = %q, viaResin=%v; want same snapshot as inference", url, viaResin)
			}
		})
	}
}

func TestResolveCodexEgressForContextKeepsDisabledSnapshot(t *testing.T) {
	withResin(t, nil)
	ctx := WithResinConfig(t.Context(), nil)
	SetResinConfig(&ResinConfig{BaseURL: "http://resin/token", PlatformName: "p1,p2"})
	account := &auth.Account{DBID: 42}
	const target = "https://chatgpt.com/backend-api/codex/responses"
	const wsTarget = "wss://chatgpt.com/backend-api/codex/responses"
	const proxyURL = "http://pool:8080"

	for _, tc := range []struct {
		egress CodexEgress
		target string
	}{
		{ResolveCodexEgressForContext(ctx, account, target, " "+proxyURL+" "), target},
		{ResolveCodexWebsocketEgressForContext(ctx, account, wsTarget, " "+proxyURL+" "), wsTarget},
	} {
		if tc.egress.Kind != CodexEgressProxy || tc.egress.URL != tc.target || tc.egress.DialProxyURL != proxyURL {
			t.Fatalf("disabled snapshot route = %+v", tc.egress)
		}
		h := http.Header{}
		tc.egress.ApplyHeaders(h)
		if h.Get("X-Resin-Account") != "" {
			t.Fatal("disabled snapshot must not add a Resin account header")
		}
	}
	if got := CodexDialProxyURLForContext(ctx, account, " "+proxyURL+" "); got != proxyURL {
		t.Fatalf("WS dial proxy = %q, want %q", got, proxyURL)
	}
	if url, client, viaResin := resinMaintenanceTargetForContext(ctx, account, target, "p2"); url != target || client != nil || viaResin {
		t.Fatalf("disabled maintenance route = %q, client=%v, viaResin=%v", url, client, viaResin)
	}
}

func TestUpstreamTraceResinUsesRequestSnapshot(t *testing.T) {
	withResin(t, &ResinConfig{BaseURL: "http://resin/token", PlatformName: "p1,p2"})
	resinCtx := WithResinConfig(t.Context(), GetResinConfig())
	directCtx := WithResinConfig(t.Context(), nil)

	for _, enabled := range []bool{false, true} {
		if enabled {
			SetResinConfig(&ResinConfig{BaseURL: "http://new-resin/token", PlatformName: "p3"})
		} else {
			SetResinConfig(nil)
		}
		for _, tc := range []struct {
			name    string
			ctx     context.Context
			account *auth.Account
			want    string
		}{
			{"codex", resinCtx, &auth.Account{DBID: 1}, "resin"},
			{"traecn", resinCtx, &auth.Account{DBID: 2, UpstreamType: auth.UpstreamTraeCN}, "resin"},
			{"relay", resinCtx, &auth.Account{DBID: 3, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://relay.example", APIKey: "sk-test"}, "direct/no_proxy"},
			{"disabled", directCtx, &auth.Account{DBID: 4}, "direct/no_proxy"},
		} {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequestWithContext(tc.ctx, http.MethodPost, "/v1/responses", nil)
			attachUpstreamTrace(c, nil)
			beginUpstreamTrace(c.Request.Context(), tc.account, "", false)
			if got := snapshotUpstreamTrace(c.Request.Context()).Proxy.Name; got != tc.want {
				t.Fatalf("%s trace with global enabled=%v: got %q, want %q", tc.name, enabled, got, tc.want)
			}
		}
	}
}

func TestExecuteCodexRequestsKeepResinSnapshot(t *testing.T) {
	type capturedRequest struct {
		path    string
		account string
	}
	captured := make(chan capturedRequest, 2)
	resin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- capturedRequest{path: r.URL.Path, account: r.Header.Get("X-Resin-Account")}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resin-snapshot-test"}`)
	}))
	defer resin.Close()
	withResin(t, &ResinConfig{BaseURL: resin.URL + "/token", PlatformName: "p1,p2"})
	ctx := WithResinPlatform(WithResinConfig(t.Context(), GetResinConfig()), "p2")
	SetResinConfig(nil)
	account := &auth.Account{DBID: 91003, AccessToken: "AT", ProxyURL: "http://127.0.0.1:1"}
	body := []byte(`{"model":"gpt-5.4","input":"hello"}`)

	for _, compact := range []bool{false, true} {
		var resp *http.Response
		var err error
		endpoint := "/responses"
		if compact {
			endpoint += "/compact"
			resp, err = ExecuteCompactRequest(ctx, account, body, "session", "", "key", nil, nil)
		} else {
			resp, err = ExecuteRequest(ctx, account, body, "session", "", "key", nil, nil, false)
		}
		if err != nil {
			t.Fatalf("%s request failed: %v", endpoint, err)
		}
		_ = resp.Body.Close()
		select {
		case got := <-captured:
			wantPath := "/token/p2/https/chatgpt.com/backend-api/codex" + endpoint
			if got.path != wantPath || got.account != "91003" {
				t.Fatalf("%s route = %+v, want path=%q, account=91003", endpoint, got, wantPath)
			}
		default:
			t.Fatalf("%s did not reach the pinned Resin endpoint", endpoint)
		}
	}
}
