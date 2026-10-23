package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/harvest"
	"github.com/codex2api/internal/mihomo"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var gatewayHostPattern = regexp.MustCompile(`^chat\.gateway\.unified-[0-9]+\.api\.openai\.com$`)

func gatewayCookieHost(value string) string {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Host string `json:"host"`
	}
	if json.Unmarshal(raw, &claims) != nil || !gatewayHostPattern.MatchString(claims.Host) {
		return ""
	}
	return claims.Host
}
func gatewayResponseCookie(resp *http.Response, endpoint *url.URL, now time.Time, ttl int) (string, time.Time, error) {
	value := ""
	expires := time.Time{}
	for _, cookie := range resp.Cookies() {
		if cookie.Name != "__oailb" {
			continue
		}
		if !cookie.Secure || (cookie.Domain != "" && strings.TrimPrefix(cookie.Domain, ".") != endpoint.Hostname()) {
			continue
		}
		path := cookie.Path
		if path == "" {
			path = endpoint.Path[:strings.LastIndex(endpoint.Path, "/")]
		}
		if path == "" || path[0] != '/' || !(endpoint.Path == path || strings.HasPrefix(endpoint.Path, path) && (strings.HasSuffix(path, "/") || strings.HasPrefix(strings.TrimPrefix(endpoint.Path, path), "/"))) {
			continue
		}
		if cookie.MaxAge < 0 || cookie.Value == "" || cookie.MaxAge == 0 && !cookie.Expires.IsZero() && !now.Before(cookie.Expires) {
			return "", time.Time{}, errors.New("routing_cookie_missing")
		}
		if len(cookie.Value) > 4096 || strings.ContainsAny(cookie.Value, ";\r\n\t ,") {
			return "", time.Time{}, errors.New("routing_cookie_missing")
		}
		if cookie.MaxAge == 0 && cookie.Expires.IsZero() {
			continue
		}
		expiry := now.Add(time.Duration(ttl) * time.Second)
		if cookie.MaxAge > 0 {
			expiry = minGatewayTime(expiry, now.Add(time.Duration(cookie.MaxAge)*time.Second))
		} else {
			expiry = minGatewayTime(expiry, cookie.Expires)
		}
		value, expires = cookie.Value, expiry
	}
	if value == "" {
		return "", time.Time{}, errors.New("routing_cookie_missing")
	}
	return value, expires, nil
}
func minGatewayTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// gatewayRouteChanged 判断探针响应是否改写了借来的路由：值变化、被删除或已过期都算变化。
// 上游完全不回 __oailb 时沿用原路由，与来源续接逻辑一致。
func gatewayRouteChanged(resp *http.Response, value string) bool {
	if resp == nil {
		return false
	}
	now := time.Now()
	for _, cookie := range resp.Cookies() {
		if cookie.Name != "__oailb" {
			continue
		}
		if cookie.Value != value || cookie.MaxAge < 0 || (!cookie.Expires.IsZero() && !now.Before(cookie.Expires)) {
			return true
		}
	}
	return false
}
func gatewayFingerprint(r gatewayRoute) [32]byte {
	return sha256.Sum256([]byte(r.cookie + "\x00" + r.proxy + "\x00" + r.nodeID + "\x00" + r.identity))
}
func (g *codexGatewayManager) routeValidLocked(v harvest.GatewaySettings, r gatewayRoute, now time.Time) bool {
	a := g.owner.Store.FindByID(r.sourceID)
	return gatewayRouteRevision(g.settings) == gatewayRouteRevision(v) && v.CookiePool.Enabled && slices.Contains(v.CookiePool.SourceAccountIDs, r.sourceID) && now.Before(r.expires) && gatewayAccountEligible(a) && gatewayIdentity(a) == r.identity
}

