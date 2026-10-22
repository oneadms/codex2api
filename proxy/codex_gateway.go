package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/harvest"
)

var ErrGatewayRevisionConflict = errors.New("借票配置已变化，请刷新后重新保存")

const gatewayModel = "gpt-6-astra"

type gatewayRoute struct {
	sourceID                 int64
	identity                 string
	cookie                   string
	proxy                    string
	nodeID, nodeName, poolID string
	managed                  bool
	gateway                  string
	captured, expires        time.Time
}
type gatewayPreparation struct {
	done     chan struct{}
	err      error
	revision string
}
type gatewayCheck struct {
	status      harvest.GatewayRouteStatus
	fingerprint [32]byte
	identity    string
}
type codexGatewayManager struct {
	coverage     harvestCoverageState
	owner        *CodexHarvestManager
	mu           sync.Mutex
	settings     harvest.GatewaySettings
	loaded       bool
	loadedUntil  time.Time
	routes       map[int64]gatewayRoute
	statuses     map[int64]harvest.GatewayRouteStatus
	checks       map[int64]gatewayCheck
	activeSource int64
	preparation  *gatewayPreparation
	retryAfter   time.Time
	nodeCooldown map[string]time.Time
	lastTest     *harvest.GatewayTestResult
	setup        harvest.GatewaySetup
	cancel       context.CancelFunc
	stopped      bool
	anchors      map[string]*gatewayAnchor
}

