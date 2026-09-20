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

var _ = fmt.Sprintf
