package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestCodexGatewayEgressUsesEffectiveExitBeforeResin(t *testing.T) {
	for _, tc := range []struct {
		name           string
		affinity       bool
		fallback, want string
	}{
		{"source affinity", true, "http://127.0.0.1:18002", "http://127.0.0.1:18001"},
		{"target exit", false, "http://127.0.0.1:18002", "http://127.0.0.1:18002"},
		{"explicit direct", false, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, source, target, v := newGatewayRegressionFixture(t)
			v.CookiePool.IPAffinity = tc.affinity
			seedGatewayRegressionSettings(t, m, v)
			g := m.gatewayManager()
			r := seedGatewayRegressionRoute(g, source)
			r.proxy = "http://127.0.0.1:18001"
			g.mu.Lock()
			g.routes[source.ID()] = r
			g.mu.Unlock()
			ctx := WithResinConfig(t.Context(), &ResinConfig{BaseURL: "http://127.0.0.1:2260/test", PlatformName: "test"})
			ctx, selected, release, err := prepareCodexGatewayHTTP(ctx, target, []byte(`{"model":"gpt-6-astra"}`), tc.fallback)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			for _, ws := range []bool{false, true} {
				egress := ResolveCodexRequestEgress(ctx, target, "https://chatgpt.com/backend-api/codex/responses", selected, ws)
				if egress.DialProxyURL != tc.want || egress.ViaResin() {
					t.Fatalf("bound route lost: ws=%v %+v", ws, egress)
				}
			}
			if got := CodexDialProxyURLForContext(ctx, target, selected); got != tc.want {
				t.Fatalf("dialer lost bound exit: %q", got)
			}
			h := http.Header{}
			ApplyCodexGatewayCookie(ctx, h)
			if h.Get("Cookie") != "__oailb="+r.cookie {
				t.Fatal("borrowed cookie missing")
			}
		})
	}
}

func TestCodexGatewayUnavailableOptionalExitFallsBack(t *testing.T) {
	m, source, target, _ := newGatewayRegressionFixture(t)
	g := m.gatewayManager()
	r := seedGatewayRegressionRoute(g, source)
	r.nodeID, r.poolID, r.managed = "removed", "removed", true
	g.mu.Lock()
	g.routes[source.ID()] = r
	g.mu.Unlock()
	ctx, selected, release, err := prepareCodexGatewayHTTP(t.Context(), target, []byte(`{"model":"gpt-6-astra"}`), "http://127.0.0.1:18002")
	defer release()
	if err != nil || selected != "http://127.0.0.1:18002" {
		t.Fatalf("optional exit blocked ordinary request: %q %v", selected, err)
	}
	if _, bound, _ := CodexGatewayEgressFromContext(ctx); bound {
		t.Fatal("failed optional exit kept borrowed materials")
	}
	h := http.Header{}
	ApplyCodexGatewayCookie(ctx, h)
	if h.Get("Cookie") != "" {
		t.Fatal("borrowed cookie survived fallback")
	}
}

func TestCodexGatewayManagedHTTPRouteOpensFreshConnection(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	m, _, target, _ := newGatewayRegressionFixture(t)
	_ = m
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "complete") }))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.StartTLS()
	defer server.Close()
	defer recyclePooledClient(target, "")
	for _, node := range []string{"node-A", "node-B"} {
		ctx := WithResinConfig(t.Context(), nil)
		ctx = context.WithValue(ctx, gatewayRequestKey{}, gatewayRequest{route: gatewayRoute{nodeID: node, cookie: node}, proxyBound: true, fresh: true})
		egress := ResolveCodexEgressForContext(ctx, target, server.URL, "")
		client := gatewayHTTPClient(ctx, egress.Client())
		tr := client.Transport.(*http.Transport)
		tr.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		defer client.CloseIdleConnections()
	}
	if connections.Load() != 2 {
		t.Fatalf("selector switch reused previous TLS connection: %d connections", connections.Load())
	}
}
