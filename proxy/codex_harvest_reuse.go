package proxy

import (
	"context"
	"github.com/codex2api/auth"
	"github.com/codex2api/internal/harvest"
	"github.com/codex2api/internal/mihomo"
	"slices"
	"time"
)

type gatewayReuseGuardKey struct{}
type gatewayReuseGuard struct {
	revision, identity string
	targetID           int64
}

func (m *CodexHarvestManager) reuseCandidate(ctx context.Context, target *auth.Account, model string, controls harvest.CodexHarvestControls, budget *int, tried map[string]bool) (*auth.CodexTicket, func()) {
	noop := func() {}
	key := "reuse:" + model
	if target == nil || model != gatewayModel || controls.Transport != "sse" || auth.CodexTicketTargetLength(target.GetPlanType()) != 780 || tried[key] || *budget < 2 {
		return nil, noop
	}
	v, err := m.GatewaySettings(ctx)
	if err != nil || !gatewayUnified(v) || !slices.Contains(v.CookiePool.TargetAccountIDs, target.ID()) {
		return nil, noop
	}
	tried[key] = true
	var ticket *auth.CodexTicket
	sourceID := int64(0)
	for _, id := range v.CookiePool.SourceAccountIDs {
		if id == target.ID() {
			continue
		}
		source := m.Store.FindByID(id)
		if !gatewayAccountEligible(source) {
			continue
		}
		t := source.CodexTicketForModel(model, time.Now(), 780, "sse")
		if t == nil {
			continue
		}
		if t.Standby != nil && t.Standby.Valid(time.Now(), 780) && t.Standby.Transport == "sse" && (t.Standby.Identity == "" || t.Standby.Identity == gatewayIdentity(source)) && t.Standby.EffectiveExpiry().After(t.EffectiveExpiry()) {
			t = t.Standby
		}
		if time.Until(t.EffectiveExpiry()) <= 30*time.Second {
			continue
		}
		ticket = t
		sourceID = id
		break
	}
	if ticket == nil {
		return nil, noop
	}
	release := noop
	if ticket.ManagedNode {
		p, finish, err := mihomo.PinNode(ctx, ticket.HarvestNodeID, ticket.HarvestPoolID)
		if err != nil {
			return nil, noop
		}
		ticket.HarvestProxyURL = p
		release = finish
	} else if ticket.HarvestNodeID != "" {
		sidecar, err := mihomo.LoadDirectedSidecar(m.DataDir, ticket.HarvestProxyURL)
		if err != nil || sidecar.PoolID != ticket.HarvestPoolID {
			return nil, noop
		}
		node, ok := sidecar.Lookup(ctx, ticket.HarvestNodeID, "")
		if !ok {
			return nil, noop
		}
		finish, err := sidecar.Acquire(ctx, node)
		if err != nil {
			return nil, noop
		}
		release = finish
	}
	if ticket.HarvestProxyURL == "" {
		release()
		return nil, noop
	}
	ticket.SourceAccountID = sourceID
	ticket.ReuseRevision = gatewayRouteRevision(v)
	return ticket, release
}
