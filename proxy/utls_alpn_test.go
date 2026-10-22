package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// relayHTTP1OnlyServer 起一个只支持 HTTP/1.1 的 TLS 上游（ALPN 只宣告 http/1.1），
// 模拟 new-api 系中转网关（如 ddl.ddpapi.top）。
//
// 修复前 uTLS transport 不看 ALPN、无条件按 h2 发帧：对端回 HTTP/1.1 文本响应，
// 客户端读帧头时报
// "http2: frame too large, note that the frame header looked like an HTTP/1.1 header"。
func relayHTTP1OnlyServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = false
	server.TLS = &tls.Config{NextProtos: []string{"http/1.1"}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

// trustServerTransport 构造一个信任测试服务端自签证书的 HTTP/1.1 transport。
// 生产环境的回退路径用系统根证书，测试需要注入测试根才能握手成功。
func trustServerTransport(t *testing.T, server *httptest.Server) *http.Transport {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	transport.TLSClientConfig = &tls.Config{RootCAs: pool}
	return transport
}

// TestUTLSNegotiatedHTTP2OnlyAcceptsH2 锁定 ALPN 判定本身：
// 只有协商结果恰好是 h2 才按 HTTP/2 发帧；空值（对端不支持 ALPN）与
// "http/1.1" 都必须判为 HTTP/1.1。
func TestUTLSNegotiatedHTTP2OnlyAcceptsH2(t *testing.T) {
	for _, tc := range []struct {
		name       string
		negotiated string
		want       bool
	}{
		{"h2", http2.NextProtoTLS, true},
		{"http1.1", "http/1.1", false},
		{"empty (no ALPN)", "", false},
		{"spdy", "spdy/3.1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := utls.ConnectionState{NegotiatedProtocol: tc.negotiated}
			if got := utlsNegotiatedHTTP2(state); got != tc.want {
				t.Fatalf("utlsNegotiatedHTTP2(%q) = %v, want %v", tc.negotiated, got, tc.want)
			}
		})
	}
}

// TestUTLSTransportFallsBackToHTTP1WhenALPNOmitsH2 验证端到端行为：
// 已知只能走 HTTP/1.1 的上游，请求必须由 h1 回退路径完成，而不是抛协议错配错误。
func TestUTLSTransportFallsBackToHTTP1WhenALPNOmitsH2(t *testing.T) {
	sawProto := ""
	server := relayHTTP1OnlyServer(t, func(w http.ResponseWriter, r *http.Request) {
		sawProto = r.Proto
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})

	rt := NewUTLSTransport("").(*utlsRoundTripper)
	rt.fallback = trustServerTransport(t, server)

	// 模拟「上一次请求已完成 ALPN 探测并记录该 host 只说 HTTP/1.1」。
	// 探测本身由 createConnection 完成（测试服务端是自签证书，无法在单测里
	// 走真实的 uTLS 握手），这里直接预置探测结论。
	host := server.Listener.Addr().String()
	hostname := "127.0.0.1"
	rt.mu.Lock()
	rt.http1Hosts[hostname] = struct{}{}
	rt.mu.Unlock()

	req, err := http.NewRequest(http.MethodGet, "https://"+host+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("HTTP/1.1 上游被协议错配打断: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if resp.ProtoMajor != 1 || sawProto != "HTTP/1.1" {
		t.Fatalf("proto = %s (upstream saw %q), want HTTP/1.1", resp.Proto, sawProto)
	}
}

// TestUTLSTransportRecordsHTTP1OnlyHost 验证探测结论会被记住：
// 一旦某 host 协商不出 h2，后续请求不再重复做注定失败的 h2 握手。
func TestUTLSTransportRecordsHTTP1OnlyHost(t *testing.T) {
	rt := NewUTLSTransport("").(*utlsRoundTripper)
	rt.http1Hosts["relay.example"] = struct{}{}

	if _, err := rt.getOrCreateConnection("relay.example", "relay.example:443"); err == nil {
		t.Fatal("已知 h1-only 的上游仍尝试建立 h2 连接")
	} else if !isUTLSHTTP1Only(err) {
		t.Fatalf("err = %v, want errUTLSHTTP1Only", err)
	}
}

// TestUTLSHTTP1FallbackTransportDisablesH2 确认回退 transport 不会协商 h2：
// 这条路径存在的意义就是服务只支持 HTTP/1.1 的上游。
func TestUTLSHTTP1FallbackTransportDisablesH2(t *testing.T) {
	rt := NewUTLSTransport("").(*utlsRoundTripper)
	if rt.fallback == nil {
		t.Fatal("缺少 HTTP/1.1 回退 transport：h1-only 上游会直接失败")
	}
	if rt.fallback.ForceAttemptHTTP2 {
		t.Fatal("回退 transport 仍会尝试 HTTP/2")
	}
	if len(rt.fallback.TLSNextProto) != 0 {
		t.Fatalf("回退 transport 注册了 h2 处理器: %d", len(rt.fallback.TLSNextProto))
	}
	if rt.fallback.Proxy != nil {
		t.Fatal("回退 transport 不应使用 Proxy 字段（隧道由 dialer 负责）")
	}
}
