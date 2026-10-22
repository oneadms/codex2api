package proxy

import (
	"context"
	"github.com/codex2api/auth"
	"github.com/codex2api/internal/harvest"
	"time"
)

func (m *CodexHarvestManager) SyncHarvestScheduling(ctx context.Context) {
	v, err := m.GatewaySettings(ctx)
	if err != nil {
		return
	}
	scope, err := m.Scope(ctx)
	if err != nil {
		return
	}
	controls, _, err := m.Controls.Controls(ctx)
	if err != nil {
		return
	}
	for _, a := range m.Store.Accounts() {
		if ctx.Err() != nil {
			return
		}
		if !a.SupportsCodexTickets() {
			continue
		}
		owned, err := m.Repository.HarvestSchedulingState(ctx, a.ID())
		if err != nil {
			continue
		}
		a.Mu().RLock()
		blocked := a.HarvestModelBlocked
		a.Mu().RUnlock()
		m.Store.ApplyHarvestScheduling(a.ID(), owned.Mode == "account" && owned.Identity == gatewayIdentity(a), blocked, owned.BeforeGroups)
		if !v.TicketPoolScheduling || !auth.ConfiguredCodexTicketSettings().Enabled {
			continue
		}
		if !harvestAccountEligible(a, scope, false) {
			continue
		}
		ready := false
		for _, model := range auth.ConfiguredCodexTicketSettings().Models {
			if v.SchedulingMode == "model" && model != gatewayModel {
				continue
			}
			if a.CodexTicketForModel(model, time.Now(), auth.CodexTicketTargetLength(a.GetPlanType()), controls.Transport) != nil {
				ready = true
				break
			}
		}
		result, err := m.Repository.ApplyHarvestScheduling(ctx, a.ID(), gatewayIdentity(a), v, ready)
		if err != nil {
			continue
		}
		m.Store.ApplyHarvestScheduling(a.ID(), result.State.Mode == "account", result.ModelBlocked, result.State.BeforeGroups)
		if result.Changed {
			m.Store.ApplyAccountEnabled(a.ID(), result.Enabled)
			m.Store.ApplyAccountGroups(a.ID(), result.Groups)
			reason := "ticket_unavailable"
			if ready {
				reason = "ticket_ready"
			}
			m.appendEvent(harvest.Event{AccountID: a.ID(), Source: "scheduling", Stage: result.State.Mode, Result: reason, CreatedAt: time.Now()})
		}
	}
}