func (g *codexGatewayManager) pickRoute(v harvest.GatewaySettings) (gatewayRoute, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	pick := func(id int64) (gatewayRoute, bool) {
		r, ok := g.routes[id]
		return r, ok && g.routeValidLocked(v, r, now)
	}
	if r, ok := pick(g.activeSource); ok {
		return r, true
	}
	for _, id := range v.CookiePool.SourceAccountIDs {
		if r, ok := pick(id); ok {
			g.activeSource = id
			return r, true
		}
	}
	return gatewayRoute{}, false
}
func (g *codexGatewayManager) prepare(ctx context.Context, v harvest.GatewaySettings) error {
	if !v.CookiePool.Enabled {
		return errors.New("cookie_pool_disabled")
	}
	for {
		g.mu.Lock()
		call := g.preparation
		if call == nil {
			break
		}
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-call.done:
			if !g.current(ctx, v) {
				return errors.New("configuration_changed")
			}
			if call.revision == gatewayRouteRevision(v) {
				return call.err
			}
			// A superseded preparation has drained; start the current generation.
		}
	}
	if time.Now().Before(g.retryAfter) {
		g.mu.Unlock()
		return errors.New("source_probe_cooldown")
	}
	call := &gatewayPreparation{done: make(chan struct{}), revision: gatewayRouteRevision(v)}
	g.preparation = call
	g.setup = harvest.GatewaySetup{State: "running"}
	g.mu.Unlock()
	call.err = g.prepareSources(ctx, v)
	g.mu.Lock()
	if gatewayRouteRevision(g.settings) == gatewayRouteRevision(v) {
		if call.err != nil {
			g.retryAfter = time.Now().Add(15 * time.Second)
			g.setup = harvest.GatewaySetup{State: "failed", Reason: gatewaySafeReason(call.err)}
		} else {
			g.retryAfter = time.Time{}
			g.setup = harvest.GatewaySetup{State: "ready"}
		}
	}
	g.preparation = nil
	close(call.done)
	g.mu.Unlock()
	return call.err
}
func (g *codexGatewayManager) prepareSources(ctx context.Context, v harvest.GatewaySettings) error {
	if gatewayUnified(v) {
		return g.prepareStoredSources(ctx, v)
	}
	if r, ok := g.pickRoute(v); ok && time.Until(r.expires) > 90*time.Second {
		return nil
	}
	nodes := []mihomo.HarvestNode{{}}
	pool := ""
	if v.CookiePool.RotateNodes {
		var err error
		nodes, pool, err = g.listNodes()
		if err != nil {
			return err
		}
		if len(nodes) == 0 {
			return errors.New("node_unavailable")
		}
	}
	g.mu.Lock()
	startNode := int(g.nodeCursor % uint64(len(nodes)))
	g.mu.Unlock()
	controls, _, err := g.owner.Controls.Controls(ctx)
	if err != nil {
		return err
	}
	budget := controls.Speed.MaxRequestsPerRound
	attemptedNodes := 0
	for scanned := 0; scanned < len(nodes) && attemptedNodes < v.CookiePool.MaxNodeAttempts; scanned++ {
		node := nodes[(startNode+scanned)%len(nodes)]
		g.mu.Lock()
		g.nodeCursor = uint64((startNode + scanned + 1) % len(nodes))
		nodeCooling := node.ID != "" && time.Now().Before(g.nodeCooldown[node.ID])
		g.mu.Unlock()
		if nodeCooling {
			continue
		}
		nodeAttempted := false
		for _, id := range v.CookiePool.SourceAccountIDs {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !g.current(ctx, v) {
				return errors.New("configuration_changed")
			}
			if budget <= 0 {
				return errors.New("no_qualified_source_route")
			}
			a := g.owner.Store.FindByID(id)
			if !gatewayAccountEligible(a) || a.GetActiveRequests() > 0 {
				continue
			}
			identity := gatewayIdentity(a)
			cooldownKey := fmt.Sprintf("%d:%s:%s", id, identity, node.ID)
			g.mu.Lock()
			cooling := time.Now().Before(g.sourceCooldown[cooldownKey])
			g.mu.Unlock()
			if cooling {
				continue
			}
			if !nodeAttempted {
				attemptedNodes++
				nodeAttempted = true
			}
			a.Mu().RLock()
			proxyURL := a.ProxyURL
			a.Mu().RUnlock()
			if proxyURL == "" {
				proxyURL = auth.ConfiguredCodexTicketSettings().HarvestProxyURL
			}
			r := gatewayRoute{sourceID: id, identity: identity, proxy: proxyURL}
			release := func() {}
			if node.ID != "" {
				effective, finish, pinErr := g.pinNode(ctx, node.ID, pool)
				if pinErr != nil {
					g.mu.Lock()
					g.nodeCooldown[node.ID] = time.Now().Add(time.Duration(v.CookiePool.NodeCooldownSeconds) * time.Second)
					g.mu.Unlock()
					break // A failed exit lease affects the node, not a donor credential.
				}
				proxyURL, release = effective, finish
				r.nodeID, r.nodeName, r.poolID, r.managed, r.proxy = node.ID, node.Name, pool, true, effective
			}
			budget--
			started := time.Now()
			shot, resp, probeErr := gatewayShot(ctx, a, proxyURL, "sse", NewUpstreamSessionUUID(), "", "", time.Duration(controls.Speed.AttemptTimeoutSeconds)*time.Second)
			release()
			if probeErr == nil && (shot.Status != http.StatusOK || !shot.Completed || shot.Model != gatewayModel || strings.TrimSpace(shot.Text) == "") {
				probeErr = errors.New("response_not_qualified")
			}
			if probeErr == nil {
				endpoint, _ := url.Parse(codexTicketUpstreamEndpoint())
				r.cookie, r.expires, probeErr = gatewayResponseCookie(resp, endpoint, time.Now(), v.CookiePool.TTLSeconds)
			}
			if !g.current(ctx, v) || !gatewayAccountEligible(g.owner.Store.FindByID(id)) || gatewayIdentity(g.owner.Store.FindByID(id)) != identity {
				return errors.New("configuration_changed")
			}
			checked := time.Now()
			r.captured, r.gateway = checked, gatewayCookieHost(r.cookie)
			status := harvest.GatewayRouteStatus{AccountID: id, State: "rejected", Reason: gatewaySafeReason(probeErr), CheckedAt: &checked, Gateway: r.gateway, ProxyNode: r.nodeName}
			g.mu.Lock()
			if gatewayRouteRevision(g.settings) == gatewayRouteRevision(v) {
				if probeErr == nil {
					if old, exists := g.routes[id]; exists && old.cookie == r.cookie {
						r.expires = minGatewayTime(r.expires, old.expires)
					}
					status.State, status.Reason, status.ExpiresAt = "candidate", "source_passed_pending_target", &r.expires
					g.routes[id], g.activeSource = r, id
					delete(g.sourceCooldown, cooldownKey)
				} else {
					// Another donor is still allowed to use this same healthy node.
					g.sourceCooldown[cooldownKey] = checked.Add(time.Duration(v.CookiePool.NodeCooldownSeconds) * time.Second)
				}
				g.statuses[id] = status
			}
			g.mu.Unlock()
			_ = g.owner.Repository.AppendGatewayHistory(ctx, harvest.GatewayHistoryRecord{SourceAccountID: id, Gateway: r.gateway, Action: "source", Success: probeErr == nil, Reason: status.Reason, CheckedAt: started})
			if probeErr == nil {
				return nil
			}
			if !harvestWait(ctx, time.Duration(controls.Speed.ProbeIntervalSeconds)*time.Second) {
				return ctx.Err()
			}
		}
	}
	return errors.New("no_qualified_source_route")
}
func gatewayStoredDonor(a *auth.Account, model string, now time.Time, protocol string) *auth.CodexTicket {
	if !gatewayAccountEligible(a) {
		return nil
	}
	t := a.CodexTicketForModel(model, now, 780, protocol)
	if t == nil || auth.NormalizeCodexTicketModel(t.Model) != model || t.Transport != protocol {
		return nil
	}
	if t.Standby.Valid(now, 780) && t.Standby.Model == model && t.Standby.Transport == protocol && (t.Standby.Identity == "" || t.Standby.Identity == gatewayIdentity(a)) && t.Standby.EffectiveExpiry().After(t.EffectiveExpiry()) {
		t = t.Standby
	}
	if t.EffectiveExpiry().Sub(now) <= 30*time.Second {
		return nil
	}
	return t
}

