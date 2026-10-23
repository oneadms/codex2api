package proxy

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/codex2api/auth"
)

// ==================== Codex 出口链路统一解析 ====================
//
// Codex 渠道的出站有三套配置并存(issue #679):
//
//  1. Resin 正向或反向代理（系统设置 resin_mode、resin_url 与平台列表）
//  2. 代理池 / 分组代理 / 账号 proxy_url / 全局 proxy_url
//     (由 auth.Store.ResolveProxyForAccount 按 账号 > 分组 > 代理池 > 全局 选出一条)
//  3. 直连
//
// 普通请求的优先级为 Resin > 代理 > 直连,且 Resin 是整层覆盖:一旦启用,Codex 渠道
// 所有携带账号身份的出站(/responses、compact、WS、wham 用量、订阅查询、遥测、
// 令牌刷新)全部改经 Resin,第 2 层选出的代理只保留在日志/审计里、不参与拨号。
// 显式模板刷新可通过 ResolveCodexRequestEgress 使用专属签发代理,不改变以上默认链路。
// Claude / Grok / Antigravity 等中继型账号不经 Resin,继续走第 2 层。
// 已注入门票的业务请求优先使用该票保存的采票代理，保持票据所需的出口绑定。
//
// 所有 Codex 出站选客户端的地方都必须经过本文件的解析器,禁止各自再写
// `if IsResinEnabled()` 分支——分支散落正是"三套互相覆盖、看不出谁在生效"的根源。

// CodexEgressKind 标识一次 Codex 出站最终走的是哪条链路。
type CodexEgressKind string

const (
	// CodexEgressResin 经 Resin，具体转发方式由 ResinMode 区分。
	CodexEgressResin CodexEgressKind = "resin"
	// CodexEgressProxy 经第 2 层选出的代理直达上游。
	CodexEgressProxy CodexEgressKind = "proxy"
	// CodexEgressDirect 无代理直连上游。
	CodexEgressDirect CodexEgressKind = "direct"
)

// CodexEgress 描述解析后的出口:最终请求地址、拨号用代理与客户端。
type CodexEgress struct {
	Kind CodexEgressKind
	// URL 是最终请求地址；仅 Resin 反向模式改写为反代路径。
	URL string
	// ProxyURL 是第 2 层选出的代理。Resin 模式下保留原值用于审计,但拨号不使用。
	ProxyURL string
	// DialProxyURL 是实际参与拨号的代理；Resin 反向模式为空。
	DialProxyURL string
	ResinMode    string
	Err          error

	account         *auth.Account
	client          *http.Client
	resinRoute      auth.ResinRoute
	freshTicketNode bool
}

// ResolveCodexEgress 为一次携带账号身份的 Codex HTTP 出站决定链路。
// proxyURL 是第 2 层已经解析好的代理(可为空);Resin 启用时被整层覆盖。
// account 为 nil 时不可能走 Resin(Resin 按账号粘性,无身份无从粘),退回代理/直连。
func ResolveCodexEgress(account *auth.Account, targetURL, proxyURL string) CodexEgress {
	return ResolveCodexEgressForContext(context.Background(), account, targetURL, proxyURL)
}

// ResolveCodexEgressForContext 沿用请求固定的 Resin 配置与平台，保证热更新时
// URL、客户端、身份头与审计仍对应同一次出口决策。
func ResolveCodexEgressForContext(ctx context.Context, account *auth.Account, targetURL, proxyURL string) CodexEgress {
	if pinned := CodexTicketProxyForRequest(ctx, ""); pinned != "" {
		return CodexEgress{Kind: CodexEgressProxy, URL: targetURL, ProxyURL: pinned, DialProxyURL: pinned, account: account, freshTicketNode: codexTicketFromContext(ctx).HarvestNodeID != ""}
	}
	if codexTicketFromContext(ctx) == nil {
		if pinned, bound, fresh := CodexGatewayEgressFromContext(ctx); bound {
			kind := CodexEgressProxy
			if pinned == "" {
				kind = CodexEgressDirect
			}
			return CodexEgress{Kind: kind, URL: targetURL, ProxyURL: pinned, DialProxyURL: pinned, account: account, freshTicketNode: fresh}
		}
	}
	proxyURL = strings.TrimSpace(proxyURL)
	if account != nil && account.IsTraeCNAPI() {
		ctx = auth.WithResinChannel(ctx, auth.UpstreamTraeCN)
	}
	cfg := ResinConfigFromContext(ctx).forChannel(auth.ResinChannelFromContext(ctx))
	if cfg != nil && AccountSupportsResin(account) {
		return resolveResinEgress(ctx, cfg, account, targetURL, proxyURL)
	}
	kind := CodexEgressDirect
	if proxyURL != "" {
		kind = CodexEgressProxy
	}
	return CodexEgress{
		Kind:         kind,
		URL:          targetURL,
		ProxyURL:     proxyURL,
		DialProxyURL: proxyURL,
		account:      account,
	}
}

