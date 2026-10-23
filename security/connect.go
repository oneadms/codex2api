package security

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// HTTPConnectDialer 为 HTTP、uTLS 和 WebSocket 共用的隧道拨号器。
// HTTPS 代理先完成代理侧 TLS，再在 CONNECT 隧道内由调用方握手上游 TLS。
type HTTPConnectDialer struct {
	proxyURL  *url.URL
	tlsConfig *tls.Config
}

func NewHTTPConnectDialer(proxyURL *url.URL) *HTTPConnectDialer {
	return &HTTPConnectDialer{proxyURL: proxyURL}
}

func (d *HTTPConnectDialer) Dial(network, addr string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, addr)
}

func (d *HTTPConnectDialer) DialContext(ctx context.Context, network, addr string) (_ net.Conn, err error) {
	proxyAddr := d.proxyURL.Host
	if d.proxyURL.Port() == "" {
		port := "80"
		if d.proxyURL.Scheme == "https" {
			port = "443"
		}
		proxyAddr = net.JoinHostPort(d.proxyURL.Hostname(), port)
	}
	setupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext(setupCtx, network, proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("连接代理服务器失败: %w", err)
	}
	// CONNECT 读写也受取消和超时约束，不能把握手挂起变成永久占用。
	rawConn := conn
	stop := context.AfterFunc(setupCtx, func() { _ = rawConn.Close() })
	defer stop()
	defer func() {
		if err != nil {
			_ = rawConn.Close()
		}
	}()
	deadline, _ := setupCtx.Deadline()
	_ = conn.SetDeadline(deadline)
	if d.proxyURL.Scheme == "https" {
		cfg := &tls.Config{ServerName: d.proxyURL.Hostname()}
		if d.tlsConfig != nil {
			cfg = d.tlsConfig.Clone()
			cfg.ServerName = d.proxyURL.Hostname()
		}
		proxyTLS := tls.Client(conn, cfg)
		if err = proxyTLS.HandshakeContext(setupCtx); err != nil {
			return nil, fmt.Errorf("代理 TLS 握手失败: %w", err)
		}
		conn = proxyTLS
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: addr}, Host: addr, Header: make(http.Header)}
	if user := d.proxyURL.User; user != nil {
		password, _ := user.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user.Username()+":"+password)))
	}
	if err = req.Write(conn); err != nil {
		return nil, fmt.Errorf("发送代理 CONNECT 失败: %w", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		return nil, fmt.Errorf("读取代理 CONNECT 响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("代理 CONNECT 失败 (status %d)", resp.StatusCode)
	}
	if err = setupCtx.Err(); err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	if reader.Buffered() > 0 {
		return &connectBufferedConn{Conn: conn, reader: reader}, nil
	}
	return conn, nil
}

type connectBufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *connectBufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
