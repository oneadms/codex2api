package wsrelay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
)

func TestExecuteRequestViaWebsocketKeepsResinSnapshot(t *testing.T) {
	previous := proxy.GetResinConfig()
	previousFlag := auth.ResinEgressEnabled()
	t.Cleanup(func() {
		proxy.SetResinConfig(previous)
		auth.SetResinEgressEnabled(previousFlag)
	})

	type capturedRequest struct {
		path    string
		account string
	}
	captured := make(chan capturedRequest, 1)
	upgrader := websocket.Upgrader{}
	resin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer conn.Close()
		captured <- capturedRequest{path: r.URL.Path, account: r.Header.Get("X-Resin-Account")}
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_snapshot","status":"completed","output":[]}}`)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	t.Cleanup(resin.Close)
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: resin.URL + "/token", PlatformName: "p1,p2"})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ctx = proxy.WithResinPlatform(proxy.WithResinConfig(ctx, proxy.GetResinConfig()), "p2")
	proxy.SetResinConfig(nil)

	manager := NewManager()
	t.Cleanup(manager.Stop)
	// The request snapshot must suppress both account and inherited dialer proxies.
	manager.dialer.Proxy = func(*http.Request) (*url.URL, error) {
		return url.Parse("http://127.0.0.1:1")
	}
	account := &auth.Account{DBID: 91004, AccessToken: "AT", ProxyURL: "http://127.0.0.1:1"}
	executor := NewExecutorWithManager(manager)
	resp, err := executor.ExecuteRequestViaWebsocket(ctx, account, []byte(`{"model":"gpt-5.4","input":"hello"}`), "session", "", "key", nil, nil, "")
	if err != nil {
		t.Fatalf("ExecuteRequestViaWebsocket: %v", err)
	}
	defer resp.Close()
	if err := resp.ReadStream(func([]byte) bool { return true }); err != nil {
		t.Fatalf("read response: %v", err)
	}
	select {
	case got := <-captured:
		const wantPath = "/token/p2/https/chatgpt.com/backend-api/codex/responses"
		if got.path != wantPath || got.account != "91004" {
			t.Fatalf("WS route = %+v, want path=%q, account=91004", got, wantPath)
		}
	default:
		t.Fatal("WebSocket request did not reach the pinned Resin endpoint")
	}
}
