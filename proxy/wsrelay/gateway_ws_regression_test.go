package wsrelay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/harvest"
	"github.com/codex2api/internal/mihomo"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
)

func fixedGatewayExecutionFixture(t *testing.T) (*proxy.CodexHarvestManager, *auth.Account, *gatewayWSServer, *Executor, context.Context) {
	t.Helper()
	server := newGatewayWSServer(t)
	manager, sourceID, targetID := newGatewayManager(t)
	v := harvest.DefaultGatewaySettings()
	v.CookiePool.SourceAccountIDs, v.CookiePool.TargetAccountIDs = []int64{sourceID}, []int64{targetID}
	v.WSSession.Enabled, v.WSSession.AccountIDs = true, []int64{targetID}
	v.Revision, v.RouteRevision = "offline-cas", "offline-route"
	v.Normalize()
	raw, _ := json.Marshal(v)
	if err := manager.Repository.Set(t.Context(), harvest.GatewaySettingsKey, string(raw)); err != nil {
		t.Fatal(err)
	}
	account := manager.Store.FindByID(targetID)
	seedGatewayWSTicket(t, account)
	executor := NewExecutorWithManager(NewManager())
	t.Cleanup(executor.manager.Stop)
	installGatewayTestExecutor(t, executor)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	ctx = proxy.WithResinConfig(ctx, &proxy.ResinConfig{BaseURL: server.server.URL, PlatformName: "gateway-offline"})
	return manager, account, server, executor, ctx
}

func installGatewayTestExecutor(t *testing.T, executor *Executor) {
	t.Helper()
	previous := proxy.WebsocketExecuteFunc
	t.Cleanup(func() { proxy.WebsocketExecuteFunc = previous })
	proxy.WebsocketExecuteFunc = func(ctx context.Context, a *auth.Account, body []byte, session, proxyURL, key string, cfg *proxy.DeviceProfileConfig, headers http.Header, pool string) (*http.Response, error) {
		response, err := executor.ExecuteRequestViaWebsocket(ctx, a, body, session, proxyURL, key, cfg, headers, pool)
		if err != nil {
			return nil, err
		}
		return websocketResponseToHTTP(ctx, response, http.StatusOK, nil), nil
	}
}

func gatewayExecuteAndRead(t *testing.T, ctx context.Context, a *auth.Account, input, previous string, headers http.Header) string {
	t.Helper()
	response, err := proxy.ExecuteRequest(ctx, a, gatewayWSBody(input, previous), "fixed-client-session", "", "offline-key", nil, headers, true)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := readAllHTTPBody(response)
	if err != nil {
		t.Fatal(err)
	}
	id := gatewayWSResponseID(raw)
	if id == "" {
		t.Fatalf("no response ID: %s", raw)
	}
	return id
}

func TestGatewayWSRealEntrySeparatesThreadsAndMemory(t *testing.T) {
	for _, lane := range []string{"thread", "memory"} {
		t.Run(lane, func(t *testing.T) {
			_, account, server, _, ctx := fixedGatewayExecutionFixture(t)
			firstHeaders := http.Header{"Session-Id": {"shared"}, "Thread-Id": {"thread-a"}}
			first, err := proxy.ExecuteRequest(ctx, account, gatewayWSBody("a", ""), "fixed-client-session", "", "offline-key", nil, firstHeaders, true)
			if err != nil {
				t.Fatal(err)
			}
			secondHeaders := firstHeaders.Clone()
			if lane == "thread" {
				secondHeaders.Set("Thread-Id", "thread-b")
			} else {
				secondHeaders.Set("X-Codex-Turn-Metadata", `{"thread_id":"thread-a","request_kind":"memory"}`)
			}
			secondID := gatewayExecuteAndRead(t, ctx, account, "b", "", secondHeaders)
			firstBody, err := readAllHTTPBody(first)
			if err != nil {
				t.Fatal(err)
			}
			firstID := gatewayWSResponseID(firstBody)
			gatewayExecuteAndRead(t, ctx, account, "a-next", firstID, firstHeaders)
			secondID = gatewayExecuteAndRead(t, ctx, account, "b-next", secondID, secondHeaders)
			if _, err := proxy.ExecuteRequest(ctx, account, gatewayWSBody("cross-scope", secondID), "fixed-client-session", "", "offline-key", nil, firstHeaders, true); err == nil {
				t.Fatal("one thread continued another thread's latest response")
			}
			if server.dials.Load() != 2 {
				t.Fatalf("independent scopes must each retain one connection: %d", server.dials.Load())
			}
		})
	}
}

