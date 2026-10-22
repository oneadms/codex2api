package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

type gatewayRequestKey struct{}
type gatewayRequest struct {
	route          gatewayRoute
	revision       string
	effectiveProxy string
	proxyBound     bool // true with an empty proxy means explicitly direct
	fresh          bool // a managed selector must not reuse an HTTP transport
}

// CodexGatewayEgressFromContext binds the Cookie to the exit actually selected
// for this request. Never infer that exit from the source route when IP affinity
// is disabled. Complete tickets remain higher priority in the egress resolver.
func CodexGatewayEgressFromContext(ctx context.Context) (proxyURL string, bound, fresh bool) {
	if ctx == nil {
		return "", false, false
	}
	v, ok := ctx.Value(gatewayRequestKey{}).(gatewayRequest)
	if !ok {
		return "", false, false
	}
	return v.effectiveProxy, v.proxyBound, v.fresh
}

func replaceGatewayCookie(h http.Header, value string) {
	kept := []string{}
	for _, raw := range h.Values("Cookie") {
		for _, part := range strings.Split(raw, ";") {
			part = strings.TrimSpace(part)
			name, _, _ := strings.Cut(part, "=")
			if part != "" && strings.TrimSpace(name) != "__oailb" {
				kept = append(kept, part)
			}
		}
	}
	kept = append(kept, "__oailb="+value)
	h.Set("Cookie", strings.Join(kept, "; "))
}
func ApplyCodexGatewayCookie(ctx context.Context, h http.Header) {
	if codexTicketFromContext(ctx) != nil {
		return
	}
	if v, ok := ctx.Value(gatewayRequestKey{}).(gatewayRequest); ok && v.route.cookie != "" {
		replaceGatewayCookie(h, v.route.cookie)
	}
}
func gatewayHTTPClient(ctx context.Context, c *http.Client) *http.Client {
	if _, ok := ctx.Value(gatewayRequestKey{}).(gatewayRequest); !ok {
		return c
	}
	copy := *c
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copy
}
func prepareCodexGatewayHTTP(ctx context.Context, a *auth.Account, body []byte, proxy string) (context.Context, string, func(), error) {
	noop := func() {}
	m := codexHarvester.Load()
	if m == nil || a == nil || !a.SupportsCodexTickets() || gjson.GetBytes(body, "model").String() != gatewayModel || CodexTurnStateInjectionFromContext(ctx) != "" {
		return ctx, proxy, noop, nil
	}
	if bound, ok := ctx.Value(gatewayRequestKey{}).(gatewayRequest); ok && bound.proxyBound {
		return ctx, bound.effectiveProxy, noop, nil
	}
	v, err := m.GatewaySettings(ctx)
	if err != nil {
		return ctx, proxy, noop, err
	}
	if !v.CookiePool.Enabled || !slices.Contains(v.CookiePool.TargetAccountIDs, a.ID()) || gatewayUnified(v) {
		return ctx, proxy, noop, nil
	}
	g := m.gatewayManager()
	r, ok := g.pickRoute(v)
	if !ok {
		return ctx, proxy, noop, nil
	} // Ordinary traffic never prepares or probes.
	fallback := proxy
	if fallback == "" {
		a.Mu().RLock()
		fallback = a.ProxyURL
		a.Mu().RUnlock()
	}
	effective, release, err := g.pin(ctx, v, r, fallback)
	if err != nil {
		if release != nil {
			release()
		}
		// Borrowing is optional for business traffic. A stale route/selector
		// must not block the selected account's ordinary exit.
		if ctx.Err() != nil {
			return ctx, proxy, noop, ctx.Err()
		}
		return ctx, proxy, noop, nil
	}
	bound := gatewayRequest{route: r, revision: gatewayRouteRevision(v), effectiveProxy: effective, proxyBound: true, fresh: r.nodeID != ""}
	return context.WithValue(ctx, gatewayRequestKey{}, bound), effective, release, nil
}

// PrepareCodexGatewayWebsocket also covers non-fixed WS traffic. It runs before
// URL rewriting/dialer resolution and keeps the lease until the response closes.
func PrepareCodexGatewayWebsocket(ctx context.Context, a *auth.Account, body []byte, proxyURL string) (context.Context, string, func(), error) {
	return prepareCodexGatewayHTTP(ctx, a, body, proxyURL)
}

type gatewayLeaseBody struct {
	io.ReadCloser
	once   sync.Once
	finish func()
}

