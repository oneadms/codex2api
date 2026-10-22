package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	route    gatewayRoute
	revision string
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
	if v, ok := ctx.Value(gatewayRequestKey{}).(gatewayRequest); ok {
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
		return ctx, proxy, noop, err
	}
	return context.WithValue(ctx, gatewayRequestKey{}, gatewayRequest{route: r, revision: v.Revision}), effective, release, nil
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
	next, cancel := context.WithDeadline(ctx, t.anchor.expires)
	old := t.release
	t.release = func() { cancel(); old() }
	return next
}
func (t *GatewayWSTurn) CanRetain() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.success && time.Now().Before(t.anchor.expires)
}
func (t *GatewayWSTurn) Deadline() time.Time { return t.anchor.expires }
func (t *GatewayWSTurn) Apply(h http.Header) {
	if t.anchor.cookie != "" {
		replaceGatewayCookie(h, t.anchor.cookie)
	}
}
func (t *GatewayWSTurn) BindConnection(id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.anchor.connectionID != "" && t.anchor.connectionID != id {
		return errors.New("固定会话原连接已失效")
	}
	t.connectionID = id
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
		t.success = t.responseID != "" && e.Get("response.model").String() == gatewayModel && e.Get("response.status").String() == "completed" && !e.Get("response.error").Exists()
	case "error", "response.failed", "response.incomplete", "response.cancelled":
		t.success = false
	}
}
func (t *GatewayWSTurn) Finish(failed bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return
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
		return
	}
	if failed || !t.success || t.connectionID == "" || !time.Now().Before(t.anchor.expires) || g.settings.Revision != t.revision || gatewayWSIdentity(g.owner.Store.FindByID(t.accountID)) != t.identity {
		delete(g.anchors, t.key)
		return
	}
	current.busy = false
	current.responseID = t.responseID
	current.connectionID = t.connectionID
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
	v, err := m.GatewaySettings(ctx)
	if err != nil {
		return nil, err
	}
	if !v.WSSession.Enabled || !slices.Contains(v.WSSession.AccountIDs, a.ID()) {
		return nil, nil
	}
	scope := ""
	for _, name := range []string{"Session-Id", "Session_id", "Thread-Id", "Thread_id"} {
		if scope = strings.TrimSpace(headers.Get(name)); scope != "" {
			break
		}
	}
	if scope == "" {
		for _, path := range []string{"client_metadata.session_id", "client_metadata.thread_id", "prompt_cache_key"} {
			if scope = gjson.GetBytes(body, path).String(); scope != "" {
				break
			}
		}
	}
	if scope == "" || apiKey == "" {
		return nil, errors.New("固定 WebSocket 会话需要 API Key 和显式客户端会话标识")
	}
	identity := gatewayWSIdentity(a)
	groupScope := ""
	if quota, ok := ctx.Value(apiKeyModelRequestContextKey{}).(*apiKeyModelRequestQuota); ok && quota.db != nil {
		row, e := quota.db.GetAPIKeyByID(ctx, quota.keyID)
		if e != nil {
			return nil, errors.New("无法复核固定会话分组")
		}
		raw, _ := json.Marshal(row.AllowedGroupIDs)
		groupScope = string(raw)
	}
	keyHash := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%s\x00%s", apiKey, a.ID(), scope, identity, v.Revision, groupScope)))
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
		return nil, errors.New("固定 WebSocket 会话已有请求正在运行")
	}
	if previous != "" && (anchor == nil || anchor.responseID != previous || !now.Before(anchor.expires)) {
		g.mu.Unlock()
		return nil, errors.New("固定 WebSocket 会话不存在、已过期或响应不属于当前会话")
	}
	if previous == "" {
		if len(g.anchors) >= 1024 {
			g.mu.Unlock()
			return nil, errors.New("固定 WebSocket 会话数量已达上限")
		}
		anchor = &gatewayAnchor{accountID: a.ID(), nonce: NewUpstreamSessionUUID(), expires: now.Add(time.Duration(v.WSSession.TTLSeconds) * time.Second), proxy: proxy}
		g.anchors[key] = anchor
	}
	anchor.busy = true
	g.mu.Unlock()
	turn := &GatewayWSTurn{g: g, key: key, revision: v.Revision, identity: identity, accountID: a.ID(), anchor: anchor, release: func() {}}
	if previous == "" && codexTicketFromContext(ctx) == nil && CodexTurnStateInjectionFromContext(ctx) == "" && !gatewayUnified(v) {
		if route, ok := g.pickRoute(v); ok {
			effective, release, e := g.pin(ctx, v, route, proxy)
			if e != nil {
				turn.Finish(true)
				return nil, e
			}
			turn.release = release
			// pin 可能较慢；期间配置重置会使 anchor 失效，不能再写入新路由。
			g.mu.Lock()
			if g.anchors[key] != anchor || g.settings.Revision != v.Revision {
				g.mu.Unlock()
				turn.Finish(true)
				return nil, errors.New("configuration_changed")
			}
			anchor.proxy = effective
			anchor.cookie = route.cookie
			g.mu.Unlock()
		}
	}
	return turn, nil
}
