package proxy

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func testTicketState(issuedAt time.Time, blocks int) string {
	raw := make([]byte, auth.CodexTicketEnvelopeHeaderBytes+auth.CodexTicketBlockBytes*blocks)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(issuedAt.Unix()))
	return base64.URLEncoding.EncodeToString(raw)
}

// withCodexTicketGate 在测试期间打开门控（开关+代理+名单），并在结束后恢复。
func withCodexTicketGate(t *testing.T, models ...string) {
	t.Helper()
	previous := auth.ConfiguredCodexTicketSettings()
	t.Cleanup(func() { auth.SetConfiguredCodexTicketSettings(previous) })
	auth.SetConfiguredCodexTicketSettings(auth.CodexTicketSettings{
		Enabled:              true,
		HarvestProxyURL:      "socks5h://127.0.0.1:1080",
		TargetLength:         auth.CodexTicketDefaultTargetLength,
		TTLSeconds:           auth.CodexTicketDefaultTTLSeconds,
		RefreshBeforeSeconds: auth.CodexTicketDefaultRefreshBeforeSecs,
		ProbeIntervalSeconds: auth.CodexTicketDefaultProbeIntervalSecs,
		CooldownSeconds:      auth.CodexTicketDefaultCooldownSecs,
		MaxProbesPerRound:    auth.CodexTicketDefaultMaxProbesPerRound,
		ProbeTimeoutSeconds:  auth.CodexTicketDefaultProbeTimeoutSecs,
		FailClosed:           true,
		Models:               models,
	})
}

func newTicketAccount(id int64, model string) *auth.Account {
	state := testTicketState(time.Now(), auth.CodexTicketPersonalBlocks)
	return &auth.Account{
		DBID:     id,
		PlanType: "personal",
		CodexTickets: map[string]*auth.CodexTicket{
			model: {
				Model:     model,
				State:     state,
				Length:    len(state),
				IssuedAt:  time.Now(),
				ExpiresAt: time.Now().Add(50 * time.Minute),
			},
		},
	}
}

// 自动门票必须在没有手工配置时补位，且同样压过客户端回带值。
func TestPrepareCodexTurnStateInjectionFallsBackToTicket(t *testing.T) {
	withCodexTicketGate(t, "gpt-5.5")
	account := newTicketAccount(1, "gpt-5.5")
	want := account.CodexTickets["gpt-5.5"].State

	ctx, body, headers := prepareCodexTurnStateInjection(context.Background(), account, []byte(`{"model":"gpt-5.5"}`), nil, false)
	if got := headers.Get("X-Codex-Turn-State"); got != want {
		t.Fatalf("ticket injection = %q, want ticket", got)
	}
	if got := CodexTurnStateInjectionFromContext(ctx); got != want {
		t.Fatalf("ctx = %q, want ticket", got)
	}
	if gjson := string(body); gjson == "" {
		t.Fatal("body must be returned")
	}

	// 手工值优先于自动门票。
	manual := newTicketAccount(2, "gpt-5.5")
	manual.CodexTurnState = "manual-state"
	_, _, headers = prepareCodexTurnStateInjection(context.Background(), manual, []byte(`{"model":"gpt-5.5"}`), nil, false)
	if got := headers.Get("X-Codex-Turn-State"); got != "manual-state" {
		t.Fatalf("manual must win: %q", got)
	}
}

// 门控未命中模型时自动门票不注入（与手工注入的名单语义一致）。
func TestPrepareCodexTurnStateInjectionTicketScopeMiss(t *testing.T) {
	withCodexTicketGate(t, "gpt-5.5")
	account := newTicketAccount(3, "gpt-5.5")
	_, _, headers := prepareCodexTurnStateInjection(context.Background(), account, []byte(`{"model":"claude-3"}`), nil, false)
	if headers != nil {
		t.Fatalf("ungated model must be a no-op, got %v", headers)
	}
}

