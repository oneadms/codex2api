package auth

import (
	"encoding/base64"
	"encoding/binary"
	"testing"
	"time"
)

// 造一张可用的门票：57 字节固定头 + blocks 个 16 字节块。
// 第 0 字节 0x80，第 1..9 字节为签发时刻（大端 unix）。编码用带填充的 URL base64，
// 与 CodexTicketExpectedLength 的口径一致（个人版 292 含两个 '='）。
func buildTestTicketState(issuedAt time.Time, blocks int) string {
	raw := make([]byte, CodexTicketEnvelopeHeaderBytes+CodexTicketBlockBytes*blocks)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(issuedAt.Unix()))
	return base64.URLEncoding.EncodeToString(raw)
}

func TestCodexTicketValidityAndRefresh(t *testing.T) {
	blocks := CodexTicketExpectedBlocks("personal")
	expectedLen := CodexTicketExpectedLength(blocks)
	now := time.Now()
	valid := &CodexTicket{
		State:     buildTestTicketState(now, blocks),
		Cookie:    "ticket=fresh",
		Length:    expectedLen,
		IssuedAt:  now,
		ExpiresAt: now.Add(50 * time.Minute),
	}
	if !valid.Valid(now, expectedLen) {
		t.Fatal("fresh ticket must be valid")
	}
	// 长度不匹配。
	if len(valid.State) != expectedLen {
		t.Fatalf("expectedLen %d got %d", expectedLen, len(valid.State))
	}
	// 过期票无效。
	expired := *valid
	expired.ExpiresAt = now.Add(-time.Minute)
	if expired.Valid(now, expectedLen) {
		t.Fatal("expired ticket must be invalid")
	}
	// 长度不对的票无效。
	wrongLen := *valid
	wrongLen.Length = expectedLen - 1
	if wrongLen.Valid(now, expectedLen) {
		t.Fatal("wrong-length ticket must be invalid")
	}
	// 撤销的票无效。
	revoked := *valid
	revoked.Revoked = true
	if revoked.Valid(now, expectedLen) {
		t.Fatal("revoked ticket must be invalid")
	}
	// NeedsRefresh：进入重打窗口。
	if valid.NeedsRefresh(now, 60*time.Second) {
		t.Fatal("fresh ticket should not need a refresh")
	}
	late := now.Add(151 * time.Second)
	if !valid.NeedsRefresh(late, 60*time.Second) {
		t.Fatal("ticket entering refresh window must report NeedsRefresh")
	}
}

func TestCodexTicketShapeParse(t *testing.T) {
	now := time.Now()
	state := buildTestTicketState(now, CodexTicketPersonalBlocks)
	shape, err := ParseCodexTicketShape(state)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if shape.Blocks != CodexTicketPersonalBlocks {
		t.Fatalf("blocks = %d", shape.Blocks)
	}
	// 垃圾输入。
	for _, bad := range []string{"", "not-base64!", "a" + state, "ab\r\nc"} {
		if _, err := ParseCodexTicketShape(bad); err == nil {
			t.Fatalf("expected parse error for %q", bad)
		}
	}
}

func TestCodexTicketExpectedLength(t *testing.T) {
	if got := CodexTicketExpectedLength(CodexTicketPersonalBlocks); got != 292 {
		t.Fatalf("personal length = %d, want 292", got)
	}
	if got := CodexTicketExpectedLength(CodexTicketTeamBlocks); got == 292 {
		t.Fatal("team length must differ from personal")
	}
}

func TestCodexTicketTargetLengthTeam5xAliases(t *testing.T) {
	for _, plan := range []string{
		"team", "teamplus", "business", "enterprise", "team5x", "team-5x", "team_5x", "team 5x", " TEAM5X ",
		"self_serve_business_prolite", " SELF_SERVE_BUSINESS_PROLITE ",
	} {
		t.Run(plan, func(t *testing.T) {
			if got := CodexTicketExpectedBlocks(plan); got != CodexTicketTeamBlocks {
				t.Fatalf("plan %q blocks = %d, want %d", plan, got, CodexTicketTeamBlocks)
			}
			if got := CodexTicketExpectedLength(CodexTicketExpectedBlocks(plan)); got != 332 {
				t.Fatalf("plan %q expected length = %d, want 332", plan, got)
			}
		})
	}
	for _, plan := range []string{"", "free", "plus", "pro", "prolite", "pro_lite", "pro-lite", "personal", "unknown", "teamwork"} {
		if got := CodexTicketExpectedBlocks(plan); got != CodexTicketPersonalBlocks {
			t.Errorf("unrelated plan %q must retain personal blocks, got %d", plan, got)
		}
	}
}

