package proxy

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/codex2api/auth"
)

type resinConfigContextKey struct{}
type freshCodexConnectionContextKey struct{}

// AccountSupportsResin 覆盖 Codex 授权账号和 TRAE CN。
func AccountSupportsResin(account *auth.Account) bool {
	return account != nil && (!account.IsRelayStyle() || account.IsTraeCNAPI())
}

// ValidateResinConfig 校验保存的配置，防止只填地址或平台时被静默视为直连。
func ValidateResinConfig(cfg *ResinConfig) error {
	if cfg == nil || (strings.TrimSpace(cfg.BaseURL) == "" && strings.TrimSpace(cfg.PlatformName) == "") {
		return nil
	}
	if strings.TrimSpace(cfg.BaseURL) == "" || len(cfg.platformList()) == 0 {
		return fmt.Errorf("Resin 地址和平台标识必须同时填写，禁用时请同时清空")
	}
	parsed, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("Resin 地址必须是有效的 HTTP 或 HTTPS 地址")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("Resin 地址不能包含查询参数或片段，Token 应放在地址路径中")
	}
	return nil
}

// WithResinConfig 固定本次请求的配置；nil 明确表示禁用，不受并发热更新影响。
func WithResinConfig(ctx context.Context, cfg *ResinConfig) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	var snapshot *ResinConfig
	if cfg != nil && strings.TrimSpace(cfg.BaseURL) != "" && len(cfg.platformList()) > 0 {
		snapshot = &ResinConfig{BaseURL: strings.TrimSpace(cfg.BaseURL), PlatformName: strings.Join(cfg.platformList(), ",")}
	}
	ctx = context.WithValue(ctx, resinConfigContextKey{}, snapshot)
	if snapshot == nil {
		return auth.WithResinRequestDecorator(ctx, nil)
	}
	return auth.WithResinRequestDecorator(ctx, func(requestCtx context.Context, targetURL, _ string) string {
		return buildReverseProxyURL(snapshot, targetURL, ResinPlatformFromContext(requestCtx))
	})
}

// ResinConfigFromContext 优先读取请求快照，普通请求仍兼容全局配置。
func ResinConfigFromContext(ctx context.Context) *ResinConfig {
	if ctx != nil {
		if cfg, ok := ctx.Value(resinConfigContextKey{}).(*ResinConfig); ok {
			return cfg
		}
	}
	return GetResinConfig()
}

func IsResinEnabledForContext(ctx context.Context) bool {
	return ResinConfigFromContext(ctx) != nil
}

func ResinPlatformForSessionFromContext(ctx context.Context, sessionKey string) string {
	return resinPlatformForSession(ResinConfigFromContext(ctx), sessionKey)
}

func BuildReverseProxyURLForContext(ctx context.Context, targetURL, platformName string) string {
	return buildReverseProxyURL(ResinConfigFromContext(ctx), targetURL, platformName)
}

func BuildWebSocketURLForContext(ctx context.Context, targetURL, platformName string) string {
	return buildWebSocketURL(ResinConfigFromContext(ctx), targetURL, platformName)
}

// WithFreshCodexConnection 让手工测连重新握手，避免复用连接掩盖当前出口状态。
func WithFreshCodexConnection(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, freshCodexConnectionContextKey{}, true)
}

func FreshCodexConnection(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	fresh, _ := ctx.Value(freshCodexConnectionContextKey{}).(bool)
	return fresh
}
