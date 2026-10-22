package proxy

import (
	"context"
	"encoding/json"
	"github.com/codex2api/auth"
	"github.com/codex2api/internal/harvest"
	"sync"
	"time"
)

type harvestCoverageRow struct {
	harvest.AccountCoverage
	until time.Time
}
type harvestCoverageState struct {
	mu               sync.Mutex
	signature        string
	started, updated time.Time
	rows             map[int64]harvestCoverageRow
}

func (m *CodexHarvestManager) ObserveHarvestCoverage(ctx context.Context) {
	scope, err := m.Scope(ctx)
	if err != nil {
		return
	}
	controls, _, err := m.Controls.Controls(ctx)
	if err != nil {
		return
	}
	settings := auth.ConfiguredCodexTicketSettings()
	raw, _ := json.Marshal([]any{scope, controls.Transport, settings.Models, settings.TargetLength, settings.Enabled})
	c := &m.gatewayManager().coverage
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.signature != string(raw) || c.rows == nil {
		c.signature = string(raw)
		c.started = now
		c.rows = map[int64]harvestCoverageRow{}
	}
	seen := map[int64]bool{}
	if settings.Enabled {
		for _, a := range m.Store.Accounts() {
			if !harvestAccountEligible(a, scope, false) {
				continue
			}
			seen[a.ID()] = true
			row := c.rows[a.ID()]
			if row.StartedAt.IsZero() {
				row.StartedAt = now
			}
			elapsed := now.Sub(row.UpdatedAt)
			if !row.UpdatedAt.IsZero() && elapsed > 0 && elapsed <= 15*time.Second {
				ready := minGatewayTime(now, row.until).Sub(row.UpdatedAt)
				if ready < 0 {
					ready = 0
				}
				row.ReadySeconds += ready.Seconds()
				row.MissingSeconds += (elapsed - ready).Seconds()
			}
			row.UpdatedAt = now
			row.until = time.Time{}
			for _, model := range settings.Models {
				if ticket := a.CodexTicketForModel(model, now, auth.CodexTicketTargetLength(a.GetPlanType()), controls.Transport); ticket != nil && ticket.EffectiveExpiry().After(row.until) {
					row.until = ticket.EffectiveExpiry()
				}
			}
			if total := row.ReadySeconds + row.MissingSeconds; total > 0 {
				p := 100 * row.ReadySeconds / total
				row.Percent = &p
			}
			c.rows[a.ID()] = row
		}
	}
	for id := range c.rows {
		if !seen[id] {
			delete(c.rows, id)
		}
	}
	c.updated = now
}
func (m *CodexHarvestManager) HarvestCoverage(context.Context) harvest.CoverageSnapshot {
	c := &m.gatewayManager().coverage
	c.mu.Lock()
	defer c.mu.Unlock()
	out := harvest.CoverageSnapshot{SampleIntervalSeconds: 5, Accounts: map[int64]harvest.AccountCoverage{}}
	if c.started.IsZero() {
		return out
	}
	started, updated := c.started, c.updated
	out.StartedAt = &started
	out.UpdatedAt = &updated
	sum := 0.0
	for id, row := range c.rows {
		out.Accounts[id] = row.AccountCoverage
		if row.Percent != nil {
			out.AccountCount++
			out.AverageReadySeconds += row.ReadySeconds
			out.AverageMissingSeconds += row.MissingSeconds
			sum += *row.Percent
		}
	}
	if out.AccountCount > 0 {
		n := float64(out.AccountCount)
		out.AverageReadySeconds /= n
		out.AverageMissingSeconds /= n
		sum /= n
		out.AveragePercent = &sum
	}
	return out
}