func resolveResinEgress(ctx context.Context, cfg *ResinConfig, account *auth.Account, targetURL, proxyURL string) CodexEgress {
	route, err := buildResinRoute(cfg, targetURL, ResinPlatformFromContext(ctx), ResinAccountID(account))
	egress := CodexEgress{Kind: CodexEgressResin, URL: route.URL, ProxyURL: proxyURL,
		DialProxyURL: route.ProxyURL, ResinMode: cfg.mode(), Err: err, account: account, resinRoute: route}
	if err == nil && route.Mode == auth.ResinModeReverse {
		egress.client = getResinHTTPClient(account)
	}
	return egress
}

// resinCarriesEgressForContext 与出口解析使用相同的账号范围与请求配置快照。
// TRAE CN 保留二开的 Resin 支持，其余中继型账号仍按代理链解析。
func resinCarriesEgressForContext(ctx context.Context, account *auth.Account) bool {
	if account != nil && account.IsTraeCNAPI() {
		ctx = auth.WithResinChannel(ctx, auth.UpstreamTraeCN)
	}
	if _, bound, _ := CodexGatewayEgressFromContext(ctx); bound && codexTicketFromContext(ctx) == nil {
		return false
	}
	return CodexTicketProxyForRequest(ctx, "") == "" && IsResinEnabledForContext(ctx) && AccountSupportsResin(account)
}

// ViaResin 报告本次出站是否经 Resin。
func (e CodexEgress) ViaResin() bool {
	return e.Kind == CodexEgressResin
}

// Client 返回本链路的 HTTP 客户端。反向模式复用 Resin 连接池；正向模式沿用
// 网关的 TLS 策略，并以含平台和身份的实际代理地址隔离连接。account 为 nil 时返回 nil，
// 由调用方按自己的兜底 transport 处理。
func (e CodexEgress) Client() *http.Client {
	if e.Err != nil {
		return &http.Client{Transport: codexEgressErrorTransport{err: e.Err}}
	}
	if e.ResinMode == auth.ResinModeReverse && e.client != nil {
		return e.client
	}
	if e.freshTicketNode {
		// Selector 切换后的请求必须重新 CONNECT，不能复用指向其他节点的 TCP 连接。
		client, err := auth.BuildHTTPClientChecked(e.DialProxyURL)
		if err != nil {
			return &http.Client{Transport: codexEgressErrorTransport{err: err}}
		}
		if transport, ok := client.Transport.(*http.Transport); ok {
			transport.DisableKeepAlives = true
		}
		return client
	}
	if e.client == nil {
		if e.account == nil {
			return nil
		}
		e.client = getPooledClient(e.account, e.DialProxyURL)
	}
	if e.ViaResin() {
		client := *e.client
		client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
		return &client
	}
	return e.client
}

type codexEgressErrorTransport struct{ err error }

func (t codexEgressErrorTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, t.err
}

// ApplyHeaders 只在反代注入账号头；正代身份由代理认证携带。
func (e CodexEgress) ApplyHeaders(h http.Header) {
	if h == nil || !e.ViaResin() || e.account == nil {
		return
	}
	e.resinRoute.ApplyHeaders(h)
}

// ResolveCodexWebsocketEgress 为 Codex WS 出站决定链路:返回最终 WS 地址与
// 拨号用代理。反向模式改写地址，正向模式保留目标并通过 CONNECT 建立隧道；
// 调用方用 ApplyHeaders 应用对应的身份头策略。
func ResolveCodexWebsocketEgress(account *auth.Account, wsURL, proxyURL string) CodexEgress {
	return ResolveCodexWebsocketEgressForContext(context.Background(), account, wsURL, proxyURL)
}

