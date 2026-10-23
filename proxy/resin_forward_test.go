package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// 测试代理只转发字节，HTTPS 的证书与 ClientHello 均由隧道两端处理。
func newResinForwardTestProxy(t *testing.T, dialTarget string, observe func(*http.Request)) *httptest.Server {
	t.Helper()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	t.Cleanup(transport.CloseIdleConnections)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if observe != nil {
			observe(r)
		}
		if r.Method != http.MethodConnect {
			forward := r.Clone(r.Context())
			forward.RequestURI = ""
			forward.Header.Del("Proxy-Authorization")
			resp, err := transport.RoundTrip(forward)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			for key, values := range resp.Header {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
			return
		}
		address := r.Host
		if dialTarget != "" {
			address = dialTarget
		}
		upstream, err := net.DialTimeout("tcp", address, time.Second)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer upstream.Close()
		client, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer client.Close()
		_, _ = io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
		done := make(chan struct{})
		go func() { _, _ = io.Copy(upstream, rw); _ = upstream.Close(); close(done) }()
		_, _ = io.Copy(client, upstream)
		_ = client.Close()
		<-done
	}))
	t.Cleanup(server.Close)
	return server
}

func TestResinForwardConfig(t *testing.T) {
	for _, tc := range []struct {
		name, mode, endpoint, platform string
		valid                          bool
	}{
		{"http", "forward", "http://localhost:2260/token", "p1,p2", true},
		{"https", "forward", "https://localhost/token", "p1", true},
		{"ipv6", "forward", "http://[::1]:2260/token", "p1", true},
		{"empty token", "forward", "http://localhost:2260", "p1", true},
		{"legacy mode", "", "http://localhost/token", "p1", true},
		{"unknown mode", "auto", "http://localhost/token", "p1", false},
		{"reverse prefix", "forward", "http://localhost/prefix/token", "p1", false},
		{"encoded slash", "forward", "http://localhost/token%2Fpart", "p1", false},
		{"userinfo", "forward", "http://user:pass@localhost/token", "p1", false},
		{"invalid port", "forward", "http://localhost:70000/token", "p1", false},
		{"platform delimiter", "forward", "http://localhost/token", "p.1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateResinConfig(&ResinConfig{Mode: tc.mode, BaseURL: tc.endpoint, PlatformName: tc.platform})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestResinForwardRouteSnapshotAndPoolIsolation(t *testing.T) {
	withResin(t, &ResinConfig{Mode: "forward", BaseURL: "http://resin.test:2260/token", PlatformName: "p1,p2", TraeCNPlatformName: "trae"})
	account := &auth.Account{DBID: 90741}
	ctx := WithResinPlatform(WithResinConfig(t.Context(), GetResinConfig()), "p2")
	const target = "https://chatgpt.com/path%2Fpart?q=%2B"
	first := ResolveCodexEgressForContext(ctx, account, target, "http://ignored:8080")
	if first.Err != nil || !first.ViaResin() || first.ResinMode != "forward" || first.URL != target {
		t.Fatalf("route=%+v", first)
	}
	u, _ := url.Parse(first.DialProxyURL)
	password, _ := u.User.Password()
	if u.Host != "resin.test:2260" || u.Path != "" || u.User.Username() != "p2.90741" || password != "token" {
		t.Fatal("incorrect proxy identity")
	}
	headers := http.Header{"X-Resin-Account": {"injected"}, "Proxy-Authorization": {"injected"}, "Authorization": {"Bearer upstream-token"}}
	first.ApplyHeaders(headers)
	if headers.Get("X-Resin-Account") != "" || headers.Get("Proxy-Authorization") != "" || headers.Get("Authorization") != "Bearer upstream-token" {
		t.Fatal("proxy and upstream authentication mixed")
	}
	SetResinConfig(&ResinConfig{Mode: "forward", BaseURL: "http://resin.test:2260/new-token", PlatformName: "p1"})
	pinned := ResolveCodexEgressForContext(ctx, account, target, "")
	changed := ResolveCodexEgress(account, target, "")
	otherPlatform := ResolveCodexEgressForContext(WithResinPlatform(ctx, "p1"), account, target, "")
	otherAccount := ResolveCodexEgressForContext(ctx, &auth.Account{DBID: 90742}, target, "")
	if pinned.DialProxyURL != first.DialProxyURL {
		t.Fatal("hot update changed the pinned route")
	}
	for _, other := range []CodexEgress{changed, otherPlatform, otherAccount} {
		if other.Client().Transport == first.Client().Transport {
			t.Fatal("different Resin identities shared a transport")
		}
	}
	trae := ResolveCodexEgressForContext(WithResinConfig(t.Context(), &ResinConfig{Mode: "forward", BaseURL: "http://resin.test/token", PlatformName: "shared", TraeCNPlatformName: "trae"}), &auth.Account{DBID: 90743, UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT"}, target, "")
	parsed, _ := url.Parse(trae.DialProxyURL)
	if parsed.User.Username() != "trae.90743" {
		t.Fatal("TRAE CN used the Codex platform")
	}
}

func TestResinForwardConnectCarriesTLSAndHeaders(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	var requests, connects atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.TLS == nil || r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("X-Resin-Account") != "" || r.Header.Get("Authorization") != "Bearer application" {
			t.Error("invalid upstream TLS or headers")
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(origin.Close)
	server := newResinForwardTestProxy(t, "", func(r *http.Request) {
		connects.Add(1)
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("p2.90744:token"))
		if r.Method != http.MethodConnect || r.Header.Get("Proxy-Authorization") != want {
			t.Error("incorrect CONNECT authentication")
		}
	})
	withResin(t, &ResinConfig{Mode: "forward", BaseURL: server.URL + "/token", PlatformName: "p1,p2"})
	ctx := WithResinPlatform(t.Context(), "p2")
	egress := ResolveCodexEgressForContext(ctx, &auth.Account{DBID: 90744}, origin.URL+"/responses", "http://127.0.0.1:1")
	client := egress.Client()
	transport := client.Transport.(*http.Transport)
	roots := x509.NewCertPool()
	roots.AddCert(origin.Certificate())
	transport.TLSClientConfig = &tls.Config{RootCAs: roots}
	t.Cleanup(client.CloseIdleConnections)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, egress.URL, nil)
	req.Header.Set("Authorization", "Bearer application")
	egress.ApplyHeaders(req.Header)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || requests.Load() != 1 || connects.Load() != 1 {
		t.Fatalf("status=%d requests=%d connects=%d", resp.StatusCode, requests.Load(), connects.Load())
	}
}

func TestResinForwardPreservesUTLSClientHello(t *testing.T) {
	for _, purpose := range []string{"inference", "subscription", "session-refresh"} {
		t.Run(purpose, func(t *testing.T) {
			t.Setenv("CODEX_TRANSPORT_MODE", "standard")
			if purpose == "inference" {
				t.Setenv("CODEX_TRANSPORT_MODE", "utls_chrome")
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			type hello struct {
				server     string
				grease, h2 bool
			}
			captured := make(chan hello, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				server := tls.Server(conn, &tls.Config{GetConfigForClient: func(info *tls.ClientHelloInfo) (*tls.Config, error) {
					got := hello{server: info.ServerName}
					for _, cipher := range info.CipherSuites {
						if cipher&0x0f0f == 0x0a0a {
							got.grease = true
						}
					}
					for _, protocol := range info.SupportedProtos {
						if protocol == "h2" {
							got.h2 = true
						}
					}
					captured <- got
					return nil, errors.New("ClientHello captured")
				}})
				_ = server.Handshake()
			}()
			proxyServer := newResinForwardTestProxy(t, listener.Addr().String(), nil)
			withResin(t, &ResinConfig{Mode: "forward", BaseURL: proxyServer.URL + "/token", PlatformName: "chrome"})
			account := &auth.Account{DBID: 90745}
			const target = "https://upstream.test/responses"
			var client *http.Client
			switch purpose {
			case "subscription":
				client = resolveCodexMaintenanceEgress(t.Context(), account, target, "", maintenancePurposeSubscription, true).Client()
			case "session-refresh":
				route, err := ResolveResinAuthRoute(t.Context(), target, "90745")
				if err != nil {
					t.Fatal(err)
				}
				client, err = route.HTTPClient(3*time.Second, true)
				if err != nil {
					t.Fatal(err)
				}
			default:
				client = ResolveCodexEgress(account, target, "").Client()
			}
			t.Cleanup(client.CloseIdleConnections)
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
			if resp, err := client.Do(req); err == nil {
				resp.Body.Close()
				t.Fatal("test TLS server should stop after ClientHello")
			}
			select {
			case got := <-captured:
				if got.server != "upstream.test" || !got.grease || !got.h2 {
					t.Fatalf("uTLS ClientHello lost: %+v", got)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("upstream did not receive ClientHello")
			}
		})
	}
}

func TestResinForwardRejectsWithoutDirectFallback(t *testing.T) {
	var upstreamCalls atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { upstreamCalls.Add(1) }))
	defer origin.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusProxyAuthRequired) }))
	defer server.Close()
	withResin(t, &ResinConfig{Mode: "forward", BaseURL: server.URL + "/private-token", PlatformName: "p1"})
	for _, mode := range []string{"standard", "utls_chrome"} {
		t.Setenv("CODEX_TRANSPORT_MODE", mode)
		egress := ResolveCodexEgress(&auth.Account{DBID: 90746}, origin.URL, "")
		resp, err := egress.Client().Get(egress.URL)
		if resp != nil {
			resp.Body.Close()
		}
		if err == nil || strings.Contains(err.Error(), "private-token") || upstreamCalls.Load() != 0 {
			t.Fatalf("mode=%s err=%v upstream=%d", mode, err, upstreamCalls.Load())
		}
	}
}

