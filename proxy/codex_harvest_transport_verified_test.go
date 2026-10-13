package proxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestCodexHarvestWebsocket780Verification(t *testing.T) {
	for _, mode := range []string{"verified", "handshake only", "reissued", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			withCodexTicketGate(t, "gpt-6-astra")
			settings := auth.ConfiguredCodexTicketSettings()
			settings.TargetLength = 780
			auth.SetConfiguredCodexTicketSettings(settings)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx = context.WithValue(ctx, harvestProtocolKey{}, "websocket")
			state := testTicketState(time.Now(), 33)
			claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix())))
			cookie := "__cflb=a; __oailb=h." + claims + ".s"
			var session atomic.Value
			var requests atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := requests.Add(1)
				headers := http.Header{}
				if n == 1 {
					session.Store(r.Header.Get("session_id"))
					if r.Header.Get("Cookie") != "" || r.Header.Get(codexTurnStateHeader) != "" {
						t.Error("首轮混入了旧材料")
					}
					headers.Add("Set-Cookie", "__cflb=a; Path=/")
					headers.Add("Set-Cookie", "__oailb=h."+claims+".s; Path=/")
				} else if r.Header.Get("session_id") != session.Load() || r.Header.Get("Cookie") != cookie || r.Header.Get(codexTurnStateHeader) != state {
					t.Error("续接的会话、Cookie 或 state 不符")
				}
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, headers)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				_, body, err := conn.ReadMessage()
				if err != nil {
					t.Error(err)
					return
				}
				if n == 2 && gjson.GetBytes(body, "client_metadata."+codexTurnStateMetadataKey).String() != state {
					t.Error("续接帧未携带 state")
				}
				if mode == "handshake only" {
					return
				}
				if mode == "cancelled" {
					cancel()
					_, _, _ = conn.ReadMessage()
					return
				}
				issued := state
				if mode == "reissued" && n == 2 {
					issued = testTicketState(time.Now().Add(-time.Second), 33)
				}
				_ = conn.WriteJSON(map[string]any{"type": "codex.response.metadata", "headers": map[string]string{"x-codex-turn-state": issued}})
				_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]string{"model": "gpt-6-astra"}})
			}))
			defer upstream.Close()
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodConnect {
					w.WriteHeader(400)
					return
				}
				remote, err := net.Dial("tcp", strings.TrimPrefix(upstream.URL, "http://"))
				if err != nil {
					t.Error(err)
					w.WriteHeader(502)
					return
				}
				local, buffer, err := w.(http.Hijacker).Hijack()
				if err != nil {
					remote.Close()
					t.Error(err)
					return
				}
				_, _ = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
				_ = buffer.Flush()
				go func() { defer local.Close(); defer remote.Close(); _, _ = io.Copy(remote, local) }()
				go func() { defer local.Close(); defer remote.Close(); _, _ = io.Copy(local, remote) }()
			}))
			defer gateway.Close()
			old := codexTicketProbeURLForTest
			codexTicketProbeURLForTest = upstream.URL
			defer func() { codexTicketProbeURLForTest = old }()
			result := fireCodexTicketHarvest(ctx, &auth.Account{AccessToken: "test-only"}, "gpt-6-astra", gateway.URL, 3*time.Second)
			if mode == "verified" {
				if result.Result != "success" || requests.Load() != 2 || result.Binding.Transport != "websocket" || !result.Binding.ProbeVerified {
					t.Fatalf("WS 验收失败: %s %v", result.Result, result.Err)
				}
			} else if result.Result == "success" || result.State != "" {
				t.Fatal("未通过续接的 WS 票被接受")
			}
		})
	}
}

func TestCodexHarvestEdgePreservesHostAndTLSVerification(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "example.com:443" || r.TLS.ServerName != "example.com" {
			t.Error("Host 或 SNI 未保留")
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	for _, trusted := range []bool{true, false} {
		client := server.Client()
		if !trusted {
			client = &http.Client{Transport: &http.Transport{}}
		}
		transport := client.Transport.(*http.Transport)
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr != "8.8.8.8:443" {
				t.Errorf("拨号目的地址错误: %s", addr)
			}
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		}
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com:443/responses", nil)
		request, edgeClient, err := codexHarvestEdgeClient(req, client, "8.8.8.8")
		if err != nil {
			t.Fatal(err)
		}
		if edgeClient.Transport.(*http.Transport).TLSClientConfig.InsecureSkipVerify || req.URL.Host != "example.com:443" {
			t.Fatal("边缘 IP 修改了证书验证或原请求")
		}
		resp, err := edgeClient.Do(request)
		if trusted && err != nil {
			t.Fatal(err)
		}
		if !trusted && err == nil {
			t.Error("不可信证书被接受")
		}
		if resp != nil {
			resp.Body.Close()
		}
		edgeClient.CloseIdleConnections()
	}
}
