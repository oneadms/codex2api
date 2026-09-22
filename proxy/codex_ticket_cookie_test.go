package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

func TestCodexTicketCookieCaptureScopeAndExpiry(t *testing.T) {
	endpoint, _ := url.Parse(CodexBaseURL + "/responses")
	response := &http.Response{Header: make(http.Header)}
	for _, value := range []string{
		"ticket=new; Path=/backend-api/codex; Domain=.chatgpt.com; Secure; HttpOnly; SameSite=None; Max-Age=90",
		"wrong-domain=x; Domain=example.com; Path=/",
		"wrong-path=x; Path=/unrelated",
		"expired=x; Path=/; Expires=Wed, 01 Jan 2020 00:00:00 GMT",
		"deleted=x; Path=/; Max-Age=0",
	} {
		response.Header.Add("Set-Cookie", value)
	}
	before := time.Now()
	got := captureCodexTicketCookies(endpoint, codexTicketCookies{Header: "ticket=old; kept=ok; deleted=x"}, response)
	request := &http.Request{Header: http.Header{"Cookie": {got.Header}}}
	cookies := request.Cookies()
	if len(cookies) != 2 {
		t.Fatalf("only matching live cookies should survive, got %d", len(cookies))
	}
	for name, value := range map[string]string{"ticket": "new", "kept": "ok"} {
		cookie, err := request.Cookie(name)
		if err != nil || cookie.Value != value {
			t.Fatalf("cookie %s was not merged correctly", name)
		}
	}
	if got.ExpiresAt.Before(before.Add(90*time.Second)) || got.ExpiresAt.After(time.Now().Add(90*time.Second)) {
		t.Fatal("Max-Age must bound the cached ticket")
	}
	if strings.Contains(got.Header, "Path=") || strings.Contains(got.Header, "HttpOnly") {
		t.Fatal("Set-Cookie attributes must never enter the Cookie request header")
	}
	response.Header = http.Header{"Set-Cookie": {"ticket=; Path=/; Max-Age=0"}}
	if got := captureCodexTicketCookies(endpoint, codexTicketCookies{Header: "ticket=old"}, response); got.Header != "" {
		t.Fatal("deleting the only cookie must invalidate the pair")
	}
}

func TestCodexTicketHarvestRejectsMissingCookie(t *testing.T) {
	withCodexTicketGate(t, "gpt-5.5")
	state := testTicketState(time.Now(), auth.CodexTicketPersonalBlocks)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(codexTurnStateHeader, state)
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\"}\n\n"))
	}))
	t.Cleanup(server.Close)
	previousURL := codexTicketProbeURLForTest
	codexTicketProbeURLForTest = server.URL
	t.Cleanup(func() { codexTicketProbeURLForTest = previousURL })
	result := fireCodexTicketHarvest(context.Background(), &auth.Account{AccessToken: "test-token"}, "gpt-5.5", server.URL, time.Second)
	if result.Result != "invalid_state" || result.Err == nil || !strings.Contains(result.Err.Error(), "validation=cookie") || result.State != "" {
		t.Fatalf("missing cookie was accepted: result=%s err=%v", result.Result, result.Err)
	}
}

func TestCodexTicketCookieInjectionSnapshotAndManualPriority(t *testing.T) {
	withCodexTicketGate(t, "gpt-5.5")
	for _, ws := range []bool{false, true} {
		account := newTicketAccount(710, "gpt-5.5")
		client := http.Header{"Cookie": {"client=private"}}
		ctx, body, headers := prepareCodexTurnStateInjection(context.Background(), account, []byte(`{"model":"gpt-5.5"}`), client, ws)
		if headers.Get("Cookie") != "ticket=local" || client.Get("Cookie") != "client=private" {
			t.Fatal("ticket cookie must override a cloned header only")
		}
		account.CodexTickets["gpt-5.5"].Cookie = "ticket=next"
		final := http.Header{"Cookie": {"custom=private"}}
		applyCodexTurnStateInjectionHeader(ctx, final)
		if final.Get("Cookie") != "ticket=local" {
			t.Fatal("in-flight request must retain its selected cookie snapshot")
		}
		if ws && gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String() != final.Get(codexTurnStateHeader) {
			t.Fatal("WebSocket frame and handshake must carry the same ticket")
		}
		account.CodexTurnState = "manual-state"
		_, _, manual := prepareCodexTurnStateInjection(ctx, account, []byte(`{"model":"gpt-5.5"}`), client, ws)
		if manual.Get("Cookie") != "client=private" || manual.Get(codexTurnStateHeader) != "manual-state" {
			t.Fatal("manual injection must not inherit an automatic ticket cookie")
		}
	}
	account := newTicketAccount(711, "gpt-5.5")
	account.CodexTickets["gpt-5.5"].Cookie = ""
	if _, blocked := CodexTicketGateBlocked(context.Background(), account, "", "gpt-5.5"); !blocked {
		t.Fatal("legacy ticket without cookies must fail the gate")
	}
}

