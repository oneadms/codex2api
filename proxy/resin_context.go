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
	return account != nil && (!account.IsRelayStyle() || (account.IsTraeCNAPI() && account.ID() > 0))
}

// ValidateResinConfig 校验保存的配置，防止只填地址或平台时被静默视为直连。
func ValidateResinConfig(cfg *ResinConfig) error {
	if cfg.mode() != auth.ResinModeReverse && cfg.mode() != auth.ResinModeForward {
		return fmt.Errorf("Resin 模式必须是 reverse 或 forward")
	}
	if cfg == nil || (strings.TrimSpace(cfg.BaseURL) == "" && strings.TrimSpace(cfg.PlatformName) == "" && strings.TrimSpace(cfg.CodexPlatformName) == "" && strings.TrimSpace(cfg.TraeCNPlatformName) == "") {
		return nil
	}
	if normalizeResinConfig(cfg) == nil {
		return fmt.Errorf("Resin 地址和至少一个平台标识必须同时填写，禁用时请清空地址及全部平台标识")
	}
	parsed, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("Resin 地址必须是有效的 HTTP 或 HTTPS 地址")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("Resin 地址不能包含查询参数或片段，Token 应放在地址路径中")
	}
	if cfg.mode() == auth.ResinModeForward {
		if _, _, err := resinForwardEndpoint(cfg.BaseURL); err != nil {
			return err
		}
		for _, list := range []string{cfg.PlatformName, cfg.CodexPlatformName, cfg.TraeCNPlatformName} {
			for _, platform := range parseResinPlatforms(list) {
				if strings.ContainsAny(platform, ".:|/\\@?#%~ \t") {
					return fmt.Errorf("Resin 正向代理平台名称不能包含分隔符、空格或特殊字符")
				}
			}
		}
	}
	return nil
}

// WithResinConfig 固定本次请求的配置；nil 明确表示禁用，不受并发热更新影响。
func WithResinConfig(ctx context.Context, cfg *ResinConfig) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	snapshot := normalizeResinConfig(cfg)
	ctx = context.WithValue(ctx, resinConfigContextKey{}, snapshot)
	if snapshot == nil {
		return auth.WithResinRequestResolver(ctx, nil)
	}
	return auth.WithResinRequestResolver(ctx, func(requestCtx context.Context, targetURL, accountID string) (auth.ResinRoute, error) {
		return buildResinRoute(snapshot.forChannel(auth.ResinChannelFromContext(requestCtx)), targetURL, ResinPlatformFromContext(requestCtx), accountID)
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
	return ResinConfigFromContext(ctx).forChannel(auth.ResinChannelFromContext(ctx)) != nil
}

func ResinPlatformForSessionFromContext(ctx context.Context, sessionKey string) string {
	return resinPlatformForSession(ResinConfigFromContext(ctx).forChannel(auth.ResinChannelFromContext(ctx)), sessionKey)
}

func BuildReverseProxyURLForContext(ctx context.Context, targetURL, platformName string) string {
	return buildReverseProxyURL(ResinConfigFromContext(ctx).forChannel(auth.ResinChannelFromContext(ctx)), targetURL, platformName)
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
