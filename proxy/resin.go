package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
)

// ==================== Resin 粘性代理池集成 ====================

// ResinConfig 保存 Resin 代理池连接配置
type ResinConfig struct {
	Mode               string // reverse 或 forward；空值沿用反向代理
	BaseURL            string // 完整基础地址，例如 http://127.0.0.1:2260/my-token
	PlatformName       string // 全局平台标识；支持逗号分隔，例如 codex2api 或 p1,p2,p3
	CodexPlatformName  string // Codex 独立平台列表，留空沿用公共平台
	TraeCNPlatformName string // TRAE CN 独立平台列表，留空沿用公共平台
}

// resinPlatformContextKey carries the platform selected for one request.  It
// is deliberately request-scoped: the global config may be hot-updated while
// an in-flight request (or retry) must continue using the same egress.
type resinPlatformContextKey struct{}

// WithResinPlatform pins a selected Resin platform to ctx.  A nil context is
// normalized to context.Background so callers can use it in small helpers and
// tests without special casing.
func WithResinPlatform(ctx context.Context, platform string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	platform = strings.TrimSpace(platform)
	return withResinPlatforms(ctx, resinPlatformSelection{codex: platform, traecn: platform})
}

// ResinPlatformFromContext returns the request-scoped platform, if one was
// pinned by the ingress handler.
func ResinPlatformFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	platforms, _ := ctx.Value(resinPlatformContextKey{}).(resinPlatformSelection)
	if auth.ResinChannelFromContext(ctx) == auth.UpstreamTraeCN {
		return platforms.traecn
	}
	return platforms.codex
}

// parseResinPlatforms normalizes the legacy single string field into a global
// ordered platform set.  Commas are the documented separator; newlines and
// semicolons are accepted as a convenience for environment/config files.
// Empty entries are ignored and duplicates are removed while preserving the
// first occurrence, so a malformed list cannot make one platform count twice.
func parseResinPlatforms(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		switch r {
		case ',', ';', '\n', '\r':
			return true
		default:
			return false
		}
	})
	if len(parts) == 0 {
		return nil
	}
	platforms := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		platform := strings.TrimSpace(part)
		if platform == "" {
			continue
		}
		if _, exists := seen[platform]; exists {
			continue
		}
		seen[platform] = struct{}{}
		platforms = append(platforms, platform)
	}
	return platforms
}

func (cfg *ResinConfig) platformList() []string {
	if cfg == nil {
		return nil
	}
	return parseResinPlatforms(cfg.PlatformName)
}

// 全局 Resin 配置（原子指针，支持热更新）
var resinCfg atomic.Pointer[ResinConfig]

// SetResinConfig 设置全局 Resin 配置；地址或全部平台列表为空时禁用 Resin。
// 每条链路优先使用独立平台，未填写时沿用公共平台。
// 启用/禁用同步到 auth 包的出口标记；日志隐藏 Resin URL 中的 token。
func SetResinConfig(cfg *ResinConfig) {
	wasEnabled := GetResinConfig() != nil
	normalized := normalizeResinConfig(cfg)
	resinCfg.Store(normalized)
	auth.SetResinEgressEnabled(normalized.forChannel("") != nil)
	if normalized != nil {
		log.Printf("[Resin] 已启用: mode=%s codex=%s traecn=%s endpoint=%s",
			normalized.mode(),
			strings.Join(normalized.forChannel("").platformList(), ","),
			strings.Join(normalized.forChannel(auth.UpstreamTraeCN).platformList(), ","),
			MaskResinBaseURL(normalized.BaseURL))
	} else if wasEnabled {
		log.Printf("[Resin] 已禁用;Codex 渠道出站恢复按 账号 > 分组 > 代理池 > 全局 > 直连 解析")
	}
}

// GetResinConfig 获取当前 Resin 配置，未配置时返回 nil
func GetResinConfig() *ResinConfig {
	return resinCfg.Load()
}

// IsResinEnabled 检查 Codex 链路是否已启用 Resin。
func IsResinEnabled() bool {
	return GetResinConfig().forChannel("") != nil
}