func (g *codexGatewayManager) prepareStoredSources(ctx context.Context, v harvest.GatewaySettings) error {
	if !g.owner.harvestMu.TryRLock() {
		return errors.New("source_busy")
	}
	defer g.owner.harvestMu.RUnlock()
	controls, _, _ := g.owner.Controls.Controls(ctx)
	budget := controls.Speed.MaxRequestsPerRound
	for _, model := range auth.ConfiguredCodexTicketSettings().Models {
		if err := g.prepareStoredModelSources(ctx, v, model, controls, &budget); err != nil {
			return err
		}
	}
	return nil
}

func (g *codexGatewayManager) prepareStoredModelSources(ctx context.Context, v harvest.GatewaySettings, model string, controls harvest.CodexHarvestControls, budget *int) error {
	for _, id := range v.CookiePool.SourceAccountIDs {
		if !g.current(ctx, v) {
			return errors.New("configuration_changed")
		}
		a := g.owner.Store.FindByID(id)
		if !gatewayAccountEligible(a) {
			continue
		}
		if gatewayStoredDonor(a, model, time.Now(), controls.Transport) != nil {
			return nil
		}
		if *budget < 2 {
			continue
		}
		scope, err := g.owner.Scope(ctx)
		if err != nil || !harvestAccountEligible(a, scope, true) || a.CodexTicketProbeCoolingDown(model, time.Now()) {
			continue
		}
		if _, busy := g.owner.working.LoadOrStore(id, true); busy {
			continue
		}
		g.owner.attempt(ctx, a, model, auth.ConfiguredCodexTicketSettings().HarvestProxyURL, controls, budget, map[string]bool{}, nil, "auto", "", 1)
		g.owner.working.Delete(id)
		if gatewayStoredDonor(a, model, time.Now(), controls.Transport) != nil {
			return nil
		}
	}
	return errors.New("no_qualified_source_route")
}

