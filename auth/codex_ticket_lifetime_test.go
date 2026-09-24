package auth

import (
	"strings"
	"testing"
	"time"
)

func TestCodexTicketShortLifetimeAndLegacyCache(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	ticket := &CodexTicket{
		Model: "gpt-5.5", State: buildTestTicketState(now, CodexTicketPersonalBlocks),
		Cookie: "ticket=paired", Length: 292,
		CapturedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	// 缺少 issued_at 的旧记录也必须从信封读取时间，不能借旧 TTL 延长有效期。
	for _, tc := range []struct {
		age  time.Duration
		want bool
	}{
		{0, true}, {209 * time.Second, true}, {210 * time.Second, false},
		{240 * time.Second, false}, {5 * time.Minute, false},
	} {
		if got := ticket.Valid(now.Add(tc.age), ticket.Length); got != tc.want {
			t.Errorf("age=%s valid=%v, want %v", tc.age, got, tc.want)
		}
	}
	if got := ticket.EffectiveExpiry(); !got.Equal(now.Add(210 * time.Second)) {
		t.Fatalf("effective expiry=%s", got)
	}
	account := &Account{CodexTickets: map[string]*CodexTicket{"gpt-5.5": ticket}}
	status := account.CodexTicketStatuses([]string{"gpt-5.5"}, now, ticket.Length)[0]
	if !status.Ready || status.RemainingSeconds != 210 {
		t.Fatalf("legacy status must use the new lifetime: %+v", status)
	}
	if !ticket.NeedsRefresh(now.Add(150*time.Second), 60*time.Second) {
		t.Fatal("legacy cache must refresh before its effective expiry")
	}
	for _, cookie := range []string{"", " ", "broken", "sid=x\r\nX-Test: yes", "sid=x\t", "sid=" + strings.Repeat("x", 8192)} {
		copy := *ticket
		copy.Cookie = cookie
		if copy.Valid(now, copy.Length) {
			t.Fatal("missing or malformed cookies must make the ticket unusable")
		}
	}
	ticket.CookieExpiresAt = now.Add(15 * time.Second)
	if ticket.Valid(now.Add(15*time.Second), ticket.Length) || !ticket.EffectiveExpiry().Equal(ticket.CookieExpiresAt) {
		t.Fatal("cookie expiry must also bound ticket validity")
	}
}

func TestCodexTicketSharedAndStandbyCookiesStayPaired(t *testing.T) {
	ResetCodexTicketSharedPoolForTest()
	t.Cleanup(ResetCodexTicketSharedPoolForTest)
	now := time.Now().Truncate(time.Second)
	standby := &CodexTicket{
		Model: "gpt-5.5", State: buildTestTicketState(now.Add(-time.Second), CodexTicketPersonalBlocks),
		Cookie: "ticket=standby", Length: 292, CapturedAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Minute),
	}
	main := &CodexTicket{
		Model: "gpt-5.5", State: buildTestTicketState(now, CodexTicketPersonalBlocks),
		Cookie: "ticket=main", Length: 292, CapturedAt: now, ExpiresAt: now.Add(time.Minute), Standby: standby,
	}
	source := &Account{DBID: 701, PlanType: "plus"}
	PublishCodexTicketToSharedPool(source, main)
	destination := &Account{DBID: 702, PlanType: "plus"}
	got := destination.CodexTicketWithShared(now, 292, "gpt-5.5")
	if got == nil || got.Cookie != main.Cookie || got.State != main.State {
		t.Fatal("shared ticket must carry its own cookie")
	}
	got.Cookie = "ticket=changed-copy"
	RevokeSharedCodexTicket("plus", "gpt-5.5", main.State)
	got = destination.CodexTicketWithShared(now, 292, "gpt-5.5")
	if got == nil || got.Cookie != standby.Cookie || got.State != standby.State {
		t.Fatal("standby promotion must switch both state and cookie")
	}
}

func TestCodexTicketLegacySettingsUseShortRefreshCycle(t *testing.T) {
	settings, err := ParseCodexTicketSettings(`{"ttl_seconds":3600,"refresh_before_seconds":600,"probe_interval_seconds":180}`)
	if err != nil {
		t.Fatal(err)
	}
	if settings.TTLSeconds != 240 || settings.RefreshBeforeSeconds != 60 || settings.ProbeIntervalSeconds != 30 {
		t.Fatalf("legacy settings not migrated: %+v", settings)
	}
}
