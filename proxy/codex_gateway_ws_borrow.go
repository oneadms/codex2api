package proxy

import (
	"context"
	"net/http"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/harvest"
)

// 从已验收的 WS 来源票提取路由，不在业务请求中触发采票，也不转移来源身份或 turn state。
func (m *CodexHarvestManager) prepareStoredGatewayWebsocket(ctx context.Context, target *auth.Account, v harvest.GatewaySettings, model, fallback string) (context.Context, string, func(), error) {
	noop := func() {}
	for _, id := range v.CookiePool.SourceAccountIDs {
		if id == target.ID() {
			continue
		}
		source := m.Store.FindByID(id)
		ticket := gatewayStoredDonor(source, model, time.Now(), "websocket")
		if ticket == nil || ticket.HarvestProxyURL == "" {
			continue
		}
		request := &http.Request{Header: http.Header{"Cookie": {ticket.Cookie}}}
		cookie, err := request.Cookie("__oailb")
		if err != nil {
			continue
		}
		_, cookieExpiry, valid := auth.CodexRouteCookie(ticket.Cookie, time.Now())
		if !valid {
			continue
		}
		captured := ticket.CookieCapturedAt
		if captured.IsZero() {
			captured = ticket.CapturedAt
		}
		if captured.IsZero() {
			shape, _ := auth.ParseCodexTicketShape(ticket.State)
			captured = shape.IssuedAt
		}
		expires := minGatewayTime(ticket.EffectiveExpiry(), cookieExpiry)
		expires = minGatewayTime(expires, captured.Add(time.Duration(v.CookiePool.TTLSeconds)*time.Second))
		if !time.Now().Before(expires) {
			continue
		}
		identity := gatewayIdentity(source)
		// 复用票据的节点租约实现，但绝不把这个含来源票据的临时上下文传给目标执行器。
		leaseCtx := context.WithValue(ctx, codexTicketInjectionKey{}, ticket)
		leased, release, err := acquireCodexTicketEgress(leaseCtx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx, fallback, noop, ctx.Err()
			}
			continue
		}
		current := gatewayStoredDonor(m.Store.FindByID(id), model, time.Now(), "websocket")
		if !m.GatewayReuseCurrent(ctx, gatewayRouteRevision(v), target.ID(), model) || gatewayIdentity(m.Store.FindByID(id)) != identity || current == nil || current.State != ticket.State || current.Cookie != ticket.Cookie || !time.Now().Before(expires) {
			release()
			continue
		}
		effective := codexTicketFromContext(leased).HarvestProxyURL
		route := gatewayRoute{sourceID: id, identity: identity, cookie: cookie.Value, proxy: effective,
			nodeID: ticket.HarvestNodeID, nodeName: ticket.HarvestNodeName, poolID: ticket.HarvestPoolID,
			managed: ticket.ManagedNode, gateway: gatewayCookieHost(cookie.Value), captured: captured, expires: expires}
		// 普通 WS 必须重发握手，不能复用冻结了另一份 Cookie 的旧连接；固定会话自行保留原连接。
		bound := gatewayRequest{route: route, revision: gatewayRouteRevision(v), accountID: target.ID(), model: model, websocket: true, effectiveProxy: effective, proxyBound: true, fresh: true}
		return context.WithValue(ctx, gatewayRequestKey{}, bound), effective, release, nil
	}
	return ctx, fallback, noop, nil
}

func codexGatewayWebsocketBorrowReady(ctx context.Context, account *auth.Account, model string) bool {
	if ctx == nil {
		return false
	}
	model = auth.NormalizeCodexTicketModel(model)
	bound, ok := ctx.Value(gatewayRequestKey{}).(gatewayRequest)
	if !ok || !bound.websocket || !bound.proxyBound || bound.accountID != account.ID() || bound.model != model || bound.route.cookie == "" {
		return false
	}
	if turn, _ := ctx.Value(gatewayWSTurnKey{}).(*GatewayWSTurn); turn != nil && turn.accountID == account.ID() && turn.model == model {
		// 已建立的固定会话只续用原连接，路由 Cookie 到期不会触发重新握手。
		turn.mu.Lock()
		connected := turn.anchor.connectionID != "" && !turn.done && time.Now().Before(turn.anchor.expires)
		turn.mu.Unlock()
		if connected {
			return true
		}
	}
	return time.Now().Before(bound.route.expires)
}