func (m *CodexHarvestManager) gatewayManager() *codexGatewayManager {
	m.gatewayOnce.Do(func() {
		m.gateway = &codexGatewayManager{owner: m, settings: harvest.DefaultGatewaySettings(), routes: map[int64]gatewayRoute{}, statuses: map[int64]harvest.GatewayRouteStatus{}, checks: map[int64]gatewayCheck{}, nodeCooldown: map[string]time.Time{}, anchors: map[string]*gatewayAnchor{}}
	})
	return m.gateway
}
func cloneGatewaySettings(v harvest.GatewaySettings) harvest.GatewaySettings {
	v.CookiePool.SourceAccountIDs = slices.Clone(v.CookiePool.SourceAccountIDs)
	v.CookiePool.TargetAccountIDs = slices.Clone(v.CookiePool.TargetAccountIDs)
	v.WSSession.AccountIDs = slices.Clone(v.WSSession.AccountIDs)
	v.SchedulingGroupIDs = slices.Clone(v.SchedulingGroupIDs)
	return v
}
func (m *CodexHarvestManager) GatewaySettings(ctx context.Context) (harvest.GatewaySettings, error) {
	g := m.gatewayManager()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.loaded && time.Now().Before(g.loadedUntil) {
		return cloneGatewaySettings(g.settings), nil
	}
	query, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	raw, err := m.Repository.GetValue(query, harvest.GatewaySettingsKey)
	if errors.Is(err, harvest.ErrSettingNotFound) {
		err = nil
		raw = ""
	}
	if err != nil {
		return cloneGatewaySettings(g.settings), err
	}
	v := harvest.DefaultGatewaySettings()
	if raw != "" {
		if err = json.Unmarshal([]byte(raw), &v); err != nil {
			return cloneGatewaySettings(g.settings), errors.New("借票配置读取失败")
		}
		v.Normalize()
		if err = v.Validate(); err != nil {
			return cloneGatewaySettings(g.settings), err
		}
	}
	if g.loaded && !gatewayRouteSettingsEqual(g.settings, v) {
		g.resetLocked()
	}
	g.settings = v
	g.loaded = true
	g.loadedUntil = time.Now().Add(3 * time.Second)
	return cloneGatewaySettings(v), nil
}
func gatewayRouteSettingsEqual(a, b harvest.GatewaySettings) bool {
	a.TicketPoolScheduling = false
	b.TicketPoolScheduling = false
	a.SchedulingMode = ""
	b.SchedulingMode = ""
	a.SchedulingGroupIDs = nil
	b.SchedulingGroupIDs = nil
	a.Revision = ""
	b.Revision = ""
	aa, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(aa) == string(bb)
}
func (g *codexGatewayManager) resetLocked() {
	if g.cancel != nil {
		g.cancel()
		g.cancel = nil
	}
	g.routes = map[int64]gatewayRoute{}
	g.statuses = map[int64]harvest.GatewayRouteStatus{}
	g.checks = map[int64]gatewayCheck{}
	g.activeSource = 0
	g.retryAfter = time.Time{}
	// Active turns keep their own snapshot but cannot publish into this generation.
	g.anchors = map[string]*gatewayAnchor{}
}
func (m *CodexHarvestManager) SaveGatewaySettings(ctx context.Context, v harvest.GatewaySettings) (harvest.GatewaySettings, error) {
	current, err := m.GatewaySettings(ctx)
	if err != nil {
		return v, err
	}
	v.Normalize()
	if err = v.Validate(); err != nil {
		return v, err
	}
	ids := append(slices.Clone(v.CookiePool.SourceAccountIDs), v.CookiePool.TargetAccountIDs...)
	ids = append(ids, v.WSSession.AccountIDs...)
	for _, id := range ids {
		a := m.Store.FindByID(id)
		if a == nil || !a.SupportsCodexTickets() {
			return v, fmt.Errorf("所选账号 %d 不存在或不支持 Codex 借票", id)
		}
	}
	g := m.gatewayManager()
	g.mu.Lock()
	if v.Revision != g.settings.Revision || current.Revision != g.settings.Revision {
		g.mu.Unlock()
		return v, ErrGatewayRevisionConflict
	}
	v.Revision = NewUpstreamSessionUUID()
	raw, _ := json.Marshal(v)
	if err = m.Repository.Set(ctx, harvest.GatewaySettingsKey, string(raw)); err != nil {
		g.mu.Unlock()
		return v, errors.New("保存借票配置失败")
	}
	routeChanged := !gatewayRouteSettingsEqual(g.settings, v)
	if routeChanged {
		g.resetLocked()
	}
	g.settings = cloneGatewaySettings(v)
	g.loaded = true
	g.loadedUntil = time.Now().Add(3 * time.Second)
	if !v.CookiePool.Enabled {
		g.setup = harvest.GatewaySetup{State: "disabled"}
	}
	g.mu.Unlock()
	if routeChanged && v.CookiePool.Enabled {
		g.schedulePreparation(v.Revision)
	}
	KickCodexTicketHarvest()
	return v, nil
}
func (m *CodexHarvestManager) GatewayReuseCurrent(ctx context.Context, revision string, targetID int64) bool {
	v, err := m.GatewaySettings(ctx)
	return err == nil && v.Revision == revision && gatewayUnified(v) && slices.Contains(v.CookiePool.TargetAccountIDs, targetID)
}
func gatewayUnified(v harvest.GatewaySettings) bool {
	return v.CookiePool.Enabled && auth.CodexTicketGateEnabled() && auth.ConfiguredCodexTicketSettings().TargetLength == 780 && auth.CodexTicketModelGated(gatewayModel)
}
func gatewayIdentity(a *auth.Account) string {
	if a == nil {
		return ""
	}
	a.Mu().RLock()
	email := a.Email
	a.Mu().RUnlock()
	return auth.CodexTicketIdentity(a.EffectiveAccountID(), email)
}
func gatewayAccountEligible(a *auth.Account) bool {
	return a != nil && a.CanHarvestCodexTicket(time.Now()) && a.GetAccessToken() != ""
}
func (g *codexGatewayManager) current(ctx context.Context, v harvest.GatewaySettings) bool {
	now, err := g.owner.GatewaySettings(ctx)
	return err == nil && now.Revision == v.Revision && now.CookiePool.Enabled
}
func (g *codexGatewayManager) schedulePreparation(revision string) {
	g.mu.Lock()
	if g.stopped {
		g.mu.Unlock()
		return
	}
	if g.cancel != nil {
		g.cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	g.cancel = cancel
	g.setup = harvest.GatewaySetup{State: "queued"}
	g.mu.Unlock()
	if !g.owner.DB.RunBackgroundTask(func(lifecycle context.Context) {
		stop := context.AfterFunc(lifecycle, cancel)
		defer stop()
		defer cancel()
		v, err := g.owner.GatewaySettings(ctx)
		if err != nil || v.Revision != revision {
			return
		}
		_, _ = g.owner.TestGateway(ctx, "prepare", 0)
	}) {
		cancel()
	}
}
func (g *codexGatewayManager) start() {
	g.owner.DB.RunBackgroundTask(func(ctx context.Context) {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				g.mu.Lock()
				g.stopped = true
				g.resetLocked()
				g.mu.Unlock()
				return
			case <-ticker.C:
				sample, cancel := context.WithTimeout(ctx, 4*time.Second)
				g.owner.ObserveHarvestCoverage(sample)
				g.owner.SyncHarvestScheduling(sample)
				cancel()
			}
		}
	})
}
func (m *CodexHarvestManager) GatewaySnapshot(ctx context.Context) harvest.GatewayRuntime {
	v, err := m.GatewaySettings(ctx)
	out := harvest.GatewayRuntime{Revision: v.Revision, GeneratedAt: time.Now(), Mode: "disabled", Sources: []harvest.GatewayRouteStatus{}, Targets: []harvest.GatewayRouteStatus{}, WS: []harvest.GatewayWSStatus{}}
	if err != nil {
		out.Setup = harvest.GatewaySetup{State: "failed", Reason: "settings_unavailable"}
		return out
	}
	if v.CookiePool.Enabled {
		out.Mode = "cookie_route"
		if gatewayUnified(v) {
			out.Mode = "unified_ticket"
		}
	}
	g := m.gatewayManager()
	g.mu.Lock()
	out.Preparing = g.preparation != nil
	out.Setup = g.setup
	if g.lastTest != nil {
		copy := *g.lastTest
		out.LastTest = &copy
	}
	now := time.Now()
	project := func(id int64, target bool) harvest.GatewayRouteStatus {
		row := harvest.GatewayRouteStatus{AccountID: id, State: "waiting", Reason: "not_tested"}
		if !v.CookiePool.Enabled {
			row.State = "disabled"
			row.Reason = "disabled"
			return row
		}
		if out.Mode == "unified_ticket" {
			a := m.Store.FindByID(id)
			if !gatewayAccountEligible(a) {
				row.Reason = "account_unavailable"
				return row
			}
			controls, _, _ := m.Controls.Controls(ctx)
			t := a.CodexTicketForModel(gatewayModel, now, 780, controls.Transport)
			if t != nil {
				expiry := t.EffectiveExpiry()
				row.State = "ready"
				row.Reason = "target_probe_passed"
				if !target {
					row.State = "candidate"
					row.Reason = "source_passed_pending_target"
				}
				row.ExpiresAt = &expiry
				row.RemainingSeconds = max(0, int64(expiry.Sub(now).Seconds()))
				row.ProxyNode = t.HarvestNodeName
			}
			return row
		}
		if !target {
			if old, ok := g.statuses[id]; ok {
				row = old
			}
			if r, ok := g.routes[id]; ok {
				row.Gateway = r.gateway
				row.ProxyNode = r.nodeName
				row.ExpiresAt = &r.expires
				row.RemainingSeconds = max(0, int64(r.expires.Sub(now).Seconds()))
				row.Active = g.activeSource == id
				if !now.Before(r.expires) {
					row.State = "expired"
					row.Reason = "route_expired"
				}
			}
			return row
		}
		if c, ok := g.checks[id]; ok {
			row = c.status
			if row.ExpiresAt != nil && !now.Before(*row.ExpiresAt) {
				row.State = "expired"
				row.Reason = "route_expired"
			}
			if row.ExpiresAt != nil {
				row.RemainingSeconds = max(0, int64(row.ExpiresAt.Sub(now).Seconds()))
			}
		}
		return row
	}
	for _, id := range v.CookiePool.SourceAccountIDs {
		out.Sources = append(out.Sources, project(id, false))
	}
	for _, id := range v.CookiePool.TargetAccountIDs {
		r := project(id, true)
		out.Targets = append(out.Targets, r)
		if r.State == "ready" {
			out.ReadyRoutes++
		}
	}
	for _, id := range v.WSSession.AccountIDs {
		row := harvest.GatewayWSStatus{AccountID: id, Reason: "waiting"}
		for key, a := range g.anchors {
			if !now.Before(a.expires) && !a.busy {
				delete(g.anchors, key)
				continue
			}
			if a.accountID == id && now.Before(a.expires) {
				row.ActiveSessions++
				row.Ready = true
				row.Reason = "ready"
			}
		}
		out.WS = append(out.WS, row)
	}
	g.mu.Unlock()
	return out
}
func gatewaySafeReason(err error) string {
	if err == nil {
		return "success"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	// Only locally constructed error codes are exposed, never proxy/upstream text.
	switch err.Error() {
	case "cookie_pool_disabled", "configuration_changed", "source_probe_cooldown", "no_qualified_source_route", "target_probe_failed", "target_route_changed", "target_probe_degraded", "target_validation_in_progress", "account_unavailable", "source_busy", "routing_cookie_missing", "upstream_http_error", "response_not_qualified", "node_unavailable", "node_cooling_down", "rotation_use_once", "websocket_ticket_unavailable", "websocket_disabled":
		return err.Error()
	}
	return "request_failed"
}
func (m *CodexHarvestManager) TestGateway(ctx context.Context, action string, id int64) (harvest.GatewayTestResult, error) {
	result := harvest.GatewayTestResult{Action: action, AccountID: id, CheckedAt: time.Now()}
	start := time.Now()
	if action != "prepare" && action != "verify" && action != "ws" {
		return result, errors.New("诊断操作必须是 prepare、verify 或 ws")
	}
	v, err := m.GatewaySettings(ctx)
	if err != nil {
		return result, err
	}
	if !v.CookiePool.Enabled {
		return result, errors.New("cookie_pool_disabled")
	}
	if action != "prepare" && !slices.Contains(v.CookiePool.TargetAccountIDs, id) {
		return result, errors.New("目标账号不在借票范围内")
	}
	g := m.gatewayManager()
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if action == "prepare" {
		err = g.prepare(ctx, v)
	} else {
		err = g.verify(ctx, v, id, action == "ws")
	}
	result.Success = err == nil
	result.Reason = gatewaySafeReason(err)
	result.DurationMS = time.Since(start).Milliseconds()
	g.mu.Lock()
	if g.settings.Revision == v.Revision {
		g.lastTest = &result
	}
	g.mu.Unlock()
	_ = m.Repository.AppendGatewayHistory(ctx, harvest.GatewayHistoryRecord{TargetAccountID: id, Action: action, Success: result.Success, Reason: result.Reason, CheckedAt: result.CheckedAt})
	// A completed diagnostic failure is data, not an infrastructure HTTP 500.
	return result, nil
}