func (b *gatewayLeaseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.finish)
	return err
}
func retainCodexGatewayHTTP(ctx context.Context, response *http.Response, release func()) {
	if response == nil || response.Body == nil {
		release()
		return
	}
	if _, ok := ctx.Value(gatewayRequestKey{}).(gatewayRequest); !ok {
		release()
		return
	}
	var once sync.Once
	finish := func() { once.Do(release) }
	stop := context.AfterFunc(ctx, finish)
	response.Body = &gatewayLeaseBody{ReadCloser: response.Body, finish: func() { stop(); finish() }}
}

type gatewayAnchor struct {
	accountID                                      int64
	expires                                        time.Time
	responseID, connectionID, proxy, cookie, nonce string
	busy                                           bool
	egress                                         *gatewayRequest
	discard                                        func()
}

// GatewayWSTurn is an opt-in fixed-session turn, independent of ordinary WS
// pooling. It never contains source OAuth or source turn-state.
type GatewayWSTurn struct {
	g                        *codexGatewayManager
	key, revision, identity  string
	accountID                int64
	anchor                   *gatewayAnchor
	mu                       sync.Mutex
	done                     bool
	responseID, connectionID string
	success                  bool
	release                  func()
}

func (t *GatewayWSTurn) ProxyURL() string { return t.anchor.proxy }
func (t *GatewayWSTurn) PoolKey() string  { return "gateway:" + t.key + ":" + t.anchor.nonce }
func (t *GatewayWSTurn) Context(ctx context.Context) context.Context {
	if t.anchor.egress != nil {
		ctx = context.WithValue(ctx, gatewayRequestKey{}, *t.anchor.egress)
	}
	next, cancel := context.WithDeadline(ctx, t.anchor.expires)
	t.mu.Lock()
	old := t.release
	t.release = func() {
		cancel()
		if old != nil {
			old()
		}
	}
	t.mu.Unlock()
	return next
}
func (t *GatewayWSTurn) CanRetain() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.success && time.Now().Before(t.anchor.expires)
}
func (t *GatewayWSTurn) Deadline() time.Time { return t.anchor.expires }
func (t *GatewayWSTurn) Apply(h http.Header, contexts ...context.Context) {
	if len(contexts) > 0 && codexTicketFromContext(contexts[0]) != nil {
		return // Never mix a newly selected complete ticket with an old route Cookie.
	}
	if t.anchor.cookie != "" {
		replaceGatewayCookie(h, t.anchor.cookie)
	}
}
func (t *GatewayWSTurn) BindConnection(id string, discard ...func()) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.anchor.connectionID != "" && t.anchor.connectionID != id {
		return &CodexGatewayWSError{Reason: "原连接已失效"}
	}
	t.connectionID = id
	if len(discard) != 0 {
		t.anchor.discard = discard[0]
	}
	return nil
}
func (t *GatewayWSTurn) Observe(frame []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return
	}
	e := gjson.ParseBytes(frame)
	switch e.Get("type").String() {
	case "response.completed", "response.done":
		t.responseID = e.Get("response.id").String()
		t.success = t.responseID != "" && e.Get("response.model").String() == gatewayModel && e.Get("response.status").String() == "completed" && !e.Get("response.error").IsObject()
	case "error", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		t.success = false
	}
}
func (t *GatewayWSTurn) Finish(failed bool) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return t.success
	}
	t.done = true
	if t.release != nil {
		t.release()
	}
	g := t.g
	g.mu.Lock()
	defer g.mu.Unlock()
	current := g.anchors[t.key]
	if current != t.anchor {
		t.success = false
		return false
	}
	if failed || !t.success || t.connectionID == "" || !time.Now().Before(t.anchor.expires) || gatewayRouteRevision(g.settings) != t.revision || gatewayWSIdentity(g.owner.Store.FindByID(t.accountID)) != t.identity {
		delete(g.anchors, t.key)
		t.success = false
		return false
	}
	current.busy = false
	current.responseID = t.responseID
	current.connectionID = t.connectionID
	return true
}
func gatewayWSIdentity(a *auth.Account) string {
	if a == nil {
		return ""
	}
	groups := a.GroupIDSnapshot()
	slices.Sort(groups)
	b, _ := json.Marshal(groups)
	sum := sha256.Sum256([]byte(gatewayIdentity(a) + "\x00" + a.GetAccessToken() + "\x00" + string(b)))
	return hex.EncodeToString(sum[:])
}
func BeginCodexGatewayWSTurn(ctx context.Context, a *auth.Account, body []byte, headers http.Header, apiKey, proxy string) (*GatewayWSTurn, error) {
	m := codexHarvester.Load()
	if m == nil || a == nil || !a.SupportsCodexTickets() || gjson.GetBytes(body, "model").String() != gatewayModel {
		return nil, nil
	}
	// 防御纵深：合成探针即使绕过 CodexGatewayWSRequired 也必须在这里放行到
	// 普通路径。锚点键由 API Key + 客户端会话标识构成，合成探针两者皆无。
	if codexSyntheticProbeFromContext(ctx) {
		return nil, nil
	}
	v, err := m.GatewaySettings(ctx)
	if err != nil {
		return nil, err
	}
	if !v.WSSession.Enabled || !slices.Contains(v.WSSession.AccountIDs, a.ID()) {
		return nil, nil
	}
	scope, captured := ctx.Value(codexGatewayWSClientScopeKey{}).(string)
	if !captured {
		scope = codexGatewayWSClientScope(headers, body)
	}
	if scope == "" || apiKey == "" {
		return nil, &CodexGatewayWSError{Reason: "需要 API Key 和显式客户端会话标识"}
	}
	identity := gatewayWSIdentity(a)
	groupScope := ""
	if quota, ok := ctx.Value(apiKeyModelRequestContextKey{}).(*apiKeyModelRequestQuota); ok && quota.db != nil {
		row, e := quota.db.GetAPIKeyByID(ctx, quota.keyID)
		if e != nil {
			return nil, &CodexGatewayWSError{Reason: "无法复核分组"}
		}
		raw, _ := json.Marshal(row.AllowedGroupIDs)
		groupScope = string(raw)
	}
	revision := gatewayRouteRevision(v)
	keyHash := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%s\x00%s", apiKey, a.ID(), scope, identity, revision, groupScope)))
	key := hex.EncodeToString(keyHash[:])
	previous := gjson.GetBytes(body, "previous_response_id").String()
	g := m.gatewayManager()
	now := time.Now()
	g.mu.Lock()
	for k, old := range g.anchors {
		if !old.busy && !now.Before(old.expires) {
			delete(g.anchors, k)
		}
	}
	anchor := g.anchors[key]
	if anchor != nil && anchor.busy {
		g.mu.Unlock()
		return nil, &CodexGatewayWSError{Reason: "已有请求正在运行"}
	}
	if previous != "" && (anchor == nil || anchor.responseID != previous || !now.Before(anchor.expires)) {
		g.mu.Unlock()
		return nil, &CodexGatewayWSError{Reason: "不存在、已过期或响应不属于当前会话"}
	}
	var discardPrevious func()
	if previous == "" {
		if anchor == nil && len(g.anchors) >= 1024 {
			g.mu.Unlock()
			return nil, &CodexGatewayWSError{Reason: "数量已达上限"}
		}
		if anchor != nil {
			discardPrevious = anchor.discard
		}
		anchor = &gatewayAnchor{accountID: a.ID(), nonce: NewUpstreamSessionUUID(), expires: now.Add(time.Duration(v.WSSession.TTLSeconds) * time.Second), proxy: proxy}
		g.anchors[key] = anchor
	}
	anchor.busy = true
	g.mu.Unlock()
	if discardPrevious != nil {
		discardPrevious()
	}
	turn := &GatewayWSTurn{g: g, key: key, revision: revision, identity: identity, accountID: a.ID(), anchor: anchor, release: func() {}}
	if previous == "" {
		prepared, effective, release, err := prepareCodexGatewayHTTP(ctx, a, body, proxy)
		if err != nil {
			turn.Finish(true)
			return nil, err
		}
		turn.release = release
		g.mu.Lock()
		if g.anchors[key] != anchor || gatewayRouteRevision(g.settings) != revision {
			g.mu.Unlock()
			turn.Finish(true)
			return nil, &CodexGatewayWSError{Reason: "configuration_changed"}
		}
		anchor.proxy = effective
		if route, ok := prepared.Value(gatewayRequestKey{}).(gatewayRequest); ok {
			anchor.egress = &route
			anchor.cookie = route.route.cookie
		}
		g.mu.Unlock()
	}
	recordGatewayWSDiagnosticTurn(ctx, turn)
	return turn, nil
}
