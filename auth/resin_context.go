package auth

import (
	"context"
	"net/http"
	"strings"
	"time"
)

type resinDecoratorContextKey struct{}
type resinContextDecorator func(context.Context, string, string) string

// WithResinRequestDecorator 将鉴权请求绑定到调用方已选定的 Resin 配置，
// 避免测试连接期间再次读取进程全局配置而切换出口。
func WithResinRequestDecorator(ctx context.Context, decorator func(context.Context, string, string) string) context.Context {
	return context.WithValue(ctx, resinDecoratorContextKey{}, resinContextDecorator(decorator))
}

// ResinRequestURL 优先使用请求内的路由快照；显式空装饰器表示本次禁用 Resin。
func ResinRequestURL(ctx context.Context, targetURL, accountID string) (string, bool) {
	if strings.TrimSpace(accountID) == "" {
		return targetURL, false
	}
	if ctx != nil {
		if decorator, ok := ctx.Value(resinDecoratorContextKey{}).(resinContextDecorator); ok {
			if decorator == nil {
				return targetURL, false
			}
			finalURL := decorator(ctx, targetURL, accountID)
			return finalURL, finalURL != targetURL
		}
	}
	if decorator := ResinRequestDecorator; decorator != nil {
		finalURL := decorator(targetURL, accountID)
		return finalURL, finalURL != targetURL
	}
	return targetURL, false
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