// 门控未开启时即便账号手上有票也不注入——门票不能脱离门控单独生效。
func TestPrepareCodexTurnStateInjectionTicketRequiresGate(t *testing.T) {
	previous := auth.ConfiguredCodexTicketSettings()
	t.Cleanup(func() { auth.SetConfiguredCodexTicketSettings(previous) })
	auth.SetConfiguredCodexTicketSettings(auth.DefaultCodexTicketSettings())

	account := newTicketAccount(4, "gpt-5.5")
	_, _, headers := prepareCodexTurnStateInjection(context.Background(), account, []byte(`{"model":"gpt-5.5"}`), nil, false)
	if headers != nil {
		t.Fatal("ticket must not inject while the gate is disabled")
	}
}

// FailClosed：门控模型上没有可用门票时拒绝出站，且报 503。
func TestExecuteRequestFailClosedWithoutTicket(t *testing.T) {
	withCodexTicketGate(t, "gpt-5.5")
	account := &auth.Account{DBID: 5, PlanType: "personal", AccessToken: "token-1"}
	_, err := ExecuteRequest(context.Background(), account, []byte(`{"model":"gpt-5.5","input":"hi"}`), "", "", "api-key-1", nil, http.Header{}, false)
	if err == nil {
		t.Fatal("expected fail-closed rejection")
	}
	proxyErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error type = %T", err)
	}
	if proxyErr.HTTPStatus != http.StatusServiceUnavailable || !proxyErr.Retryable {
		t.Fatalf("expected 503 retryable, got %d retryable=%v", proxyErr.HTTPStatus, proxyErr.Retryable)
	}
}

// FailClosed 关闭时无票放行（裸打上游，由上游自己拒绝）。
func TestCodexTicketGateBlockedHonorsFailClosedOff(t *testing.T) {
	previous := auth.ConfiguredCodexTicketSettings()
	t.Cleanup(func() { auth.SetConfiguredCodexTicketSettings(previous) })
	withCodexTicketGate(t, "gpt-5.5")
	settings := auth.ConfiguredCodexTicketSettings()
	settings.FailClosed = false
	auth.SetConfiguredCodexTicketSettings(settings)

	account := &auth.Account{DBID: 6, PlanType: "personal"}
	if model, blocked := CodexTicketGateBlocked(context.Background(), account, "", "gpt-5.5"); blocked {
		t.Fatalf("must not block with FailClosed off (model %q)", model)
	}
}

// 门控判定：有票放行、无票拦截、非门控模型放行、手工值放行。
func TestCodexTicketGateBlocked(t *testing.T) {
	withCodexTicketGate(t, "gpt-5.5")

	if model, blocked := CodexTicketGateBlocked(context.Background(), newTicketAccount(7, "gpt-5.5"), "", "gpt-5.5"); blocked {
		t.Fatalf("account with ticket must pass, blocked on %q", model)
	}
	if _, blocked := CodexTicketGateBlocked(context.Background(), &auth.Account{DBID: 8, PlanType: "personal"}, "", "gpt-5.5"); !blocked {
		t.Fatal("account without ticket must be blocked")
	}
	if _, blocked := CodexTicketGateBlocked(context.Background(), &auth.Account{DBID: 9, PlanType: "personal"}, "", "claude-3"); blocked {
		t.Fatal("ungated model must not be blocked")
	}
	manual := &auth.Account{DBID: 10, PlanType: "personal", CodexTurnState: "manual"}
	if _, blocked := CodexTicketGateBlocked(context.Background(), manual, "", "gpt-5.5"); blocked {
		t.Fatal("manual injection must bypass the gate")
	}
}

// 打票必须读到成功终态才认；response.failed 与截断流都不算成功。
func TestValidateCodexHarvestStream(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		wantTerm bool
		wantOK   bool
	}{
		{name: "completed", body: "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n", wantTerm: true, wantOK: true},
		{name: "incomplete", body: "data: {\"type\":\"response.incomplete\"}\n\n", wantTerm: true, wantOK: true},
		{name: "failed", body: "data: {\"type\":\"response.failed\"}\n\n", wantTerm: false, wantOK: false},
		{name: "error frame", body: "data: {\"type\":\"error\"}\n\n", wantTerm: false, wantOK: false},
		{name: "truncated", body: "data: {\"type\":\"response.output_text.delta\"}\n\n", wantTerm: false, wantOK: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			term, ok := validateCodexHarvestStream(strings.NewReader(tc.body))
			if term != tc.wantTerm || ok != tc.wantOK {
				t.Fatalf("got term=%v ok=%v, want term=%v ok=%v", term, ok, tc.wantTerm, tc.wantOK)
			}
		})
	}
}

