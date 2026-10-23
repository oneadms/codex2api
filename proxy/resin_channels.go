package proxy

import (
	"context"
	"strings"

	"github.com/codex2api/auth"
)

func normalizeResinConfig(cfg *ResinConfig) *ResinConfig {
	if cfg == nil || strings.TrimSpace(cfg.BaseURL) == "" {
		return nil
	}
	snapshot := &ResinConfig{
		Mode:               cfg.mode(),
		BaseURL:            strings.TrimSpace(cfg.BaseURL),
		PlatformName:       strings.Join(parseResinPlatforms(cfg.PlatformName), ","),
		CodexPlatformName:  strings.Join(parseResinPlatforms(cfg.CodexPlatformName), ","),
		TraeCNPlatformName: strings.Join(parseResinPlatforms(cfg.TraeCNPlatformName), ","),
	}
	if snapshot.PlatformName == "" && snapshot.CodexPlatformName == "" && snapshot.TraeCNPlatformName == "" {
		return nil
	}
	return snapshot
}

// forChannel 返回链路实际使用的平台，调用方只需处理选定后的配置。
func (cfg *ResinConfig) forChannel(channel string) *ResinConfig {
	if cfg == nil || strings.TrimSpace(cfg.BaseURL) == "" {
		return nil
	}
	platforms := cfg.CodexPlatformName
	if channel == auth.UpstreamTraeCN {
		platforms = cfg.TraeCNPlatformName
	}
	if len(parseResinPlatforms(platforms)) == 0 {
		platforms = cfg.PlatformName
	}
	if len(parseResinPlatforms(platforms)) == 0 {
		return nil
	}
	return &ResinConfig{Mode: cfg.mode(), BaseURL: cfg.BaseURL, PlatformName: platforms}
}

type resinPlatformSelection struct {
	codex  string
	traecn string
}

func withResinPlatforms(ctx context.Context, platforms resinPlatformSelection) context.Context {
	return context.WithValue(ctx, resinPlatformContextKey{}, platforms)
}

// 入口尚未选定账号时同时固定两条链路的平台，换账号重试仍沿用各自的会话出口。
func resinPlatformsForSessionIdentity(ctx context.Context, identity requestSessionIdentity, apiKeyID int64) resinPlatformSelection {
	sessionKey := ""
	if identity.hasStableAffinity {
		sessionKey = sessionAffinityKey(identity.affinityID, apiKeyID)
	}
	cfg := ResinConfigFromContext(ctx)
	return resinPlatformSelection{
		codex:  resinPlatformForSession(cfg.forChannel(""), sessionKey),
		traecn: resinPlatformForSession(cfg.forChannel(auth.UpstreamTraeCN), sessionKey),
	}
}

func withTraeCNResinContext(ctx context.Context) context.Context {
	ctx = auth.WithResinChannel(ctx, auth.UpstreamTraeCN)
	return WithResinConfig(ctx, ResinConfigFromContext(ctx))
}