// gatewayShot is an explicit diagnostic, never invoked by the ordinary forwarding
// path. It always uses the selected account's own token and workspace.
func gatewayShot(ctx context.Context, a *auth.Account, proxyURL, protocol, session, state, cookie string, timeout time.Duration) (codexHarvestResponse, *http.Response, error) {
	if !gatewayAccountEligible(a) {
		return codexHarvestResponse{}, nil, errors.New("account_unavailable")
	}
	if timeout <= 0 {
		timeout = 25 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client, err := auth.BuildHTTPClientChecked(proxyURL)
	if err != nil {
		return codexHarvestResponse{}, nil, errors.New("node_unavailable")
	}
	defer client.CloseIdleConnections()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	body := codexTicketHarvestProbeBody(gatewayModel)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexTicketUpstreamEndpoint(), bytes.NewReader(body))
	if err != nil {
		return codexHarvestResponse{}, nil, err
	}
	if err = applyCodexRequestHeaders(req, a, a.GetAccessToken(), session, "", nil, nil); err != nil {
		return codexHarvestResponse{}, nil, err
	}
	req.Header.Del("Cookie")
	req.Header.Del(codexTurnStateHeader)
	req.Header.Set("session-id", session)
	req.Header.Set("session_id", session)
	req.Header.Set("Accept-Encoding", "identity")
	req.Close = true
	if state != "" {
		req.Header.Set(codexTurnStateHeader, state)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	if protocol == "websocket" {
		tr, ok := client.Transport.(*http.Transport)
		if !ok {
			return codexHarvestResponse{}, nil, errors.New("node_unavailable")
		}
		dialer := websocket.Dialer{Proxy: tr.Proxy, NetDialContext: tr.DialContext, TLSClientConfig: tr.TLSClientConfig, HandshakeTimeout: timeout}
		endpoint := *req.URL
		if endpoint.Scheme == "https" {
			endpoint.Scheme = "wss"
		} else {
			endpoint.Scheme = "ws"
		}
		h := req.Header.Clone()
		h.Del("Content-Type")
		h.Del("Accept")
		h.Set("OpenAI-Beta", "responses_websockets=2026-02-06")
		conn, resp, e := dialer.DialContext(ctx, endpoint.String(), h)
		if e != nil {
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
			return codexHarvestResponse{}, nil, errors.New("upstream_http_error")
		}
		defer conn.Close()
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		defer stop()
		conn.SetReadLimit(1 << 20)
		deadline, _ := ctx.Deadline()
		conn.SetReadDeadline(deadline)
		conn.SetWriteDeadline(deadline)
		out := codexHarvestResponse{Status: 200, State: observedCodexTurnState(resp.Header.Get(codexTurnStateHeader))}
		body, _ = sjson.SetBytes(body, "type", "response.create")
		body, _ = sjson.DeleteBytes(body, "stream")
		if state != "" {
			body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-state", state)
		}
		if e = conn.WriteMessage(websocket.TextMessage, body); e != nil {
			return out, resp, e
		}
		for total := 0; total <= 1<<20; {
			_, frame, e := conn.ReadMessage()
			if e != nil {
				return out, resp, e
			}
			total += len(frame)
			if e = consumeCodexHarvestEvent(frame, &out); e != nil {
				return out, resp, e
			}
			if out.Terminal {
				return out, resp, nil
			}
		}
		return out, resp, errors.New("response_not_qualified")
	}
	resp, err := client.Do(req)
	if err != nil {
		return codexHarvestResponse{}, nil, err
	}
	defer resp.Body.Close()
	out := codexHarvestResponse{Status: resp.StatusCode, State: observedCodexTurnState(resp.Header.Get(codexTurnStateHeader))}
	if resp.StatusCode != 200 {
		return out, resp, errors.New("upstream_http_error")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return out, resp, errors.New("response_not_qualified")
	}
	err = consumeCodexHarvestSSE(raw, &out)
	if out.Text == "" { // Some Responses servers emit text only in the completed event.
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "data:") {
				event := gjson.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
				for _, item := range event.Get("response.output").Array() {
					for _, content := range item.Get("content").Array() {
						if content.Get("type").String() == "output_text" {
							out.Text += content.Get("text").String()
						}
					}
				}
			}
		}
	}
	return out, resp, err
}
func (g *codexGatewayManager) pin(ctx context.Context, v harvest.GatewaySettings, r gatewayRoute, fallback string) (string, func(), error) {
	if !g.current(ctx, v) {
		return "", func() {}, errors.New("configuration_changed")
	}
	g.mu.Lock()
	current, exists := g.routes[r.sourceID]
	valid := exists && g.routeValidLocked(v, current, time.Now()) && gatewayFingerprint(current) == gatewayFingerprint(r)
	g.mu.Unlock()
	if !valid {
		return "", func() {}, errors.New("configuration_changed")
	}
	if r.nodeID != "" {
		return g.pinNode(ctx, r.nodeID, r.poolID)
	}
	if v.CookiePool.IPAffinity {
		return r.proxy, func() {}, nil
	}
	return fallback, func() {}, nil
}
func (g *codexGatewayManager) verify(ctx context.Context, v harvest.GatewaySettings, id int64, ws bool) error {
	select {
	case g.probeToken <- struct{}{}:
		defer func() { <-g.probeToken }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if !g.current(ctx, v) {
		return errors.New("configuration_changed")
	}
	a := g.owner.Store.FindByID(id)
	if !gatewayAccountEligible(a) {
		return errors.New("account_unavailable")
	}
	if ws {
		return g.owner.TestGatewayWS(ctx, id)
	}
	protocol := "sse"
	if gatewayUnified(v) {
		controls, _, err := g.owner.Controls.Controls(ctx)
		if err != nil {
			return err
		}
		if controls.Transport == "websocket" {
			return g.owner.testGatewayWebsocket(ctx, id, false)
		}
		if t := a.CodexTicketForModel(gatewayModel, time.Now(), 780, protocol); t != nil {
			return nil
		}
		if err := g.prepare(ctx, v); err != nil {
			return err
		}
		if !g.owner.harvestMu.TryLock() {
			return errors.New("target_validation_in_progress")
		}
		defer g.owner.harvestMu.Unlock()
		if a.CodexTicketProbeCoolingDown(gatewayModel, time.Now()) {
			return errors.New("source_probe_cooldown")
		}
		budget := 2
		event := g.owner.attempt(ctx, a, gatewayModel, auth.ConfiguredCodexTicketSettings().HarvestProxyURL, controls, &budget, map[string]bool{}, nil, "auto", "", 1)
		if event.Result != "success" {
			return errors.New("target_probe_failed")
		}
		return nil
	}
	r, ok := g.pickRoute(v)
	if !ok {
		if err := g.prepare(ctx, v); err != nil {
			return err
		}
		r, ok = g.pickRoute(v)
	}
	if !ok {
		return errors.New("no_qualified_source_route")
	}
	a.Mu().RLock()
	fallback := a.ProxyURL
	a.Mu().RUnlock()
	proxy, release, err := g.pin(ctx, v, r, fallback)
	if err != nil {
		return err
	}
	defer release()
	identity := gatewayIdentity(a)
	session := NewUpstreamSessionUUID()
	controls, _, _ := g.owner.Controls.Controls(ctx)
	timeout := time.Duration(controls.Speed.AttemptTimeoutSeconds) * time.Second
	first, resp, err := gatewayShot(ctx, a, proxy, protocol, session, "", "__oailb="+r.cookie, timeout)
	// 判据只看门票续接，不看 response.model：降级时上游仍会回报 astra。
	if err == nil && (!first.Completed || first.State == "") {
		err = errors.New("target_probe_failed")
	}
	cookie := "__oailb=" + r.cookie
	if err == nil {
		if gatewayRouteChanged(resp, r.cookie) {
			err = errors.New("target_route_changed")
		}
		for _, c := range resp.Cookies() {
			if c.Name == "__cflb" && c.Value != "" {
				cookie = "__cflb=" + c.Value + "; " + cookie
			}
		}
	}
	if err == nil && (!g.current(ctx, v) || gatewayIdentity(g.owner.Store.FindByID(id)) != identity || !time.Now().Before(r.expires)) {
		err = errors.New("configuration_changed")
	}
	if err == nil {
		second, resp, e := gatewayShot(ctx, a, proxy, protocol, session, first.State, cookie, timeout)
		err = e
		if err == nil && !second.Completed {
			err = errors.New("target_probe_failed")
		}
		if err == nil && second.State != "" && second.State != first.State {
			err = errors.New("target_probe_degraded")
		}
		if err == nil && gatewayRouteChanged(resp, r.cookie) {
			err = errors.New("target_route_changed")
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !g.current(ctx, v) || !gatewayAccountEligible(g.owner.Store.FindByID(id)) || gatewayIdentity(g.owner.Store.FindByID(id)) != identity {
		return errors.New("configuration_changed")
	}
	checked := time.Now()
	status := harvest.GatewayRouteStatus{AccountID: id, State: "rejected", Reason: gatewaySafeReason(err), CheckedAt: &checked, ExpiresAt: &r.expires, Gateway: r.gateway, ProxyNode: r.nodeName}
	if err == nil {
		status.State = "ready"
		status.Reason = "target_probe_passed"
	}
	g.mu.Lock()
	current, exists := g.routes[r.sourceID]
	if !exists || !g.routeValidLocked(v, current, checked) || gatewayFingerprint(current) != gatewayFingerprint(r) || gatewayIdentity(g.owner.Store.FindByID(id)) != identity {
		g.mu.Unlock()
		return errors.New("configuration_changed")
	}
	g.checks[id] = gatewayCheck{sourceID: r.sourceID, status: status, fingerprint: gatewayFingerprint(r), identity: identity}
	g.mu.Unlock()
	_ = g.owner.Repository.AppendGatewayHistory(ctx, harvest.GatewayHistoryRecord{SourceAccountID: r.sourceID, TargetAccountID: id, Gateway: r.gateway, Action: "verify", Success: err == nil, Reason: status.Reason, CheckedAt: checked})
	return err
}