func TestGatewayWSRealEntryRejectsGeneratedIdentity(t *testing.T) {
	_, account, server, _, ctx := fixedGatewayExecutionFixture(t)
	_, err := proxy.ExecuteRequest(ctx, account, gatewayWSBody("stateless", ""), "", "", "offline-key", nil, nil, true)
	var fixed *proxy.CodexGatewayWSError
	if !errors.As(err, &fixed) || server.dials.Load() != 0 {
		t.Fatalf("generated key bypassed explicit scope: err=%v dials=%d", err, server.dials.Load())
	}
}

func TestGatewayWSDiagnosticUsesBusinessExecutorAndOriginalConnection(t *testing.T) {
	manager, account, server, _, ctx := fixedGatewayExecutionFixture(t)
	if err := manager.TestGatewayWS(ctx, account.ID()); err != nil {
		t.Fatal(err)
	}
	if server.dials.Load() != 1 || server.sequence.Load() != 2 {
		t.Fatalf("diagnostic did not perform two same-connection business turns: dials=%d frames=%d", server.dials.Load(), server.sequence.Load())
	}
	rows := drainGatewayWSRequests(server, 2)
	if len(rows) != 2 || !strings.HasSuffix(rows[0], "|") || strings.HasSuffix(rows[1], "|") {
		t.Fatalf("diagnostic did not send a continuation ID: %v", rows)
	}
	for _, row := range manager.GatewaySnapshot(ctx).WS {
		if row.ActiveSessions != 0 {
			t.Fatal("diagnostic leaked a fixed session")
		}
	}
	// An available ticket and even a fake successful HTTP body cannot prove WS.
	proxy.WebsocketExecuteFunc = func(context.Context, *auth.Account, []byte, string, string, string, *proxy.DeviceProfileConfig, http.Header, string) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`data: {"type":"response.completed"}`))}, nil
	}
	if err := manager.TestGatewayWS(ctx, account.ID()); err == nil {
		t.Fatal("diagnostic accepted a cached ticket/stub without a WS turn")
	}
}

func TestGatewayWSDiagnosticRejectsLostOriginalConnection(t *testing.T) {
	manager, account, server, _, ctx := fixedGatewayExecutionFixture(t)
	server.closeAfterReply.Store(true)
	if err := manager.TestGatewayWS(ctx, account.ID()); err == nil {
		t.Fatal("diagnostic succeeded after the original connection closed")
	}
	if server.dials.Load() != 1 {
		t.Fatalf("diagnostic redialed a lost fixed connection: %d", server.dials.Load())
	}
	for _, row := range manager.GatewaySnapshot(ctx).WS {
		if row.ActiveSessions != 0 {
			t.Fatal("failed diagnostic leaked a session")
		}
	}
}

