package proxy

import (
	"context"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/harvest"
	"github.com/codex2api/internal/mihomo"
)

// 快速复用只借来源的启动材料，目标仍使用自己的 OAuth、工作区及新会话。
func (m *CodexHarvestManager) reuseCandidate(ctx context.Context, target *auth.Account, model string, controls harvest.CodexHarvestControls, budget *int, tried map[string]bool) (*auth.CodexTicket, func()) {
	noop := func() {}
	sourceID := controls.ReuseSourceAccountID
	key := "reuse:" + model
	if sourceID <= 0 || sourceID == target.ID() || controls.Transport != "sse" || auth.CodexTicketTargetLength(target.GetPlanType()) != 780 || tried[key] || *budget < 2 {
		return nil, noop
	}
	tried[key] = true
	source := m.Store.FindByID(sourceID)
	scope, err := m.Scope(ctx)
	if err != nil || !harvestAccountEligible(source, scope, true) {
		return nil, noop
	}
	ticket := source.CodexTicketForModel(model, time.Now(), 780, "sse")
	if ticket == nil && !source.CodexTicketProbeCoolingDown(model, time.Now()) && *budget >= 4 {
		if _, busy := m.working.LoadOrStore(sourceID, true); !busy {
			// 来源补票与目标验证共用原预算，来源继续遵守范围、跳过和冷却。
			prepare, cancel := context.WithTimeout(ctx, 90*time.Second)
			m.attempt(prepare, source, model, auth.ConfiguredCodexTicketSettings().HarvestProxyURL, controls, budget, map[string]bool{}, nil, "auto", "", 1)
			cancel()
			m.working.Delete(sourceID)
			ticket = source.CodexTicketForModel(model, time.Now(), 780, "sse")
		}
	}
	if ticket == nil || *budget < 2 {
		return nil, noop
	}
	var release func()
	if ticket.ManagedNode {
		proxyURL, finish, err := mihomo.PinNode(ctx, ticket.HarvestNodeID, ticket.HarvestPoolID)
		if err != nil {
			return nil, noop
		}
		ticket.HarvestProxyURL, release = proxyURL, finish
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
	} else {
		release = noop
	}
	if ticket.HarvestProxyURL == "" {
		release()
		return nil, noop
	}
	ticket.SourceAccountID = sourceID
	return ticket, release
}
