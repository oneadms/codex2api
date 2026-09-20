package auth

import (
	"encoding/base64"
	"encoding/binary"
	"testing"
	"time"
)

// codexTicketRowStub 是最小凭据行视图，覆盖 codexTicketCredentialSource 的两个方法。
type codexTicketRowStub struct {
	values  map[string]string
	entries map[string]any
}

func (r codexTicketRowStub) GetCredential(key string) string { return r.values[key] }
func (r codexTicketRowStub) CredentialEntries() map[string]any {
	return r.entries
}

func ticketStateForTest(issuedAt time.Time, blocks int) string {
	raw := make([]byte, CodexTicketEnvelopeHeaderBytes+CodexTicketBlockBytes*blocks)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(issuedAt.Unix()))
	return base64.URLEncoding.EncodeToString(raw)
}

// 门票从凭据行装载：只有 codex_ticket:* 前缀的键被认领，探测摘要按 :probe 后缀分开，
// 其它凭据键（包括手工注入的 codex_turn_state）一概不动。
func TestCodexTicketsFromRow(t *testing.T) {
	now := time.Now()
	state := ticketStateForTest(now, CodexTicketPersonalBlocks)
	row := codexTicketRowStub{
		values: map[string]string{"codex_turn_state": "manual-state"},
		entries: map[string]any{
			"refresh_token":    "rt-1",
			"codex_turn_state": "manual-state",
			"codex_ticket:gpt-5.5": map[string]any{
				"model":      "gpt-5.5",
				"state":      state,
				"length":     len(state),
				"issued_at":  now.Format(time.RFC3339),
				"expires_at": now.Add(50 * time.Minute).Format(time.RFC3339),
			},
			"codex_ticket:gpt-5.5:probe": map[string]any{
				"result":     "success",
				"checked_at": now.Format(time.RFC3339),
			},
		},
	}

	tickets := codexTicketsFromRow(row)
	if len(tickets) != 1 {
		t.Fatalf("tickets = %v", tickets)
	}
	ticket, ok := tickets["gpt-5.5"]
	if !ok || ticket.State != state || ticket.Length != len(state) {
		t.Fatalf("ticket = %+v", ticket)
	}
	// 探测摘要不能混进门票集合。
	if _, leaked := tickets["gpt-5.5:probe"]; leaked {
		t.Fatal("probe summary leaked into tickets")
	}

	probes := codexTicketProbesFromRow(row)
	if len(probes) != 1 || probes["gpt-5.5"] == nil || probes["gpt-5.5"].Result != "success" {
		t.Fatalf("probes = %v", probes)
	}

	// 装载后的账号能直接把票注入出去。
	account := &Account{PlanType: "personal", CodexTickets: tickets, CodexTicketProbes: probes}
	if got, ok := account.CodexTicketInjection(time.Now(), CodexTicketExpectedLength(CodexTicketPersonalBlocks), "gpt-5.5"); !ok || got != state {
		t.Fatalf("loaded ticket not injectable: %q %v", got, ok)
	}
	// 手工注入值与自动门票共存，互不覆盖。
	account.CodexTurnState = "manual-state"
	if got := account.CodexTurnStateInjection("gpt-5.5"); got != "manual-state" {
		t.Fatalf("manual value disturbed: %q", got)
	}
}

// 账号被重新授权（凭据整体换掉）后，内存里的旧票必须消失——整体重建而不是增量合并。
func TestSetCodexTicketsFromRowLockedRebuilds(t *testing.T) {
	now := time.Now()
	state := ticketStateForTest(now, CodexTicketPersonalBlocks)
	account := &Account{}
	account.setCodexTicketsFromRowLocked(codexTicketRowStub{
		entries: map[string]any{
			"codex_ticket:gpt-5.5": map[string]any{"model": "gpt-5.5", "state": state, "length": len(state)},
		},
	})
	if len(account.CodexTickets) != 1 {
		t.Fatalf("initial load = %v", account.CodexTickets)
	}
	// 换一份没有任何门票的凭据行：旧票必须被清掉。
	account.setCodexTicketsFromRowLocked(codexTicketRowStub{entries: map[string]any{"refresh_token": "rt-2"}})
	if len(account.CodexTickets) != 0 || len(account.CodexTicketProbes) != 0 {
		t.Fatalf("stale tickets survived reload: %v %v", account.CodexTickets, account.CodexTicketProbes)
	}
}

// 坏数据不能让装载 panic，也不能把半个门票塞进注入路径。
func TestCodexTicketsFromRowSkipsMalformed(t *testing.T) {
	row := codexTicketRowStub{
		entries: map[string]any{
			"codex_ticket:empty":   map[string]any{"model": "empty", "state": "   "},
			"codex_ticket:notjson": "just a string",
			"codex_ticket:":        map[string]any{"state": "gAAAAAstate"},
		},
	}
	tickets := codexTicketsFromRow(row)
	if len(tickets) != 0 {
		t.Fatalf("malformed entries must be skipped, got %v", tickets)
	}
	if tickets == nil {
		t.Fatal("must return an empty map, not nil, so callers can index it")
	}
}
