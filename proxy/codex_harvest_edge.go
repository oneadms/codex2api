package proxy

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"

	"github.com/codex2api/internal/harvest"
)

type harvestEdgeKey struct{}

// 仅更改拨号目的地址，Host、SNI 和证书验证始终使用原上游域名。
func codexHarvestEdgeClient(req *http.Request, client *http.Client, edge string) (*http.Request, *http.Client, error) {
	if err := harvest.ValidateEdgeIP(edge); err != nil {
		return nil, nil, err
	}
	if edge == "" {
		return req, client, nil
	}
	base, ok := client.Transport.(*http.Transport)
	if !ok {
		return nil, nil, fmt.Errorf("采票传输无法指定边缘 IP")
	}
	transport := base.Clone()
	transport.DisableKeepAlives = true
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	transport.TLSClientConfig.ServerName = req.URL.Hostname()
	clone := req.Clone(req.Context())
	clone.Host = req.URL.Host
	port := req.URL.Port()
	if port == "" {
		port = "443"
		if req.URL.Scheme == "http" || req.URL.Scheme == "ws" {
			port = "80"
		}
	}
	clone.URL.Host = net.JoinHostPort(edge, port)
	copy := *client
	copy.Transport = transport
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return clone, &copy, nil
}
