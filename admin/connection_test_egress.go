package admin

import (
	"context"
	"fmt"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
)

// prepareCodexConnectionTestContext 按已保存配置固定测连出口，避免处理请求的
// 实例尚未同步内存配置时直连。配置读取或校验失败必须中止，不能猜测出口。
func (h *Handler) prepareCodexConnectionTestContext(ctx context.Context, account *auth.Account) (context.Context, error) {
	if !proxy.AccountSupportsResin(account) {
		return ctx, nil
	}
	cfg := proxy.GetResinConfig()
	if h != nil && h.db != nil {
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		settings, err := h.db.GetSystemSettings(readCtx)
		cancel()
		if err != nil {
			return ctx, fmt.Errorf("读取 Resin 配置失败，已取消测试连接: %w", err)
		}
		if settings == nil {
			return ctx, fmt.Errorf("无法确认 Resin 配置，已取消测试连接")
		}
		cfg = &proxy.ResinConfig{BaseURL: settings.ResinURL, PlatformName: settings.ResinPlatformName}
	}
	if err := proxy.ValidateResinConfig(cfg); err != nil {
		return ctx, fmt.Errorf("Resin 配置无效，已取消测试连接: %w", err)
	}
	ctx = proxy.WithResinConfig(ctx, cfg)
	// 后台测连没有用户会话，与用量维护一样选择默认平台。
	ctx = proxy.WithResinPlatform(ctx, proxy.ResinPlatformForSessionFromContext(ctx, ""))
	return proxy.WithFreshCodexConnection(ctx), nil
}