func TestResinForwardTelemetryUsesPinnedSnapshot(t *testing.T) {
	var proxyCalls atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Resin-Account") != "" {
			t.Error("Resin identity leaked upstream")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer origin.Close()
	server := newResinForwardTestProxy(t, "", func(r *http.Request) {
		proxyCalls.Add(1)
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("p2.90747:token"))
		if r.Header.Get("Proxy-Authorization") != want {
			t.Error("telemetry changed platform")
		}
	})
	withResin(t, &ResinConfig{Mode: "forward", BaseURL: server.URL + "/token", PlatformName: "p1,p2"})
	ctx := WithResinPlatform(WithResinConfig(context.Background(), GetResinConfig()), "p2")
	SetResinConfig(nil)
	job := codexTelemetryJob{url: origin.URL, body: []byte(`{}`), client: codexTelemetryClient{account: &auth.Account{DBID: 90747}, egressContext: ctx}}
	if err := sendCodexTelemetryJob(job); err != nil {
		t.Fatal(err)
	}
	if proxyCalls.Load() != 1 {
		t.Fatal("telemetry bypassed Resin")
	}
}

func TestResinForwardTraeCNInference(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	var calls atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != auth.TraeCNChatPath || r.Header.Get("x-ide-token") != "AT" || r.Header.Get("X-Resin-Account") != "" || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("incorrect TRAE CN route or headers")
		}
		traeCNResinTestResponse(w)
	}))
	defer origin.Close()
	server := newResinForwardTestProxy(t, "", func(r *http.Request) {
		calls.Add(1)
		want := "Basic " + base64.StdEncoding.EncodeToString([]byte("trae.90748:token"))
		if r.Header.Get("Proxy-Authorization") != want {
			t.Error("TRAE CN did not use its own platform")
		}
	})
	ctx := WithResinConfig(t.Context(), &ResinConfig{Mode: "forward", BaseURL: server.URL + "/token", CodexPlatformName: "codex", TraeCNPlatformName: "trae"})
	account := &auth.Account{DBID: 90748, UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT", ExpiresAt: time.Now().Add(time.Hour), TraeCNHost: origin.URL, ProxyURL: "http://127.0.0.1:1"}
	body := []byte(`{"model":"deepseek-v3","input":"hi","stream":true}`)
	resp, err := ExecuteTraeCNRequest(ctx, account, GrokProtocolResponses, body, body, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil || calls.Load() != 1 {
		t.Fatalf("calls=%d err=%v", calls.Load(), err)
	}
}

func TestResinForwardInheritLeaseUsesOAuthSnapshot(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/token/api/v1/p2/actions/inherit-lease" || r.Header.Get("Proxy-Authorization") != "" {
			t.Errorf("unexpected lease request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["parent_account"] != "oauth-session" || body["new_account"] != "90749" {
			t.Errorf("incorrect lease inheritance: %v, %v", body, err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	withResin(t, &ResinConfig{Mode: "forward", BaseURL: server.URL + "/token", PlatformName: "p1,p2"})
	ctx, cancel := context.WithCancel(WithResinPlatform(WithResinConfig(t.Context(), GetResinConfig()), "p2"))
	SetResinConfig(nil)
	cancel()
	InheritLease(ctx, "oauth-session", "90749")
	if calls.Load() != 1 {
		t.Fatal("lease inheritance lost the OAuth snapshot")
	}
}

func TestResinForwardInviteUsesMaintenanceRoute(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer AT" || r.Header.Get("X-Resin-Account") != "" || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("incorrect invite authentication")
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	defer origin.Close()
	var calls atomic.Int32
	server := newResinForwardTestProxy(t, "", func(r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("p1.90750:token")) {
			t.Error("incorrect invite proxy identity")
		}
	})
	ctx := WithResinConfig(t.Context(), &ResinConfig{Mode: "forward", BaseURL: server.URL + "/token", PlatformName: "p1"})
	account := &auth.Account{DBID: 90750, AccessToken: "AT"}
	req, err := newInviteRequest(ctx, account, http.MethodGet, origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	status, _, _, _, err := doInviteRequest(req, account, "http://127.0.0.1:1", true)
	if err != nil || status != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("status=%d calls=%d err=%v", status, calls.Load(), err)
	}
}

// WS 失败后的 HTTP 重试会重新进入执行器，必须沿用入口固定的模式和地址。
func TestResinModeUpdateDoesNotChangeHTTPFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousSettings, previousExecutor := CurrentRuntimeSettings(), WebsocketExecuteFunc
	next := previousSettings
	next.CodexForceWebsocket = true
	ApplyRuntimeSettings(next)
	t.Cleanup(func() {
		ApplyRuntimeSettings(previousSettings)
		WebsocketExecuteFunc = previousExecutor
	})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("X-Resin-Account") != "90751" || !strings.HasPrefix(r.URL.Path, "/old-token/p1/https/") {
			t.Error("HTTP fallback lost the original Resin identity")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"pinned-egress\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	}))
	defer upstream.Close()
	withResin(t, &ResinConfig{Mode: "reverse", BaseURL: upstream.URL + "/old-token", PlatformName: "p1"})
	WebsocketExecuteFunc = func(context.Context, *auth.Account, []byte, string, string, string, *DeviceProfileConfig, http.Header, string) (*http.Response, error) {
		SetResinConfig(&ResinConfig{Mode: "forward", BaseURL: "http://127.0.0.1:1/new-token", PlatformName: "p2"})
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: errReadCloser{err: errors.New("websocket read error: websocket: close 1009 (message too big)")}}, nil
	}
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 90751, AccessToken: "AT", AccountID: "workspace", PlanType: "pro"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	handler.RegisterRoutes(router)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK || calls.Load() != 1 || !strings.Contains(recorder.Body.String(), "pinned-egress") {
		t.Fatalf("status=%d calls=%d body=%s", recorder.Code, calls.Load(), recorder.Body)
	}
}
