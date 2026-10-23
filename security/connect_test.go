package security

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestHTTPConnectDialerTunnel(t *testing.T) {
	for _, secure := range []bool{false, true} {
		name := "http"
		if secure {
			name = "https"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodConnect || r.Host != "upstream.test:443" {
					t.Errorf("unexpected CONNECT: %s %s", r.Method, r.Host)
				}
				wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("platform.42:token"))
				if r.Header.Get("Proxy-Authorization") != wantAuth {
					t.Error("missing proxy identity")
				}
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				// 响应后的字节模拟隧道数据，验证缓冲区不会丢失。
				_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\nready")
				_, _ = io.Copy(conn, conn)
			}))
			if secure {
				server.StartTLS()
			} else {
				server.Start()
			}
			t.Cleanup(server.Close)
			endpoint, _ := url.Parse(server.URL)
			endpoint.User = url.UserPassword("platform.42", "token")
			dialer := NewHTTPConnectDialer(endpoint)
			if secure {
				roots := x509.NewCertPool()
				roots.AddCert(server.Certificate())
				dialer.tlsConfig = &tls.Config{RootCAs: roots}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			conn, err := dialer.DialContext(ctx, "tcp", "upstream.test:443")
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			got := make([]byte, 5)
			if _, err := io.ReadFull(conn, got); err != nil || string(got) != "ready" {
				t.Fatalf("buffered tunnel bytes=%q err=%v", got, err)
			}
			_, _ = io.WriteString(conn, "hello")
			if _, err := io.ReadFull(conn, got); err != nil || string(got) != "hello" {
				t.Fatalf("tunnel echo=%q err=%v", got, err)
			}
		})
	}
}

func TestHTTPConnectDialerRejectsAndCancels(t *testing.T) {
	for _, status := range []int{http.StatusProxyAuthRequired, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
			defer server.Close()
			u, _ := url.Parse(server.URL)
			u.User = url.UserPassword("p.42", "secret-never-log")
			conn, err := NewHTTPConnectDialer(u).DialContext(t.Context(), "tcp", "upstream.test:443")
			if conn != nil || err == nil || strings.Contains(err.Error(), "secret-never-log") {
				t.Fatalf("conn=%v err=%v", conn, err)
			}
		})
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-release }))
	defer server.Close()
	defer close(release)
	u, _ := url.Parse(server.URL)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		conn, err := NewHTTPConnectDialer(u).DialContext(ctx, "tcp", "upstream.test:443")
		if conn != nil {
			conn.Close()
		}
		done <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled CONNECT succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled CONNECT did not stop")
	}
}
