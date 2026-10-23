package auth

import (
	"context"
	"net/http"
	"strings"
	"time"
)

type resinResolverContextKey struct{}
type resinContextResolver func(context.Context, string, string) (ResinRoute, error)
type resinChannelContextKey struct{}

const (
	ResinModeReverse = "reverse"
	ResinModeForward = "forward"
)

// ResinRoute 显式保存出口方式；正向代理保持原 URL，不能通过 URL 变化判断是否启用。
type ResinRoute struct {
	Mode      string
	URL       string
	ProxyURL  string
	AccountID string
}

// ResinRequestResolver 由入口注入，避免 auth 反向依赖 proxy。
var ResinRequestResolver func(context.Context, string, string) (ResinRoute, error)

func (r ResinRoute) Enabled() bool { return r.Mode != "" }

func (r ResinRoute) ApplyHeaders(headers http.Header) {
	if headers == nil {
		return
	}
	if r.Mode == ResinModeReverse {
		headers.Set("X-Resin-Account", r.AccountID)
	} else if r.Mode == ResinModeForward {
		// 正代身份只进入代理认证，不能泄漏到上游业务请求。
		headers.Del("X-Resin-Account")
		headers.Del("Proxy-Authorization")
	}
}

// HTTPClient 保留调用方的 TLS 策略，并复用按实际代理凭据隔离的鉴权连接池。
func (r ResinRoute) HTTPClient(timeout time.Duration, useUTLS bool) (*http.Client, error) {
	if r.Mode == ResinModeReverse {
		return NewResinHTTPClient(timeout), nil
	}
	base, err := BuildHTTPClientChecked(r.ProxyURL)
	if err != nil {
		return nil, err
	}
	if useUTLS {
		base = buildUTLSHTTPClient(r.ProxyURL)
	}
	client := *base
	client.Timeout = timeout
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &client, nil
}

// WithResinChannel 标记鉴权请求所属链路，避免 TRAE CN 刷新使用 Codex 的平台。
func WithResinChannel(ctx context.Context, channel string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, resinChannelContextKey{}, channel)
}

func ResinChannelFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	channel, _ := ctx.Value(resinChannelContextKey{}).(string)
	return channel
}

// WithResinRequestResolver 将鉴权请求绑定到调用方已选定的 Resin 配置，
// 避免测试连接期间再次读取进程全局配置而切换出口。
func WithResinRequestResolver(ctx context.Context, resolver func(context.Context, string, string) (ResinRoute, error)) context.Context {
	return context.WithValue(ctx, resinResolverContextKey{}, resinContextResolver(resolver))
}

// ResolveResinRequest 优先使用请求内的路由快照；显式空解析器表示本次禁用 Resin。
func ResolveResinRequest(ctx context.Context, targetURL, accountID string) (ResinRoute, error) {
	direct := ResinRoute{URL: targetURL}
	if strings.TrimSpace(accountID) == "" {
		return direct, nil
	}
	if ctx != nil {
		if resolver, ok := ctx.Value(resinResolverContextKey{}).(resinContextResolver); ok {
			if resolver == nil {
				return direct, nil
			}
			return resolver(ctx, targetURL, accountID)
		}
	}
	if resolver := ResinRequestResolver; resolver != nil {
		return resolver(ctx, targetURL, accountID)
	}
	return direct, nil
}

var resinAuthTransport = func() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return transport
}()

// NewResinHTTPClient 直接连接 Resin，不叠加环境代理，也不跟随重定向绕回上游。
func NewResinHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: resinAuthTransport,
		Timeout:   timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
