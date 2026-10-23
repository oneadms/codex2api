package proxy

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/codex2api/auth"
	"github.com/codex2api/security"
)

func (cfg *ResinConfig) mode() string {
	if cfg == nil || strings.TrimSpace(cfg.Mode) == "" {
		return auth.ResinModeReverse
	}
	return strings.TrimSpace(cfg.Mode)
}

// resinForwardEndpoint 只接受直接的代理入口；反代路径前缀不能用于 CONNECT。
func resinForwardEndpoint(raw string) (*url.URL, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, "", fmt.Errorf("Resin 正向代理地址必须是有效的 HTTP 或 HTTPS 地址")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, "", fmt.Errorf("Resin 正向代理地址不能包含用户信息、查询参数或片段")
	}
	token := strings.TrimSuffix(strings.TrimPrefix(parsed.Path, "/"), "/")
	if strings.ContainsAny(token, ".:|/\\@?#%~ \t\r\n") {
		return nil, "", fmt.Errorf("Resin 正向代理 Token 不能包含分隔符、空白或反代路径前缀")
	}
	endpoint := &url.URL{Scheme: parsed.Scheme, Host: parsed.Host}
	if _, err := security.ParseProxyURL(endpoint.String()); err != nil {
		return nil, "", fmt.Errorf("Resin 正向代理地址或端口无效")
	}
	return endpoint, token, nil
}

func buildResinRoute(cfg *ResinConfig, targetURL, platform, accountID string) (auth.ResinRoute, error) {
	route := auth.ResinRoute{URL: targetURL}
	if cfg == nil || strings.TrimSpace(accountID) == "" {
		return route, nil
	}
	if err := ValidateResinConfig(cfg); err != nil {
		return route, err
	}
	route.Mode, route.AccountID = cfg.mode(), accountID
	if strings.TrimSpace(platform) == "" {
		platform = defaultResinPlatform(cfg)
	}
	if route.Mode == auth.ResinModeReverse {
		if strings.HasPrefix(targetURL, "ws:") || strings.HasPrefix(targetURL, "wss:") {
			route.URL = buildWebSocketURL(cfg, targetURL, platform)
		} else {
			route.URL = buildReverseProxyURL(cfg, targetURL, platform)
		}
		return route, nil
	}
	endpoint, token, err := resinForwardEndpoint(cfg.BaseURL)
	if err != nil {
		return route, err
	}
	endpoint.User = url.UserPassword(platform+"."+accountID, token)
	route.ProxyURL = endpoint.String()
	return route, nil
}

// ResolveResinAuthRoute 让 OAuth、后台刷新和请求内刷新使用相同的渠道与平台快照。
func ResolveResinAuthRoute(ctx context.Context, targetURL, accountID string) (auth.ResinRoute, error) {
	cfg := ResinConfigFromContext(ctx).forChannel(auth.ResinChannelFromContext(ctx))
	return buildResinRoute(cfg, targetURL, ResinPlatformFromContext(ctx), accountID)
}