// 端到端打票：命中假上游 → 门票落库并发布到内存账号。
func TestHarvestOneTicketPublishesTicket(t *testing.T) {
	withCodexTicketGate(t, "gpt-5.5")
	state := testTicketState(time.Now(), auth.CodexTicketPersonalBlocks)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(codexTurnStateHeader, state)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\"}\n\n"))
	}))
	defer server.Close()

	previousURL := codexTicketProbeURLForTest
	codexTicketProbeURLForTest = server.URL
	t.Cleanup(func() { codexTicketProbeURLForTest = previousURL })

	account := &auth.Account{DBID: 11, PlanType: "personal", AccessToken: "token-1"}
	res := fireCodexTicketHarvest(context.Background(), account, "gpt-5.5", server.URL, 5*time.Second)
	if res.Result != "success" || res.State != state {
		t.Fatalf("harvest result = %q state-match=%v err=%v", res.Result, res.State == state, res.Err)
	}
}

// 上游 401 归类为 token_error，并给出冷却时刻。
func TestFireCodexTicketHarvestTokenError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	previousURL := codexTicketProbeURLForTest
	codexTicketProbeURLForTest = server.URL
	t.Cleanup(func() { codexTicketProbeURLForTest = previousURL })

	account := &auth.Account{DBID: 12, PlanType: "personal", AccessToken: "token-1"}
	res := fireCodexTicketHarvest(context.Background(), account, "gpt-5.5", server.URL, 5*time.Second)
	if res.Result != "token_error" || res.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("result = %q status = %d", res.Result, res.HTTPStatus)
	}
}

// 没有打票代理时绝不回落到业务代理。
func TestFireCodexTicketHarvestRequiresProxy(t *testing.T) {
	account := &auth.Account{DBID: 13, PlanType: "personal", AccessToken: "token-1"}
	res := fireCodexTicketHarvest(context.Background(), account, "gpt-5.5", "", 5*time.Second)
	if res.Result != "error" || res.State != "" {
		t.Fatalf("result = %q", res.Result)
	}
}

// 探测请求体是最小 ping 回合，且带 model。
func TestCodexTicketHarvestProbeBody(t *testing.T) {
	body := string(codexTicketHarvestProbeBody("gpt-5.5"))
	for _, want := range []string{`"model":"gpt-5.5"`, `"store":false`, `"stream":true`, "ping"} {
		if !strings.Contains(body, want) {
			t.Fatalf("probe body missing %s: %s", want, body)
		}
	}
}

// 反馈自愈：上游回带一张长度正确的新票时被采纳。
func TestCodexTicketFeedbackAdoptsUpstreamState(t *testing.T) {
	withCodexTicketGate(t, "gpt-5.5")
	state := testTicketState(time.Now(), auth.CodexTicketPersonalBlocks)
	account := &auth.Account{DBID: 14, PlanType: "personal"}
	account.CodexTickets = map[string]*auth.CodexTicket{
		"gpt-5.5": {State: "old", Length: len(state), IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute)},
	}
	// 未注册 sink 时不得 panic，也不得改动账号。
	previousSink := codexTicketFeedback.Load()
	codexTicketFeedback.Store(nil)
	t.Cleanup(func() { codexTicketFeedback.Store(previousSink) })
	observeCodexTicketFeedback(account, "old", state, http.StatusOK)
	if got := account.CodexTickets["gpt-5.5"].State; got != "old" {
		t.Fatalf("without a registered sink the ticket must be untouched, got %q", got)
	}

	// 注册 sink 但数据库不可用：落库失败，内存里也不该出现一张重启即丢的票。
	store := &auth.Store{}
	store.SetAccountsForTest([]*auth.Account{account})
	registerCodexTicketFeedback(nil, store)
	observeCodexTicketFeedback(account, "old", state, http.StatusOK)
	if got := account.CodexTickets["gpt-5.5"].State; got != "old" {
		t.Fatalf("without a database the ticket must not be published, got %q", got)
	}
}