// ResinPlatformForSession returns the deterministic global platform assigned
// to sessionKey.  With one platform it is exactly the legacy behavior.  An
// empty key intentionally falls back to the first configured platform so
// maintenance/anonymous requests do not hash a per-request random identity.
func ResinPlatformForSession(sessionKey string) string {
	return resinPlatformForSession(GetResinConfig().forChannel(""), sessionKey)
}

func resinPlatformForSession(cfg *ResinConfig, sessionKey string) string {
	if cfg == nil {
		return ""
	}
	platforms := cfg.platformList()
	if len(platforms) == 0 {
		return ""
	}
	if len(platforms) == 1 || strings.TrimSpace(sessionKey) == "" {
		return platforms[0]
	}

	// SHA-256 is stable across processes/platforms and gives a sufficiently
	// even distribution for the small platform sets this setting targets.
	// Prefix the input to keep this routing hash domain-separated from other
	// session derivations in the gateway.
	sum := sha256.Sum256([]byte("codex2api:resin-platform:" + strings.TrimSpace(sessionKey)))
	index := binary.BigEndian.Uint64(sum[:8]) % uint64(len(platforms))
	return platforms[index]
}

// ResinPlatformForCredential maps a downstream credential to the same
// deterministic seed used by ResolveSessionID when no explicit session key is
// supplied.  It is useful to embedded HTTP/WS executors that receive the
// credential as an argument rather than in the ingress header map.
func ResinPlatformForCredential(credential string) string {
	credential = strings.TrimSpace(credential)
	if credential == "" {
		return ResinPlatformForSession("")
	}
	return ResinPlatformForSession(DeriveStableSessionUUIDv7("codex2api:prompt-cache:" + credential))
}

func defaultResinPlatform(cfg *ResinConfig) string {
	platforms := cfg.platformList()
	if len(platforms) == 0 {
		return ""
	}
	return platforms[0]
}

// ==================== 反向代理 URL 构建 ====================

// BuildReverseProxyURL 将目标 URL 转换为 Resin 反向代理 URL
// 例如: https://chatgpt.com/backend-api/codex/responses
//
//	→ http://127.0.0.1:2260/my-token/codex2api/https/chatgpt.com/backend-api/codex/responses
func BuildReverseProxyURL(targetURL string) string {
	return BuildReverseProxyURLForPlatform(targetURL, "")
}

// BuildReverseProxyURLForPlatform is the platform-pinned URL builder used by
// request paths.  An empty platform selects the first configured platform,
// preserving the old single-platform helper semantics.
func BuildReverseProxyURLForPlatform(targetURL, platformName string) string {
	return buildReverseProxyURL(GetResinConfig().forChannel(""), targetURL, platformName)
}

func buildReverseProxyURL(cfg *ResinConfig, targetURL, platformName string) string {
	if cfg == nil {
		return targetURL
	}
	parsed, err := url.Parse(targetURL)
	if err != nil {
		return targetURL
	}
	if platformName = strings.TrimSpace(platformName); platformName == "" {
		platformName = defaultResinPlatform(cfg)
	}
	if platformName == "" {
		return targetURL
	}
	// <resin_base>/<platform>/<protocol>/<host><path+query>
	base := strings.TrimRight(cfg.BaseURL, "/")
	return fmt.Sprintf("%s/%s/%s/%s%s",
		base,
		platformName,
		parsed.Scheme,
		parsed.Host,
		parsed.RequestURI(),
	)
}

// BuildWebSocketURL 将目标 WSS URL 转换为 Resin WS 反向代理 URL
// 例如: wss://chatgpt.com/backend-api/codex/responses
//
//	→ ws://127.0.0.1:2260/my-token/codex2api/https/chatgpt.com/backend-api/codex/responses
//
// Resin 约定: 客户端到 Resin 只支持 ws://；路径中 protocol 填 http/https 对应目标 ws/wss
func BuildWebSocketURL(targetURL string) string {
	return BuildWebSocketURLForPlatform(targetURL, "")
}

