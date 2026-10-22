package proxy

import (
	"context"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/harvest"
)

// Recheck the current runtime account before every sent request and before
// publishing. A detached/replaced account must not publish into its successor.
func (m *CodexHarvestManager) harvestAdmitted(ctx context.Context, account *auth.Account, identity, model string) bool {
	if ctx.Err() != nil || account == nil || m.Store.FindByID(account.ID()) != account || account.CodexTicketIdentitySnapshot() != identity || !auth.CodexTicketGateEnabled() || !auth.CodexTicketModelGated(model) {
		return false
	}
	scope, err := m.Scope(ctx)
	return err == nil && harvestAccountEligible(account, scope, true)
}

func harvestRetryAfterSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int((d + time.Second - 1) / time.Second)
}

// Preserve the largest server cooldown, including late parallel outcomes. An
// unrelated shorter failure must not shorten a previous Retry-After deadline.
func (m *CodexHarvestManager) recordHarvestProbe(account *auth.Account, model string, event harvest.Event, cooldownSeconds int, retryAfter time.Duration) {
	if event.Result == "cancelled" || event.Result == "skipped" {
		return
	}
	summary := &auth.CodexTicketProbeSummary{Result: event.Result, HTTPStatus: event.HTTPStatus, CheckedAt: time.Now()}
	if event.Result != "success" {
		next := time.Now().Add(max(time.Duration(cooldownSeconds)*time.Second, retryAfter))
		account.Mu().RLock()
		if previous := account.CodexTicketProbes[auth.NormalizeCodexTicketModel(model)]; previous != nil && previous.NextProbeAt != nil && previous.NextProbeAt.After(next) {
			next = *previous.NextProbeAt
		}
		account.Mu().RUnlock()
		summary.NextProbeAt = &next
	}
	m.Store.PublishCodexTicketProbe(account.ID(), model, summary)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = m.DB.UpdateCredentials(ctx, account.ID(), map[string]any{auth.CodexTicketProbeCredentialKey(model): summary})
}