// 反馈只采纳形状与长度都合规的值：长度不对一律丢弃，避免把垃圾塞进注入路径。
func TestCodexTicketFeedbackRejectsMalformedState(t *testing.T) {
	withCodexTicketGate(t, "gpt-5.5")
	account := &auth.Account{DBID: 16, PlanType: "personal"}
	account.CodexTickets = map[string]*auth.CodexTicket{
		"gpt-5.5": {State: "old", Length: 292, IssuedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute)},
	}
	previousSink := codexTicketFeedback.Load()
	codexTicketFeedback.Store(nil)
	t.Cleanup(func() { codexTicketFeedback.Store(previousSink) })

	observeCodexTicketFeedback(account, "old", "gAAAAAshort", http.StatusOK)
	if got := account.CodexTickets["gpt-5.5"].State; got != "old" {
		t.Fatalf("malformed upstream state must be ignored, got %q", got)
	}
}

// 被上游拒绝的票要能立刻撤销，让热备票顶上。
func TestRevokeCodexTicketPromotesStandby(t *testing.T) {
	now := time.Now()
	state := testTicketState(now, auth.CodexTicketPersonalBlocks)
	account := &auth.Account{DBID: 15, PlanType: "personal"}
	account.CodexTickets = map[string]*auth.CodexTicket{
		"gpt-5.5": {
			State:     state,
			Length:    len(state),
			IssuedAt:  now,
			ExpiresAt: now.Add(time.Minute),
			Standby: &auth.CodexTicket{
				State:     "standby-state",
				Length:    len(state),
				IssuedAt:  now,
				ExpiresAt: now.Add(50 * time.Minute),
			},
		},
	}
	store := &auth.Store{}
	store.SetAccountsForTest([]*auth.Account{account})
	if !store.RevokeCodexTicket(account.DBID, "gpt-5.5") {
		t.Fatal("revoke must report a change")
	}
	if got := account.CodexTickets["gpt-5.5"].State; got != "standby-state" {
		t.Fatalf("standby must be promoted, got %q", got)
	}
}

func TestCodexTicketGateSkipsUnsupportedAccounts(t *testing.T) {
	withCodexTicketGate(t, "gpt-6-astra")
	for _, account := range []*auth.Account{
		{UpstreamType: "grok", AccessToken: "grok-at"},
		{UpstreamType: "claude", AccessToken: "claude-at"},
		{UpstreamType: "openai_responses", APIKey: "relay-key"},
		{UpstreamType: "future-provider", AccessToken: "other-at"},
		{CodexAuthMode: auth.CodexAuthModeAgentIdentity},
	} {
		if _, blocked := CodexTicketGateBlocked(context.Background(), account, "", "gpt-6-astra"); blocked {
			t.Fatal("ticket gate must not block an unsupported account")
		}
		// Old tickets from before the account-scope fix must not be injected.
		account.CodexTickets = newTicketAccount(1, "gpt-6-astra").CodexTickets
		if _, ok := codexTicketInjection(account, "gpt-6-astra"); ok {
			t.Fatal("ticket must not be injected into an unsupported account")
		}
	}
}

