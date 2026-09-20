package auth

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// SupportsCodexTickets reports whether this account uses the Codex token-based
// authentication flow. Keep harvesting and request gating on the same scope;
// signature-based accounts cannot obtain a ticket through this flow.
func (a *Account) SupportsCodexTickets() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.supportsCodexTicketsLocked()
}

func (a *Account) supportsCodexTicketsLocked() bool {
	upstream := strings.ToLower(strings.TrimSpace(a.UpstreamType))
	return (upstream == "" || upstream == "codex") && !a.isRelayStyleLocked() &&
		!strings.EqualFold(strings.TrimSpace(a.CodexAuthMode), CodexAuthModeAgentIdentity)
}

// CanHarvestCodexTicket limits background probes to enabled Codex OAuth/session
// accounts. The shared Store also contains other providers' credentials, which
// must never be sent to the Codex endpoint. Token-only imports and accounts that
// still need their first refresh are both supported.
func (a *Account) CanHarvestCodexTicket(now time.Time) bool {
	if a == nil || atomic.LoadInt32(&a.Disabled) != 0 || atomic.LoadInt32(&a.DispatchPaused) != 0 {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.supportsCodexTicketsLocked() {
		return false
	}
	if a.Status == StatusError || a.healthTierLocked() == HealthTierBanned ||
		(a.Status == StatusCooldown && now.Before(a.CooldownUtil)) {
		return false
	}
	return strings.TrimSpace(a.AccessToken) != "" || strings.TrimSpace(a.RefreshToken) != "" || strings.TrimSpace(a.SessionToken) != ""
}

// EnsureCodexTicketAccessToken uses the normal refresh path, including rotating
// refresh-token locking and durable credential publication. Refreshes retain the
// account's regular egress; only the ticket probe uses the harvest proxy.
// forceRefresh is used once after an upstream 401, never after a generic 403.
func (s *Store) EnsureCodexTicketAccessToken(ctx context.Context, account *Account, forceRefresh bool) error {
	if s == nil || !account.CanHarvestCodexTicket(time.Now()) {
		return fmt.Errorf("账号不支持 Codex 自动打票或当前不可用")
	}
	account.mu.RLock()
	accessToken := strings.TrimSpace(account.AccessToken)
	hasRefreshCredential := strings.TrimSpace(account.RefreshToken) != "" || strings.TrimSpace(account.SessionToken) != ""
	expiresAt := account.ExpiresAt
	account.mu.RUnlock()
	if hasRefreshCredential && (forceRefresh || accessToken == "" || time.Until(expiresAt) < 5*time.Minute) {
		if err := s.refreshAccountWithOptions(ctx, account, forceRefresh); err != nil {
			return fmt.Errorf("刷新 Codex access token 失败: %w", err)
		}
		if account.GetAccessToken() == "" {
			return fmt.Errorf("刷新后账号仍无 access token")
		}
		return nil
	}
	if forceRefresh {
		return fmt.Errorf("上游拒绝 access token，且账号无 refresh_token/session_token；请重新授权或导入有效凭据")
	}
	if accessToken == "" {
		return fmt.Errorf("账号无 access token，且无可用刷新凭据")
	}
	// An AT-only import may have no expiry metadata. Let the upstream validate
	// it, but reject a token whose expiry is known to have passed.
	if !expiresAt.IsZero() && !time.Now().Before(expiresAt) {
		return fmt.Errorf("access token 已过期，且账号无 refresh_token/session_token；请重新授权或导入有效凭据")
	}
	return nil
}
