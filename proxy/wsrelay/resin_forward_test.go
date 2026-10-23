package wsrelay

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
)

func TestResinForwardWebsocketTunnelAndContinuationIsolation(t *testing.T) {
	previous := proxy.GetResinConfig()
	t.Cleanup(func() { proxy.SetResinConfig(previous) })
	upgrader := websocket.Upgrader{}
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Resin-Account") != "" || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy credentials reached WebSocket origin")
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(origin.Close)
	var connects atomic.Int32
	resin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connects.Add(1)
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("p2.90801:token"))
		if r.Method != http.MethodConnect || r.Header.Get("Proxy-Authorization") != want {
			t.Error("incorrect Resin CONNECT identity")
		}
		upstream, err := net.DialTimeout("tcp", r.Host, time.Second)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer upstream.Close()
		client, reader, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer client.Close()
		_, _ = io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
		done := make(chan struct{})
		go func() { _, _ = io.Copy(upstream, reader); _ = upstream.Close(); close(done) }()
		_, _ = io.Copy(client, upstream)
		_ = client.Close()
		<-done
	}))
	t.Cleanup(resin.Close)
	proxy.SetResinConfig(&proxy.ResinConfig{Mode: "forward", BaseURL: resin.URL + "/token", PlatformName: "p1,p2"})
	ctx := proxy.WithResinPlatform(proxy.WithResinConfig(t.Context(), proxy.GetResinConfig()), "p2")
	proxy.SetResinConfig(nil)
	account := &auth.Account{DBID: 90801, AccessToken: "AT", ProxyURL: "http://127.0.0.1:1"}
	manager := NewManager()
	t.Cleanup(manager.Stop)
	manager.probeFunc = func(*WsConnection) bool { return true }
	roots := x509.NewCertPool()
	roots.AddCert(origin.Certificate())
	manager.dialer.TLSClientConfig = &tls.Config{RootCAs: roots}
	wsURL := "wss" + strings.TrimPrefix(origin.URL, "https") + "/responses"
	egress := proxy.ResolveCodexWebsocketEgressForContext(ctx, account, wsURL, account.ProxyURL)
	if egress.Err != nil || egress.URL != wsURL {
		t.Fatalf("egress=%+v", egress)
	}
	headers := http.Header{}
	egress.ApplyHeaders(headers)
	wc, pending, err := manager.AcquireConnection(ctx, account, egress.URL, "session", headers, account.ProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	wc.session.RemovePendingRequest(pending.RequestID)
	manager.ReleaseConnection(wc)
	manager.BindResponseConn("resp_forward", wc, "session", account.ID(), "key")
	input := websocketContinuation{responseID: "resp_forward", accountID: account.ID(), apiKey: "key", url: wsURL, proxyURL: effectiveProxyURLForContext(ctx, account, "")}
	if got, _ := manager.lookupContinuationConn(input); got != wc {
		t.Fatal("same route lost its continuation")
	}
	otherCtx := proxy.WithResinPlatform(ctx, "p1")
	input.proxyURL = effectiveProxyURLForContext(otherCtx, account, "")
	if got, _ := manager.lookupContinuationConn(input); got != nil {
		t.Fatal("continuation crossed Resin platforms")
	}
	rotatedCtx := proxy.WithResinPlatform(proxy.WithResinConfig(t.Context(), &proxy.ResinConfig{Mode: "forward", BaseURL: resin.URL + "/rotated-token", PlatformName: "p2"}), "p2")
	input.proxyURL = effectiveProxyURLForContext(rotatedCtx, account, "")
	if got, _ := manager.lookupContinuationConn(input); got != nil {
		t.Fatal("continuation reused a retired proxy credential")
	}
	if connects.Load() != 1 {
		t.Fatalf("CONNECT count=%d", connects.Load())
	}
}