func TestCodexTicketInjectionUsesSharedSamePlanFallback(t *testing.T) {
	withCodexTicketGate(t, "gpt-6-astra")
	auth.ResetCodexTicketSharedPoolForTest()
	t.Cleanup(auth.ResetCodexTicketSharedPoolForTest)
	now := time.Now()
	state := testTicketState(now, auth.CodexTicketTeamBlocks)
	source := &auth.Account{DBID: 201, PlanType: "self_serve_business_prolite"}
	auth.PublishCodexTicketToSharedPool(source, &auth.CodexTicket{
		Model: "gpt-6-astra", State: state, Length: len(state), IssuedAt: now,
		CapturedAt: now, ExpiresAt: now.Add(50 * time.Minute),
	})
	destination := &auth.Account{DBID: 202, PlanType: "self_serve_business_prolite"}
	if got, ok := codexTicketInjection(destination, "gpt-6-astra"); !ok || got != state {
		t.Fatalf("same-plan account must use shared ticket: got=%q ok=%v", got, ok)
	}
	ownState := testTicketState(now, auth.CodexTicketTeamBlocks)
	ownState = ownState[:len(ownState)-1] + "B"
	destination.CodexTickets = map[string]*auth.CodexTicket{
		"gpt-6-astra": {Model: "gpt-6-astra", State: ownState, Length: len(ownState), IssuedAt: now, ExpiresAt: now.Add(time.Hour)},
	}
	if got, ok := codexTicketInjection(destination, "gpt-6-astra"); !ok || got != ownState {
		t.Fatalf("own ticket must take priority: got=%q ok=%v", got, ok)
	}
	if _, ok := codexTicketInjection(&auth.Account{PlanType: "team"}, "gpt-6-astra"); ok {
		t.Fatal("different plan type must not use the shared ticket")
	}
}

func TestCodexTicketGateUsesSharedSamePlanFallback(t *testing.T) {
	withCodexTicketGate(t, "gpt-6-astra")
	auth.ResetCodexTicketSharedPoolForTest()
	t.Cleanup(auth.ResetCodexTicketSharedPoolForTest)
	now := time.Now()
	state := testTicketState(now, auth.CodexTicketTeamBlocks)
	source := &auth.Account{DBID: 203, PlanType: "self_serve_business_prolite"}
	auth.PublishCodexTicketToSharedPool(source, &auth.CodexTicket{
		Model: "gpt-6-astra", State: state, Length: len(state), IssuedAt: now,
		CapturedAt: now, ExpiresAt: now.Add(50 * time.Minute),
	})
	destination := &auth.Account{DBID: 204, PlanType: "self_serve_business_prolite"}
	if model, blocked := CodexTicketGateBlocked(context.Background(), destination, "", "gpt-6-astra"); blocked {
		t.Fatalf("shared ticket must satisfy FailClosed gate, blocked on %q", model)
	}
	if got, ok := codexTicketInjection(destination, "gpt-6-astra"); !ok || got != state {
		t.Fatalf("shared ticket injection = %q, ok=%v; want source ticket", got, ok)
	}
}

func TestFireCodexTicketHarvestTeam5x(t *testing.T) {
	withCodexTicketGate(t, "gpt-6-astra")
	// A small positive clock skew is within the existing tolerance.
	issuedAt := time.Now().Add(10 * time.Second)
	state := testTicketState(issuedAt, auth.CodexTicketTeamBlocks)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(codexTurnStateHeader, state)
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\"}\n\n"))
	}))
	t.Cleanup(server.Close)
	previousURL := codexTicketProbeURLForTest
	codexTicketProbeURLForTest = server.URL
	t.Cleanup(func() { codexTicketProbeURLForTest = previousURL })
	for _, plan := range []string{
		"team", "teamplus", "business", "enterprise", "team5x", "team-5x", "team_5x", "team 5x", " TEAM5X ",
		"self_serve_business_prolite", " SELF_SERVE_BUSINESS_PROLITE ",
	} {
		t.Run(plan, func(t *testing.T) {
			account := &auth.Account{AccessToken: "team-test-at", PlanType: plan}
			res := fireCodexTicketHarvest(context.Background(), account, "gpt-6-astra", server.URL, 5*time.Second)
			if res.Result != "success" || res.HTTPStatus != http.StatusOK || res.State != state || res.Err != nil {
				t.Fatalf("Team 5x harvest result=%s status=%d err=%v", res.Result, res.HTTPStatus, res.Err)
			}
			account.CodexTickets = map[string]*auth.CodexTicket{
				"gpt-6-astra": {State: res.State, Length: len(res.State), IssuedAt: issuedAt, ExpiresAt: time.Now().Add(time.Hour)},
			}
			if _, blocked := CodexTicketGateBlocked(context.Background(), account, "", "gpt-6-astra"); blocked {
				t.Fatal("fresh Team 5x ticket must pass the same plan-derived length gate")
			}
			if got, ok := codexTicketInjection(account, "gpt-6-astra"); !ok || got != state {
				t.Fatal("fresh Team 5x ticket must be injectable")
			}
		})
	}
}

