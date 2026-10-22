package proxy

import (
	"context"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// 反馈自愈：业务请求本身也在向上游铸造门票——上游每次响应都可能回带一个比我们手上
// 更新的 turn state。把这个值收下来，就能在两次后台打票之间免费换新票，也省下打票
// 代理的出口 IP 配额。
//
// 两条规则：
//  1. 上游回带的值形状合法且与本次注入值不同 → 采纳（上游刚签发的，一定比手上的新）。
//  2. 本次注入了门票但上游以 401/403 拒绝 → 撤销该票（让热备票立刻顶上），并在下一轮
//     重新打票。这是唯一能立刻发现"票已被上游作废"的信号。
//
// 其余情况一律不动：宁可留着旧票等后台重打，也不要基于误判把好票删掉。

// codexTicketFeedbackSink 是反馈回写所需的依赖。打票循环启动时注册；未启用打票时
// 为空，反馈路径直接短路（因此不需要 store/db 常驻在 trace 结构里）。
type codexTicketFeedbackSink struct {
	db    *database.DB
	store *auth.Store
}

var codexTicketFeedback atomic.Pointer[codexTicketFeedbackSink]

// 发布和反馈串行提交，避免旧请求覆盖后台刚发布的新票。
var codexTicketPublishMu sync.Mutex

// registerCodexTicketFeedback 由打票循环启动时调用，注册反馈回写目标。
func registerCodexTicketFeedback(db *database.DB, store *auth.Store) {
	if db == nil || store == nil {
		return
	}
	codexTicketFeedback.Store(&codexTicketFeedbackSink{db: db, store: store})
}

// observeCodexTicketFeedback 在每次出站 Codex 请求拿到响应后调用。injected 是本次
// 注入的门票（空表示未注入），upstream 是上游回带的 turn state，status 是响应状态码。
//
// 这是尽力而为的旁路：任何失败只记日志，绝不影响请求本身的处理。
func observeCodexTicketFeedback(account *auth.Account, injected, upstream string, status int, cookies codexTicketCookies, selected ...*auth.CodexTicket) {
	if !account.SupportsCodexTickets() || !auth.CodexTicketGateEnabled() {
		return
	}
	injected = observedCodexTurnState(injected)
	upstream = observedCodexTurnState(upstream)
	if injected == "" && upstream == "" {
		return
	}
	sink := codexTicketFeedback.Load()
	if sink == nil {
		return
	}
	adoptionLen := auth.CodexTicketTargetLength(account.GetPlanType())
	var binding *auth.CodexTicket
	if len(selected) > 0 {
		binding = selected[0]
	}
	model := codexTicketFeedbackModel(account, 0, injected)
	if binding != nil {
		model = binding.Model
	}
	if binding != nil && binding.Length == 780 && status >= 200 && status < 300 {
		// 成功响应也可能重签或换路由；这时撤销原票并重新采验。
		if (upstream == "" || upstream == injected) && cookies.Header == binding.Cookie {
			return
		}
		status = http.StatusForbidden
	}

	// 规则 1：采纳上游续签的新状态。成功的状态轮换属于同一张票的续期，
	// 直接更新当前主票；既不要求同时回带 Cookie，也不能生成备用票。
	if status >= 200 && status < 300 && injected != "" && upstream != "" && len(upstream) == adoptionLen && adoptionLen != 780 {
		if shape, err := auth.ParseCodexTicketShape(upstream); err == nil && model != "" {
			if upstream == injected && (cookies.Header == "" || (binding != nil && cookies.Header == binding.Cookie)) {
				return
			}
			if adoptCodexTicketRotation(sink, account, model, injected, upstream, shape, cookies, binding) {
				return
			}
		}
	}

	// 规则 2：注入的票被上游拒绝 → 撤销，让热备票顶上。
	// 返回非目标形态时撤销实际发出的票，不重放可能已经计费的业务请求。
	mismatch := status >= 200 && status < 300 && upstream != "" && len(upstream) != adoptionLen
	if status >= 200 && status < 300 && upstream != "" {
		shape, err := auth.ParseCodexTicketShape(upstream)
		mismatch = mismatch || err != nil || auth.CodexTicketExpectedLength(shape.Blocks) != adoptionLen
	}
	if injected != "" && (status == http.StatusUnauthorized || status == http.StatusForbidden || mismatch) {
		if model == "" {
			return
		}
		codexTicketPublishMu.Lock()
		localRevoked := revokeCodexTicketOwners(sink, account.GetPlanType(), model, injected)
		sharedRevoked := auth.RevokeSharedCodexTicket(account.GetPlanType(), model, injected)
		codexTicketPublishMu.Unlock()
		KickCodexTicketHarvest()
		if localRevoked || sharedRevoked {
			log.Printf("[codex-ticket] 账号 %d %s 门票被上游拒绝(status=%d)，已撤销%s", account.ID(), model, status, map[bool]string{true: "（含共享池）", false: ""}[sharedRevoked])
		}
	}
}

// adoptCodexTicketRotation 把业务响应回带的新状态续接到当前主票上。对齐上游：
// 状态轮换是同一张票的续期，直接替换主票，备用票保持不变；只有 Cookie 真的
// 换新时才更新 Cookie 及其独立计时。
func adoptCodexTicketRotation(sink *codexTicketFeedbackSink, account *auth.Account, model, injected, upstream string, shape auth.CodexTicketShape, cookies codexTicketCookies, binding *auth.CodexTicket) bool {
	if sink == nil || sink.db == nil || sink.store == nil || account == nil || injected == "" {
		return false
	}
	model = auth.NormalizeCodexTicketModel(model)
	if model == "" || upstream == "" {
		return false
	}
	if binding != nil && binding.State != injected {
		return false
	}
	codexTicketPublishMu.Lock()
	defer codexTicketPublishMu.Unlock()
	current := account.CodexTicketSnapshot(model)
	if current == nil || current.Revoked || current.State != injected {
		return false
	}
	now := time.Now()
	next := *current
	next.State = upstream
	next.Length = len(upstream)
	next.Blocks = shape.Blocks
	next.IssuedAt = shape.IssuedAt
	next.CapturedAt = now
	next.ExpiresAt = auth.CodexTicketExpiry(now, shape.IssuedAt, time.Duration(auth.ConfiguredCodexTicketSettings().TTLSeconds)*time.Second)
	if cookies.Header != "" && cookies.Header != current.Cookie {
		next.Cookie = auth.NormalizeCodexTicketCookie(cookies.Header)
		next.CookieExpiresAt = cookies.ExpiresAt
		next.CookieCapturedAt = cookies.CapturedAt
		if next.CookieCapturedAt.IsZero() {
			next.CookieCapturedAt = now
		}
	}
	if !next.CookieExpiresAt.IsZero() && next.CookieExpiresAt.Before(next.ExpiresAt) {
		next.ExpiresAt = next.CookieExpiresAt
	}
	if !next.Valid(now, 0) {
		return false
	}
	writeCtx, writeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer writeCancel()
	if err := sink.db.UpdateCredentials(writeCtx, account.ID(), map[string]interface{}{
		auth.CodexTicketCredentialKey(model): next,
	}); err != nil {
		log.Printf("[codex-ticket] 账号 %d %s 门票续签落库失败: %v", account.ID(), model, err)
		return false
	}
	sink.store.AdoptCodexTicket(account.ID(), &next)
	return true
}

// 共享票被拒绝后同时撤销来源账号中的副本，防止账号重载把旧票重新放回共享池。
// 按实际发出的状态匹配，来源账号后来发布的新票不受影响。
func revokeCodexTicketOwners(sink *codexTicketFeedbackSink, planType, model, state string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	revoked := false
	for _, owner := range sink.store.Accounts() {
		if !strings.EqualFold(strings.TrimSpace(owner.GetPlanType()), strings.TrimSpace(planType)) || !sink.store.RevokeCodexTicketForState(owner.ID(), model, state) {
			continue
		}
		revoked = true
		if err := sink.db.UpdateCredentials(ctx, owner.ID(), map[string]interface{}{auth.CodexTicketCredentialKey(model): owner.CodexTicketSnapshot(model)}); err != nil {
			log.Printf("[codex-ticket] 账号 %d 门票撤销落库失败", owner.ID())
		}
	}
	return revoked
}

// codexTicketFeedbackModel 找出本次被注入的门控模型。共享票可能不在当前账号的
// CodexTickets 中，因此必须同时检查账号本地票和同套餐共享池。
func codexTicketFeedbackModel(account *auth.Account, targetLen int, expected ...string) string {
	if account == nil {
		return ""
	}
	match := ""
	if len(expected) > 0 {
		match = expected[0]
	}
	now := time.Now()
	for _, model := range auth.ConfiguredCodexTicketSettings().Models {
		if !auth.CodexTicketModelGated(model) {
			continue
		}
		if state, ok := account.CodexTicketInjectionWithShared(now, targetLen, model); ok && (match == "" || state == match) {
			return model
		}
	}
	return ""
}

// publishCodexTicket 落库并即时发布一张门票。落库先行：写失败时内存里就不该出现
// 一张重启即丢的票。
func publishCodexTicket(db *database.DB, store *auth.Store, account *auth.Account, model, state string, issuedAt time.Time, cookies codexTicketCookies, bindings ...*auth.CodexTicket) bool {
	if db == nil || store == nil || account == nil {
		return false
	}
	model = auth.NormalizeCodexTicketModel(model)
	if model == "" || state == "" {
		return false
	}
	codexTicketPublishMu.Lock()
	defer codexTicketPublishMu.Unlock()
	capturedAt := time.Now()
	if cookies.CapturedAt.IsZero() {
		cookies.CapturedAt = capturedAt
	}
	ticket := &auth.CodexTicket{
		Model:            model,
		State:            state,
		Cookie:           auth.NormalizeCodexTicketCookie(cookies.Header),
		CookieExpiresAt:  cookies.ExpiresAt,
		CookieCapturedAt: cookies.CapturedAt,
		Length:           len(state),
		IssuedAt:         issuedAt,
		CapturedAt:       capturedAt,
		ExpiresAt:        auth.CodexTicketExpiry(capturedAt, issuedAt, time.Duration(auth.ConfiguredCodexTicketSettings().TTLSeconds)*time.Second),
		Identity:         account.CodexTicketIdentitySnapshot(),
	}
	if len(bindings) > 0 && bindings[0] != nil {
		binding := bindings[0]
		if binding.Identity != "" {
			if binding.Identity != ticket.Identity {
				return false
			}
			// Keep the identity used by the probe. A concurrent account edit can
			// invalidate this ticket, but can never relabel it as the new identity.
			ticket.Identity = binding.Identity
		}
		ticket.EdgeIP = binding.EdgeIP
		ticket.HarvestProxyURL, ticket.HarvestSessionID = binding.HarvestProxyURL, binding.HarvestSessionID
		ticket.HarvestNodeID, ticket.HarvestNodeName, ticket.HarvestPoolID = binding.HarvestNodeID, binding.HarvestNodeName, binding.HarvestPoolID
		ticket.Transport, ticket.ManagedNode = binding.Transport, binding.ManagedNode
		ticket.Acquisition, ticket.SourceAccountID = binding.Acquisition, binding.SourceAccountID
		// 业务响应换票或更换路由后必须重新验收，不能继承旧票的通过标记。
		ticket.ProbeVerified = binding.ProbeVerified && (binding.State == "" || binding.State == state) && (binding.Cookie == "" || binding.Cookie == ticket.Cookie)
		if binding.State != "" {
			// 只允许当前票的反馈续链，迟到的旧响应不能覆盖刚更换的主票。
			if current := account.CodexTicketWithShared(capturedAt, 0, model); current == nil || current.State != binding.State {
				return false
			}
		}
	}
	if shape, err := auth.ParseCodexTicketShape(state); err == nil {
		ticket.Blocks = shape.Blocks
	}
	if !cookies.ExpiresAt.IsZero() && cookies.ExpiresAt.Before(ticket.ExpiresAt) {
		ticket.ExpiresAt = cookies.ExpiresAt
	}
	if !ticket.Valid(capturedAt, 0) {
		return false
	}
	if previous := account.CodexTicketForModel(model, capturedAt, 0, ticket.Transport); previous != nil && previous.State != state {
		previous.Standby = ticket
		ticket = previous
	}
	writeCtx, writeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer writeCancel()
	if err := db.UpdateCredentials(writeCtx, account.ID(), map[string]interface{}{
		auth.CodexTicketCredentialKey(model): ticket,
	}); err != nil {
		log.Printf("[codex-ticket] 账号 %d %s 门票落库失败: %v", account.ID(), model, err)
		return false
	}
	store.PublishCodexTicket(account.ID(), ticket)
	return true
}