func TestCodexTicketSharedPoolPrefersOwnAndFallsBackByPlanAndModel(t *testing.T) {
	ResetCodexTicketSharedPoolForTest()
	t.Cleanup(ResetCodexTicketSharedPoolForTest)
	now := time.Now()
	state := buildTestTicketState(now, CodexTicketTeamBlocks)
	shared := &CodexTicket{
		Model: "gpt-6-astra", State: state, Length: len(state), IssuedAt: now,
		Cookie:     "ticket=shared",
		CapturedAt: now, ExpiresAt: now.Add(50 * time.Minute),
	}
	source := &Account{DBID: 101, PlanType: "self_serve_business_prolite"}
	PublishCodexTicketToSharedPool(source, shared)

	destination := &Account{DBID: 102, PlanType: "self_serve_business_prolite"}
	if got, ok := destination.CodexTicketInjectionWithShared(now, CodexTicketExpectedLength(CodexTicketTeamBlocks), "gpt-6-astra"); !ok || got != state {
		t.Fatalf("same-plan account must fall back to shared ticket: got=%q ok=%v", got, ok)
	}
	if _, ok := (&Account{PlanType: "team"}).CodexTicketInjectionWithShared(now, CodexTicketExpectedLength(CodexTicketTeamBlocks), "gpt-6-astra"); ok {
		t.Fatal("a different raw plan type must not use the shared ticket")
	}
	if _, ok := destination.CodexTicketInjectionWithShared(now, CodexTicketExpectedLength(CodexTicketTeamBlocks), "gpt-5.5"); ok {
		t.Fatal("a different model must not use the shared ticket")
	}

	ownState := buildTestTicketState(now.Add(time.Second), CodexTicketTeamBlocks)
	destination.CodexTickets = map[string]*CodexTicket{
		"gpt-6-astra": {Model: "gpt-6-astra", State: ownState, Cookie: "ticket=own", Length: len(ownState), IssuedAt: now, ExpiresAt: now.Add(time.Hour)},
	}
	if got, ok := destination.CodexTicketInjectionWithShared(now, CodexTicketExpectedLength(CodexTicketTeamBlocks), "gpt-6-astra"); !ok || got != ownState {
		t.Fatalf("own ticket must take priority: got=%q ok=%v", got, ok)
	}
	if !RevokeSharedCodexTicket(source.GetPlanType(), "gpt-6-astra", state) {
		t.Fatal("matching shared ticket must be revocable")
	}
	if _, ok := (&Account{PlanType: source.GetPlanType()}).CodexTicketInjectionWithShared(now, CodexTicketExpectedLength(CodexTicketTeamBlocks), "gpt-6-astra"); ok {
		t.Fatal("revoked shared ticket must no longer be available")
	}
}

func TestCodexTicketInjection(t *testing.T) {
	blocks := CodexTicketPersonalBlocks
	now := time.Now()
	state := buildTestTicketState(now, blocks)
	account := &Account{
		PlanType: "personal",
		CodexTickets: map[string]*CodexTicket{
			"gpt-5.5": {
				State:     state,
				Cookie:    "ticket=local",
				Length:    len(state),
				IssuedAt:  now,
				ExpiresAt: now.Add(50 * time.Minute),
			},
		},
	}
	if got, ok := account.CodexTicketInjection(now, CodexTicketExpectedLength(blocks), "gpt-5", "gpt-5.5"); !ok || got != state {
		t.Fatalf("injection = %q, %v", got, ok)
	}
	// 模型不命中。
	if _, ok := account.CodexTicketInjection(now, CodexTicketExpectedLength(blocks), "claude-3"); ok {
		t.Fatal("must not inject for ungated model")
	}
	// 过期票取不到。
	expired := &Account{
		PlanType: "personal",
		CodexTickets: map[string]*CodexTicket{
			"gpt-5.5": {
				State:     state,
				Length:    len(state),
				IssuedAt:  now.Add(-2 * time.Hour),
				ExpiresAt: now.Add(-time.Hour),
			},
		},
	}
	if _, ok := expired.CodexTicketInjection(now, CodexTicketExpectedLength(blocks), "gpt-5.5"); ok {
		t.Fatal("must not inject expired ticket")
	}
	// 热备票在主票失效时顶上。
	withStandby := &Account{
		PlanType: "personal",
		CodexTickets: map[string]*CodexTicket{
			"gpt-5.5": {
				State:     state,
				Length:    len(state),
				IssuedAt:  now.Add(-2 * time.Hour),
				ExpiresAt: now.Add(-time.Hour),
				Standby: &CodexTicket{
					State:     state,
					Cookie:    "ticket=standby",
					Length:    len(state),
					IssuedAt:  now,
					ExpiresAt: now.Add(50 * time.Minute),
				},
			},
		},
	}
	if got, ok := withStandby.CodexTicketInjection(now, CodexTicketExpectedLength(blocks), "gpt-5.5"); !ok || got != state {
		t.Fatal("standby must step in when master is expired")
	}
}

func TestCodexTicketProbeCoolingDown(t *testing.T) {
	account := &Account{}
	now := time.Now()
	if account.CodexTicketProbeCoolingDown("gpt-5", now) {
		t.Fatal("no probe recorded must not be cooling down")
	}
	next := now.Add(5 * time.Minute)
	account.CodexTicketProbes = map[string]*CodexTicketProbeSummary{
		"gpt-5": {Result: "error", NextProbeAt: &next, CheckedAt: now},
	}
	if !account.CodexTicketProbeCoolingDown("gpt-5", now) {
		t.Fatal("must be cooling down before NextProbeAt")
	}
	if account.CodexTicketProbeCoolingDown("gpt-5", next.Add(time.Second)) {
		t.Fatal("must stop cooling down after NextProbeAt")
	}
	if account.CodexTicketProbeCoolingDown("gpt-5.5", now) {
		t.Fatal("different model must not be cooling down")
	}
}