func TestFireCodexTicketHarvestRejectsUnusableTickets(t *testing.T) {
	for _, tc := range []struct {
		name           string
		state          string
		plan           string
		configured     int
		expectedLength int
		validation     string
	}{
		{name: "missing", plan: "plus", expectedLength: 292, validation: "missing"},
		{name: "wrong length", state: "gAAAAAshort", plan: "plus", expectedLength: 292, validation: "shape"},
		{name: "malformed envelope", state: "gAAAAA" + strings.Repeat("a", 286), plan: "plus", expectedLength: 292, validation: "shape"},
		{name: "wrong plan length", state: testTicketState(time.Now(), auth.CodexTicketTeamBlocks), plan: "plus", expectedLength: 292, validation: "length"},
		{name: "teamplus receives personal length", state: testTicketState(time.Now(), auth.CodexTicketPersonalBlocks), plan: "teamplus", expectedLength: 332, validation: "length"},
		{name: "team5x receives personal length", state: testTicketState(time.Now(), auth.CodexTicketPersonalBlocks), plan: "team5x", expectedLength: 332, validation: "length"},
		{name: "self-serve Team 5x receives personal length", state: testTicketState(time.Now(), auth.CodexTicketPersonalBlocks), plan: "self_serve_business_prolite", expectedLength: 332, validation: "length"},
		{name: "explicit length override", state: testTicketState(time.Now(), auth.CodexTicketTeamBlocks), plan: "self_serve_business_prolite", configured: 400, expectedLength: 400, validation: "length"},
		{name: "expired", state: testTicketState(time.Now().Add(-2*time.Hour), auth.CodexTicketPersonalBlocks), plan: "plus", expectedLength: 292, validation: "time"},
		{name: "future issue time", state: testTicketState(time.Now().Add(time.Hour), auth.CodexTicketPersonalBlocks), plan: "plus", expectedLength: 292, validation: "time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withCodexTicketGate(t, "gpt-6-astra")
			if tc.configured > 0 {
				settings := auth.ConfiguredCodexTicketSettings()
				settings.TargetLength = tc.configured
				auth.SetConfiguredCodexTicketSettings(settings)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set(codexTurnStateHeader, tc.state)
				_, _ = w.Write([]byte("data: {\"type\":\"response.completed\"}\n\n"))
			}))
			t.Cleanup(server.Close)
			previousURL := codexTicketProbeURLForTest
			codexTicketProbeURLForTest = server.URL
			t.Cleanup(func() { codexTicketProbeURLForTest = previousURL })
			account := &auth.Account{AccessToken: "harvest-private-access-token", PlanType: tc.plan}
			res := fireCodexTicketHarvest(context.Background(), account, "gpt-6-astra", server.URL, 5*time.Second)
			if res.Result != "invalid_state" || res.HTTPStatus != http.StatusOK || res.State != "" || res.Err == nil {
				t.Fatalf("invalid ticket result=%s status=%d err=%v", res.Result, res.HTTPStatus, res.Err)
			}
			diagnostic := res.Err.Error()
			for _, want := range []string{
				"validation=" + tc.validation,
				fmt.Sprintf("plan_type=%q", tc.plan),
				fmt.Sprintf("actual_length=%d", len(tc.state)),
				fmt.Sprintf("expected_length=%d", tc.expectedLength),
				fmt.Sprintf("configured_length=%d", auth.ConfiguredCodexTicketSettings().TargetLength),
			} {
				if !strings.Contains(diagnostic, want) {
					t.Errorf("diagnostic missing %q: %s", want, diagnostic)
				}
			}
			if tc.validation == "time" {
				for _, field := range []string{"issued_at=", "expires_at=", "now=", "age_seconds="} {
					if !strings.Contains(diagnostic, field) {
						t.Errorf("time diagnostic missing %s", field)
					}
				}
			}
			if strings.Contains(diagnostic, account.AccessToken) || (tc.state != "" && strings.Contains(diagnostic, tc.state)) {
				t.Fatal("diagnostic must not contain access tokens or ticket contents")
			}
		})
	}
}