func TestCodexTicketHTTPForwardsMatchingCookie(t *testing.T) {
	withCodexTicketGate(t, "gpt-5.5")
	account := newTicketAccount(712, "gpt-5.5")
	account.AccessToken = "test-token"
	account.CustomHeaders = map[string]string{"Cookie": "custom=wrong"}
	received := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Clone()
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	previous := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(previous) })
	SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "ticket-test"})
	clientPool.Delete(fmt.Sprintf("resin|%d", account.ID()))
	t.Cleanup(func() { clientPool.Delete(fmt.Sprintf("resin|%d", account.ID())) })
	response, err := ExecuteRequest(context.Background(), account, []byte(`{"model":"gpt-5.5","input":"hi"}`), "", "", "test-key", nil, http.Header{"Cookie": {"client=wrong"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	headers := <-received
	if headers.Get("Cookie") != "ticket=local" || headers.Get(codexTurnStateHeader) != account.CodexTickets["gpt-5.5"].State {
		t.Fatal("final outbound HTTP request did not carry the matching pair")
	}
}

func TestCodexTicketFeedbackPersistsCookiePair(t *testing.T) {
	withCodexTicketGate(t, "gpt-5.5")
	auth.ResetCodexTicketSharedPoolForTest()
	t.Cleanup(auth.ResetCodexTicketSharedPoolForTest)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "ticket-cookie.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	id, err := db.InsertAccountWithCredentials(context.Background(), "cookie-test", map[string]any{"access_token": "test-token", "plan_type": "plus"}, "")
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	if err := store.LoadAccountByID(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	account := store.FindByID(id)
	now := time.Now().Truncate(time.Second)
	oldState := testTicketState(now.Add(-time.Second), auth.CodexTicketPersonalBlocks)
	oldCookie := codexTicketCookies{Header: "ticket=old"}
	if !publishCodexTicket(db, store, account, "gpt-5.5", oldState, now.Add(-time.Second), oldCookie) {
		t.Fatal("initial pair was not published")
	}
	previousSink := codexTicketFeedback.Load()
	t.Cleanup(func() { codexTicketFeedback.Store(previousSink) })
	registerCodexTicketFeedback(db, store)
	ctx, _, _ := prepareCodexTurnStateInjection(context.Background(), account, []byte(`{"model":"gpt-5.5"}`), nil, false)
	ctx = context.WithValue(ctx, upstreamTraceContextKey{}, &upstreamTraceAudit{store: store})
	state := testTicketState(now, auth.CodexTicketPersonalBlocks)
	record := beginUpstreamTrace(ctx, account, "", false)
	record(&http.Response{StatusCode: http.StatusOK, Header: http.Header{
		codexTurnStateHeader: {state}, "Set-Cookie": {"ticket=new; Path=/; Max-Age=120; Secure; HttpOnly"},
	}})
	row, err := db.GetAccountByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	stored := auth.ParseCodexTicket("gpt-5.5", row.Credentials[auth.CodexTicketCredentialKey("gpt-5.5")])
	if stored == nil || stored.State != state || stored.Cookie != "ticket=new" || stored.CookieExpiresAt.IsZero() || !stored.Valid(time.Now(), 292) {
		t.Fatal("feedback pair was not persisted with its cookie expiry")
	}
	encoded, _ := json.Marshal(account.CodexTicketStatuses([]string{"gpt-5.5"}, time.Now(), 292))
	if strings.Contains(string(encoded), "ticket=new") {
		t.Fatal("management status must not expose cookies")
	}
	newer := testTicketState(now.Add(time.Second), auth.CodexTicketPersonalBlocks)
	observeCodexTicketFeedback(account, state, newer, http.StatusOK, codexTicketCookies{})
	if ticket := account.CodexTicketForModel("gpt-5.5", time.Now(), 292); ticket == nil || ticket.State != state {
		t.Fatal("feedback without a cookie must not replace a usable pair")
	}
	observeCodexTicketFeedback(account, state, newer, http.StatusUnauthorized, codexTicketCookies{Header: "ticket=rejected"})
	if ticket := account.CodexTicketForModel("gpt-5.5", time.Now(), 292); ticket != nil && ticket.State == newer {
		t.Fatal("authentication failure must not publish the response ticket")
	}
}

func TestCodexTicketGateUsesPreparedPair(t *testing.T) {
	withCodexTicketGate(t, "gpt-5.5")
	auth.ResetCodexTicketSharedPoolForTest()
	t.Cleanup(auth.ResetCodexTicketSharedPoolForTest)
	account := &auth.Account{DBID: 713, PlanType: "personal"}
	ctx, _, _ := prepareCodexTurnStateInjection(context.Background(), account, []byte(`{"model":"gpt-5.5"}`), nil, false)
	account.CodexTickets = newTicketAccount(713, "gpt-5.5").CodexTickets
	if _, blocked := CodexTicketGateBlocked(ctx, account, "", "gpt-5.5"); !blocked {
		t.Fatal("a ticket published after preparation must not allow an unpaired request")
	}
	ctx, _, headers := prepareCodexTurnStateInjection(ctx, account, []byte(`{"model":"gpt-5.5"}`), nil, false)
	if _, blocked := CodexTicketGateBlocked(ctx, account, "", "gpt-5.5"); blocked || headers.Get("Cookie") != "ticket=local" {
		t.Fatal("the next prepared attempt must use the newly published pair")
	}
}