// BuildWebSocketURLForPlatform is the platform-pinned WebSocket URL builder.
func BuildWebSocketURLForPlatform(targetURL, platformName string) string {
	return buildWebSocketURL(GetResinConfig().forChannel(""), targetURL, platformName)
}

func buildWebSocketURL(cfg *ResinConfig, targetURL, platformName string) string {
	if cfg == nil {
		return targetURL
	}
	parsed, err := url.Parse(targetURL)
	if err != nil {
		return targetURL
	}
	if platformName = strings.TrimSpace(platformName); platformName == "" {
		platformName = defaultResinPlatform(cfg)
	}
	if platformName == "" {
		return targetURL
	}
	resinParsed, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return targetURL
	}

	// wss → https, ws → http（Resin 路径中的 protocol 字段）
	protocol := "https"
	if parsed.Scheme == "ws" || parsed.Scheme == "http" {
		protocol = "http"
	}

	// 保留 Token 路径的转义字符，并去掉末尾斜线，避免拼出错误的双斜线路径。
	return fmt.Sprintf("ws://%s%s/%s/%s/%s%s",
		resinParsed.Host,
		strings.TrimRight(resinParsed.EscapedPath(), "/"),
		platformName,
		protocol,
		parsed.Host,
		parsed.RequestURI(),
	)
}

// ==================== 账号标识 ====================

// ResinAccountID 返回账号在 Resin 中的稳定标识（DBID 转字符串）
func ResinAccountID(account *auth.Account) string {
	return fmt.Sprintf("%d", account.DBID)
}

// ==================== 租约继承 ====================

// InheritLease 将临时身份的 IP 租约继承给正式账号身份
// 用于 OAuth 场景：授权阶段使用临时标识，账号创建后切换为 DBID
func InheritLease(ctx context.Context, tempAccount, newAccount string) {
	cfg := ResinConfigFromContext(ctx).forChannel("")
	if cfg == nil {
		return
	}
	platform := ResinPlatformFromContext(ctx)
	if platform == "" {
		platform = defaultResinPlatform(cfg)
	}

	inheritURL := fmt.Sprintf("%s/api/v1/%s/actions/inherit-lease",
		strings.TrimRight(cfg.BaseURL, "/"),
		platform,
	)

	body := fmt.Sprintf(`{"parent_account":%q,"new_account":%q}`, tempAccount, newAccount)

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, inheritURL, bytes.NewBufferString(body))
	if err != nil {
		log.Printf("[Resin] 构建 inherit-lease 请求失败")
		return
	}
	req.Header.Set("Content-Type", "application/json")

	client := auth.NewResinHTTPClient(10 * time.Second)
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		// url.Error 含控制接口路径中的 Token，只记录底层错误。
		if urlErr, ok := err.(*url.Error); ok {
			err = urlErr.Err
		}
		log.Printf("[Resin] inherit-lease 请求失败: %v", err)
		return
	}
	resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("[Resin] inherit-lease 返回非成功状态: %d (temp=%s new=%s)", resp.StatusCode, tempAccount, newAccount)
	} else {
		log.Printf("[Resin] inherit-lease 成功: %s → %s", tempAccount, newAccount)
	}
}

// ==================== Resin 连接池 ====================

// getResinHTTPClient 返回走 Resin 反代的标准 HTTP 客户端（无 uTLS）
// 池键按 accountID 隔离，复用底层 TCP 连接
func getResinHTTPClient(account *auth.Account) *http.Client {
	key := fmt.Sprintf("resin|%d", account.ID())

	if v, ok := clientPool.Load(key); ok {
		entry := v.(*poolEntry)
		entry.touch()
		return entry.client
	}

	transport := &http.Transport{
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 5,
		MaxConnsPerHost:     10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}

	entry := &poolEntry{
		client: &http.Client{
			Transport: transport,
			Timeout:   0, // 流式响应不设超时
			// 上游绝对地址重定向不能绕过 Resin；交由调用方处理原始状态码。
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	entry.touch()

	if v, loaded := clientPool.LoadOrStore(key, entry); loaded {
		e := v.(*poolEntry)
		e.touch()
		return e.client
	}
	return entry.client
}
