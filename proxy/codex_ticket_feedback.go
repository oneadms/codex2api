package proxy

import (
	"context"
	"log"
	"net/http"
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
func observeCodexTicketFeedback(account *auth.Account, injected, upstream string, status int) {
	if account == nil || !auth.CodexTicketGateEnabled() {
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
	targetLen := auth.CodexTicketTargetLengthFor(account.GetPlanType())

	// 规则 1：采纳上游新签发的票。
	if upstream != "" && upstream != injected && len(upstream) == targetLen {
		if shape, err := auth.ParseCodexTicketShape(upstream); err == nil && shape.Blocks == auth.CodexTicketExpectedBlocks(account.GetPlanType()) {
			model := codexTicketFeedbackModel(account, targetLen)
			if model != "" {
				publishCodexTicket(sink.db, sink.store, account, model, upstream, shape.IssuedAt)
				return
			}
		}
	}

	// 规则 2：注入的票被上游拒绝 → 撤销，让热备票顶上。
	if injected != "" && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
		model := codexTicketFeedbackModel(account, targetLen, injected)
		if model == "" {
			return
		}
		localRevoked := sink.store.RevokeCodexTicketForState(account.ID(), model, injected)
		sharedRevoked := auth.RevokeSharedCodexTicket(account.GetPlanType(), model, injected)
		if localRevoked || sharedRevoked {
			log.Printf("[codex-ticket] 账号 %d %s 门票被上游拒绝(status=%d)，已撤销%s", account.ID(), model, status, map[bool]string{true: "（含共享池）", false: ""}[sharedRevoked])
		}
	}
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
func publishCodexTicket(db *database.DB, store *auth.Store, account *auth.Account, model, state string, issuedAt time.Time) bool {
	if db == nil || store == nil || account == nil {
		return false
	}
	model = auth.NormalizeCodexTicketModel(model)
	if model == "" || state == "" {
		return false
	}
	capturedAt := time.Now()
	ticket := &auth.CodexTicket{
		Model:      model,
		State:      state,
		Length:     len(state),
		IssuedAt:   issuedAt,
		CapturedAt: capturedAt,
		ExpiresAt:  auth.CodexTicketExpiry(capturedAt, issuedAt, time.Duration(auth.ConfiguredCodexTicketSettings().TTLSeconds)*time.Second),
		Identity:   auth.CodexTicketIdentity(account.EffectiveAccountID(), account.Email),
	}
	if shape, err := auth.ParseCodexTicketShape(state); err == nil {
		ticket.Blocks = shape.Blocks
	}
	if err := db.UpdateCredentials(context.Background(), account.ID(), map[string]interface{}{
		auth.CodexTicketCredentialKey(model): ticket,
	}); err != nil {
		log.Printf("[codex-ticket] 账号 %d %s 门票落库失败: %v", account.ID(), model, err)
		return false
	}
	store.PublishCodexTicket(account.ID(), ticket)
	return true
}