// ResolveCodexWebsocketEgressForContext 为 WS URL 改写沿用请求固定的配置与平台。
func ResolveCodexWebsocketEgressForContext(ctx context.Context, account *auth.Account, wsURL, proxyURL string) CodexEgress {
	if pinned := CodexTicketProxyForRequest(ctx, ""); pinned != "" {
		return CodexEgress{Kind: CodexEgressProxy, URL: wsURL, ProxyURL: pinned, DialProxyURL: pinned, account: account}
	}
	if codexTicketFromContext(ctx) == nil {
		if pinned, bound, _ := CodexGatewayEgressFromContext(ctx); bound {
			kind := CodexEgressProxy
			if pinned == "" {
				kind = CodexEgressDirect
			}
			return CodexEgress{Kind: kind, URL: wsURL, ProxyURL: pinned, DialProxyURL: pinned, account: account}
		}
	}
	proxyURL = strings.TrimSpace(proxyURL)
	if account != nil && account.IsTraeCNAPI() {
		ctx = auth.WithResinChannel(ctx, auth.UpstreamTraeCN)
	}
	cfg := ResinConfigFromContext(ctx).forChannel(auth.ResinChannelFromContext(ctx))
	if cfg != nil && AccountSupportsResin(account) {
		return resolveResinEgress(ctx, cfg, account, wsURL, proxyURL)
	}
	kind := CodexEgressDirect
	if proxyURL != "" {
		kind = CodexEgressProxy
	}
	return CodexEgress{
		Kind:         kind,
		URL:          wsURL,
		ProxyURL:     proxyURL,
		DialProxyURL: proxyURL,
		account:      account,
	}
}

// CodexDialProxyURL 返回实际代理：反向模式为空，正向模式包含 Resin 代理身份。
// 供已解析目标 URL 的 WS 拨号方使用，避免重复改写地址。
func CodexDialProxyURL(account *auth.Account, proxyURL string) (string, error) {
	return CodexDialProxyURLForContext(context.Background(), account, proxyURL)
}

// CodexDialProxyURLForContext 与请求的 URL 改写使用同一份 Resin 配置快照。
func CodexDialProxyURLForContext(ctx context.Context, account *auth.Account, proxyURL string) (string, error) {
	egress := ResolveCodexWebsocketEgressForContext(ctx, account, "", proxyURL)
	return egress.DialProxyURL, egress.Err
}

// CodexEgressSummary 是给设置接口/日志用的全局出口摘要:只回答"Codex 渠道现在
// 由谁承担出站"。逐账号的代理选择仍由 auth.Store 决定,这里不展开。
type CodexEgressSummary struct {
	// Mode 是 resin 或 proxy_chain;proxy_chain 表示按 账号 > 分组 > 代理池 > 全局 > 直连 解析。
	Mode string `json:"mode"`
	// ResinEnabled 为 true 时 Codex 渠道的代理池/proxy_url 均不参与出站。
	ResinEnabled bool   `json:"resin_enabled"`
	ResinMode    string `json:"resin_mode,omitempty"`
	// ResinEndpoint 是打码后的 Resin 地址(去掉路径里的 token),仅用于展示。
	ResinEndpoint string `json:"resin_endpoint,omitempty"`
	// ResinPlatformName 是 Resin 侧平台标识。
	ResinPlatformName string `json:"resin_platform_name,omitempty"`
}

// CurrentCodexEgressSummary 返回当前生效的 Codex 出口摘要。
func CurrentCodexEgressSummary() CodexEgressSummary {
	return currentResinEgressSummary("")
}

func CurrentTraeCNEgressSummary() CodexEgressSummary {
	return currentResinEgressSummary(auth.UpstreamTraeCN)
}

func currentResinEgressSummary(channel string) CodexEgressSummary {
	cfg := GetResinConfig().forChannel(channel)
	if cfg == nil {
		return CodexEgressSummary{Mode: "proxy_chain"}
	}
	return CodexEgressSummary{
		Mode:              "resin",
		ResinEnabled:      true,
		ResinMode:         cfg.mode(),
		ResinEndpoint:     MaskResinBaseURL(cfg.BaseURL),
		ResinPlatformName: cfg.PlatformName,
	}
}

// MaskResinBaseURL 把 Resin 基础地址里的 token(路径段)打码,只保留 scheme 与
// host:port,供日志与界面展示。resin_url 形如 http://host:port/<token>,整段
// 打印等于把凭据写进日志(issue #679)。
func MaskResinBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "***"
	}
	masked := parsed.Scheme + "://" + parsed.Host
	if strings.Trim(parsed.Path, "/") != "" {
		masked += "/***"
	}
	return masked
}

// ResolveCodexRequestEgress picks the websocket or HTTP egress for one request.
func ResolveCodexRequestEgress(ctx context.Context, account *auth.Account, targetURL, proxyURL string, websocket bool) CodexEgress {
	if websocket {
		return ResolveCodexWebsocketEgressForContext(ctx, account, targetURL, proxyURL)
	}
	return ResolveCodexEgressForContext(ctx, account, targetURL, proxyURL)
}