func TestGatewayWSResponseUsesExecutorDeadline(t *testing.T) {
	_, account, _, executor, parent := fixedGatewayExecutionFixture(t)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err = conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"waiting"}}`))
		for {
			if _, _, err = conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	parent = proxy.WithResinConfig(parent, &proxy.ResinConfig{BaseURL: server.URL, PlatformName: "deadline"})
	proxy.WebsocketExecuteFunc = func(ctx context.Context, a *auth.Account, body []byte, session, proxyURL, key string, cfg *proxy.DeviceProfileConfig, headers http.Header, pool string) (*http.Response, error) {
		// The executor's lifetime is deliberately shorter than its caller's,
		// exactly as with a fixed-session deadline. The wrapper gets the parent.
		child, cancel := context.WithTimeout(ctx, 80*time.Millisecond)
		t.Cleanup(cancel)
		response, err := executor.ExecuteRequestViaWebsocket(child, a, body, session, proxyURL, key, cfg, headers, pool)
		if err != nil {
			return nil, err
		}
		return websocketResponseToHTTP(ctx, response, 200, nil), nil
	}
	response, err := proxy.ExecuteRequest(parent, account, gatewayWSBody("wait", ""), "deadline", "", "offline-key", nil, http.Header{"Session-Id": {"deadline"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = readAllHTTPBody(response)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("executor deadline did not stop the read: %v (%v)", err, time.Since(start))
	}
	if parent.Err() != nil {
		t.Fatal("test accidentally exercised the parent deadline")
	}
}

// The only network endpoints in this fixture are loopback. CONNECT forwards to
// the local WS fixture; no TLS/account traffic can leave the test process.
func gatewayOfflineProxy(t *testing.T, upstream string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			w.WriteHeader(503)
			return
		}
		out, err := net.Dial("tcp", upstream)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		in, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			out.Close()
			return
		}
		defer in.Close()
		defer out.Close()
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		go func() { _, _ = io.Copy(out, rw); out.Close() }()
		_, _ = io.Copy(in, out)
	}))
	t.Cleanup(server.Close)
	return server
}

func bindOfflineNodeTicket(t *testing.T, manager *proxy.CodexHarvestManager, account *auth.Account, proxyURL string) *atomic.Int32 {
	t.Helper()
	puts := &atomic.Int32{}
	group := map[string]any{"type": "Selector", "all": []string{"p-US"}, "now": "p-US"}
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts.Add(1)
			w.WriteHeader(204)
			return
		}
		switch r.URL.Path {
		case "/providers/proxies":
			_ = json.NewEncoder(w).Encode(map[string]any{"providers": map[string]any{"airport": map[string]any{"name": "airport", "vehicleType": "HTTP", "proxies": []any{map[string]any{"name": "p-US", "type": "Shadowsocks", "id": "runtime-1", "provider-name": "airport"}}}}})
		case "/proxies":
			_ = json.NewEncoder(w).Encode(map[string]any{"proxies": map[string]any{mihomo.HarvestSelectGroup: group}})
		default:
			_ = json.NewEncoder(w).Encode(group)
		}
	}))
	t.Cleanup(controller.Close)
	root := filepath.Join(manager.DataDir, "mihomo-codex")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "provider.yaml"), []byte("proxies:\n  - {name: US, type: ss, server: offline.invalid, port: 443, password: fixture}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(proxyURL)
	config := fmt.Sprintf("mixed-port: 1\nexternal-controller: %s\nlisteners:\n  - {name: codex-harvest-directed, type: mixed, listen: 127.0.0.1, port: %s, proxy: CODEX-HARVEST-SELECT}\nproxy-providers:\n  airport:\n    path: ./provider.yaml\n    override: {additional-prefix: p-}\n", controller.URL, u.Port())
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	sidecar, err := mihomo.LoadDirectedSidecar(manager.DataDir, proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := sidecar.Directory(t.Context())
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%v err=%v", nodes, err)
	}
	account.Mu().Lock()
	ticket := account.CodexTickets["gpt-6-astra"]
	ticket.HarvestProxyURL, ticket.HarvestNodeID, ticket.HarvestPoolID = proxyURL, nodes[0].ID, sidecar.PoolID
	account.Mu().Unlock()
	return puts
}

func TestGatewayWSNodeBoundCompleteTicketRetainsOriginalConnection(t *testing.T) {
	manager, account, server, executor, ctx := fixedGatewayExecutionFixture(t)
	localProxy := gatewayOfflineProxy(t, server.server.Listener.Addr().String())
	puts := bindOfflineNodeTicket(t, manager, account, localProxy.URL)
	expected, _ := url.Parse(localProxy.URL)
	executor.manager.dialer.NetDialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != expected.Host {
			return nil, fmt.Errorf("non-loopback/unexpected egress: %s", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	headers := http.Header{"Session-Id": {"node-fixed"}}
	first := gatewayExecuteAndRead(t, ctx, account, "node-first", "", headers)
	gatewayExecuteAndRead(t, ctx, account, "node-next", first, headers)
	if puts.Load() != 2 || server.dials.Load() != 1 {
		t.Fatalf("ticket leases=%d WS handshakes=%d; want two leases, same connection", puts.Load(), server.dials.Load())
	}
}
