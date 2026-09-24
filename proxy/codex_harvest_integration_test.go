package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/harvest"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestCodexHarvestWebsocketCookiePolicy(t *testing.T) {
	withCodexTicketGate(t, "gpt-6-astra")
	for _, tc := range []struct {
		name       string
		age        time.Duration
		wantCookie string
	}{
		{"first", 0, ""}, {"fresh", time.Minute, "ticket=previous"}, {"expired", 241 * time.Second, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := testTicketState(time.Now(), auth.CodexTicketPersonalBlocks)
			upgrader := websocket.Upgrader{}
			seen := make(chan http.Header, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- r.Header.Clone()
				conn, err := upgrader.Upgrade(w, r, http.Header{"Set-Cookie": []string{"ticket=next; Path=/"}})
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				_, body, err := conn.ReadMessage()
				if err != nil {
					t.Error(err)
					return
				}
				if gjson.GetBytes(body, "model").String() != "gpt-6-astra" || gjson.GetBytes(body, "type").String() != "response.create" {
					t.Error("采票帧模型或类型错误")
				}
				_ = conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "3.0"})
				_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"metadata": map[string]string{"x-codex-turn-state": state}}})
			}))
			defer upstream.Close()
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodConnect {
					t.Error("未通过采票代理建立 CONNECT")
					w.WriteHeader(400)
					return
				}
				remote, err := net.Dial("tcp", strings.TrimPrefix(upstream.URL, "http://"))
				if err != nil {
					t.Error(err)
					w.WriteHeader(502)
					return
				}
				local, buffer, err := w.(http.Hijacker).Hijack()
				if err != nil {
					remote.Close()
					t.Error(err)
					return
				}
				_, _ = buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
				_ = buffer.Flush()
				go func() { defer local.Close(); defer remote.Close(); _, _ = io.Copy(remote, local) }()
				go func() { defer local.Close(); defer remote.Close(); _, _ = io.Copy(local, remote) }()
			}))
			defer gateway.Close()
			previous := codexTicketProbeURLForTest
			codexTicketProbeURLForTest = "ws" + strings.TrimPrefix(upstream.URL, "http")
			defer func() { codexTicketProbeURLForTest = previous }()
			account := &auth.Account{AccessToken: "test-only", CustomHeaders: map[string]string{"Cookie": "custom=must-not-leak", codexTurnStateHeader: "old-state", "Session_id": "old-session"}}
			if tc.age > 0 {
				account.CodexTickets = map[string]*auth.CodexTicket{"gpt-6-astra": {Cookie: "ticket=previous", CookieCapturedAt: time.Now().Add(-tc.age)}}
			}
			result := fireCodexTicketHarvest(context.Background(), account, "gpt-6-astra", gateway.URL, 3*time.Second)
			if result.Result != "success" || result.State != state || result.Cookies.Header != "ticket=next" {
				t.Fatalf("WebSocket 采票失败: %s %v", result.Result, result.Err)
			}
			headers := <-seen
			if headers.Get("Cookie") != tc.wantCookie {
				t.Fatalf("Cookie 策略错误: %q", headers.Get("Cookie"))
			}
			if headers.Get(codexTurnStateHeader) != "" || headers.Get("Session_id") == "" || headers.Get("Session_id") == "old-session" {
				t.Fatal("采票复用了旧状态或旧会话")
			}
			if result.Binding.HarvestProxyURL != gateway.URL || result.Binding.HarvestSessionID != headers.Get("Session_id") {
				t.Fatal("未保存实际采票出口和会话")
			}
		})
	}
}

func newHarvestManagerTest(t *testing.T) (*CodexHarvestManager, *auth.Account) {
	t.Helper()
	withCodexTicketGate(t, "gpt-6-astra")
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "harvest.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	id, err := db.InsertAccountWithCredentials(t.Context(), "harvest-test", map[string]any{"upstream_type": "codex", "access_token": "test-only", "plan_type": "plus"}, "")
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	if err = store.LoadAccountByID(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	previousManager, previousSink := codexHarvester.Load(), codexTicketFeedback.Load()
	m := ConfigureCodexHarvest(db, store, t.TempDir())
	t.Cleanup(func() { codexHarvester.Store(previousManager); codexTicketFeedback.Store(previousSink) })
	scope := harvest.DefaultScope()
	scope.AccountPolicy = "prioritize_schedulable"
	if err = m.SaveScope(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	controls := harvest.CodexHarvestControls{Version: 1, Speed: harvest.CodexHarvestSpeedPresets()["burst"]}
	controls.Speed.MaxRequestsPerRound = 2
	if err = m.Controls.SaveControls(t.Context(), controls); err != nil {
		t.Fatal(err)
	}
	return m, store.FindByID(id)
}

func TestCodexHarvestRoundStandbyScopeAndManualBudget(t *testing.T) {
	m, account := newHarvestManagerTest(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		w.Header().Set(codexTurnStateHeader, testTicketState(time.Now().Add(time.Duration(n)*time.Second), auth.CodexTicketPersonalBlocks))
		w.Header().Add("Set-Cookie", fmt.Sprintf("ticket=round%d; Path=/", n))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"3.0\"}\n\ndata: {\"type\":\"response.completed\"}\n\n")
	}))
	defer server.Close()
	previous := codexTicketProbeURLForTest
	codexTicketProbeURLForTest = server.URL
	defer func() { codexTicketProbeURLForTest = previous }()
	settings := auth.ConfiguredCodexTicketSettings()
	settings.HarvestProxyURL = server.URL
	auth.SetConfiguredCodexTicketSettings(settings)
	m.RunRound(t.Context())
	m.RunRound(t.Context())
	m.RunRound(t.Context())
	if requests.Load() != 2 {
		t.Fatalf("补齐主备票后仍在采票: %d", requests.Load())
	}
	ticket := account.CodexTicketSnapshot("gpt-6-astra")
	if ticket == nil || ticket.Standby == nil || ticket.Standby.Cookie == ticket.Cookie {
		t.Fatal("备用票快照缺失")
	}
	row, err := m.DB.GetAccountByID(t.Context(), account.ID())
	if err != nil {
		t.Fatal(err)
	}
	persisted := auth.ParseCodexTicket("gpt-6-astra", row.Credentials[auth.CodexTicketCredentialKey("gpt-6-astra")])
	if persisted == nil || persisted.Standby == nil {
		t.Fatal("备用票未持久化")
	}
	job, err := m.StartManual(t.Context(), harvest.ManualRequest{AccountID: account.ID(), Models: []string{"gpt-6-astra"}, MaxAttempts: 2, NodeSwitchRule: "every_request", RateLimitCooldownSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		jobs := m.Jobs()
		if len(jobs) == 1 && !jobs[0].Running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	jobs := m.Jobs()
	if len(jobs) != 1 || jobs[0].Running || jobs[0].Attempts != 2 || jobs[0].TicketsStored != 2 {
		t.Fatalf("手动预算错误: %+v", jobs)
	}
	events, err := m.Repository.Events(t.Context(), account.ID(), job.ID, 0, 25)
	if err != nil || events.Total != 2 {
		t.Fatalf("手动事件缺失: %v", err)
	}
	scope, _ := m.Scope(t.Context())
	scope.SkippedAccountIDs = []int64{account.ID()}
	if err = m.SaveScope(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	if _, err = m.StartManual(t.Context(), job.Request); err == nil {
		t.Fatal("跳过账号仍能开始手动采票")
	}
	m.RunRound(t.Context())
	if requests.Load() != 4 {
		t.Fatal("跳过账号仍发出了请求")
	}
}

func TestCodexTicketFeedbackCompletionAndDurableRevocation(t *testing.T) {
	m, account := newHarvestManagerTest(t)
	now := time.Now()
	old := testTicketState(now, auth.CodexTicketPersonalBlocks)
	next := testTicketState(now.Add(time.Second), auth.CodexTicketPersonalBlocks)
	cookies := codexTicketCookies{Header: "ticket=cookie", CapturedAt: now.Add(-time.Minute)}
	if !publishCodexTicket(m.DB, m.Store, account, "gpt-6-astra", old, codexTicketIssuedAt(old), cookies, &auth.CodexTicket{HarvestProxyURL: "http://127.0.0.1:3101", HarvestSessionID: "fresh-session"}) {
		t.Fatal("测试票据发布失败")
	}
	ctx, _, _ := prepareCodexTurnStateInjection(t.Context(), account, []byte(`{"model":"gpt-6-astra"}`), nil, true)
	ObserveCodexTicketFeedbackFrame(ctx, []byte(fmt.Sprintf(`{"type":"response.metadata","metadata":{"x-codex-turn-state":%q}}`, next)))
	if account.CodexTicketSnapshot("gpt-6-astra").State != old {
		t.Fatal("成功终态前已续票")
	}
	ObserveCodexTicketFeedbackFrame(ctx, []byte(`{"type":"response.completed"}`))
	current := account.CodexTicketSnapshot("gpt-6-astra")
	if current.State != next || !current.CookieCapturedAt.Equal(cookies.CapturedAt) || current.HarvestSessionID != "fresh-session" || current.Standby == nil {
		t.Fatal("续票破坏了 Cookie 时间、会话或备用票")
	}
	observeCodexTicketFeedback(account, next, testTicketState(now, 11), 200, cookies, current)
	row, err := m.DB.GetAccountByID(t.Context(), account.ID())
	if err != nil {
		t.Fatal(err)
	}
	saved := auth.ParseCodexTicket("gpt-6-astra", row.Credentials[auth.CodexTicketCredentialKey("gpt-6-astra")])
	if saved == nil || saved.State != old || saved.Standby != nil {
		t.Fatal("非目标票据未触发持久化备用票提升")
	}
}

func TestCodexTicketFeedbackCompletionRevokesSharedOwner(t *testing.T) {
	m, owner := newHarvestManagerTest(t)
	id, err := m.DB.InsertAccountWithCredentials(t.Context(), "borrower-test", map[string]any{"upstream_type": "codex", "access_token": "test-only", "plan_type": "plus"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Store.LoadAccountByID(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	borrower := m.Store.FindByID(id)
	now := time.Now()
	state := testTicketState(now, auth.CodexTicketPersonalBlocks)
	cookies := codexTicketCookies{Header: "ticket=shared", CapturedAt: now}
	if !publishCodexTicket(m.DB, m.Store, owner, "gpt-6-astra", state, codexTicketIssuedAt(state), cookies) {
		t.Fatal("共享票发布失败")
	}
	shared := borrower.CodexTicketWithShared(now, len(state), "gpt-6-astra")
	if shared == nil || shared.State != state {
		t.Fatal("测试账号没有借用共享票")
	}
	observeCodexTicketFeedback(borrower, state, "", http.StatusForbidden, cookies, shared)
	if ticket := owner.CodexTicketSnapshot("gpt-6-astra"); ticket == nil || !ticket.Revoked {
		t.Fatal("共享票来源账号仍持有被拒绝的票")
	}
	// 模拟重启清空共享池后重载来源账号，旧凭据不能重新提供被撤销的票。
	auth.ResetCodexTicketSharedPoolForTest()
	if err = m.Store.LoadAccountByID(t.Context(), owner.ID()); err != nil {
		t.Fatal(err)
	}
	if ticket := borrower.CodexTicketWithShared(time.Now(), len(state), "gpt-6-astra"); ticket != nil {
		t.Fatal("账号重载重新发布了已撤销的共享票")
	}
}

func TestCodexHarvestManualCancellation(t *testing.T) {
	m, account := newHarvestManagerTest(t)
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		r.Body.Close()
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	previous := codexTicketProbeURLForTest
	codexTicketProbeURLForTest = server.URL
	defer func() { codexTicketProbeURLForTest = previous }()
	settings := auth.ConfiguredCodexTicketSettings()
	settings.HarvestProxyURL = server.URL
	auth.SetConfiguredCodexTicketSettings(settings)
	job, err := m.StartManual(t.Context(), harvest.ManualRequest{AccountID: account.ID(), Models: []string{"gpt-6-astra"}, MaxAttempts: 10, NodeSwitchRule: "never", RateLimitCooldownSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("手动任务没有发出请求")
	}
	if !m.CancelJob(job.ID) {
		t.Fatal("任务无法停止")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		jobs := m.Jobs()
		if len(jobs) == 1 && !jobs[0].Running {
			if !jobs[0].Cancelled || jobs[0].Attempts != 1 {
				t.Fatalf("停止后的状态错误: %+v", jobs)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("取消没有中断在途采票请求")
}

func TestCodexTicketHTTPFeedbackWaitsForCompletion(t *testing.T) {
	m, account := newHarvestManagerTest(t)
	now := time.Now()
	old := testTicketState(now, 10)
	next := testTicketState(now.Add(time.Second), 10)
	if !publishCodexTicket(m.DB, m.Store, account, "gpt-6-astra", old, codexTicketIssuedAt(old), codexTicketCookies{Header: "ticket=old", CapturedAt: now}) {
		t.Fatal("测试票据发布失败")
	}
	for _, kind := range []string{"response.failed", "response.completed"} {
		ctx, _, _ := prepareCodexTurnStateInjection(t.Context(), account, []byte(`{"model":"gpt-6-astra"}`), nil, false)
		payload := fmt.Sprintf("data: {\"type\":%q}\n\n", kind)
		response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}, http.CanonicalHeaderKey(codexTurnStateHeader): []string{next}, "Set-Cookie": []string{"ticket=updated; Path=/"}}, Body: io.NopCloser(strings.NewReader(payload))}
		observeCodexTicketHTTPResponse(ctx, response)
		if account.CodexTicketSnapshot("gpt-6-astra").State != old {
			t.Fatal("仅收到 HTTP 头就采纳了新票")
		}
		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil || string(body) != payload {
			t.Fatal("反馈观测修改了响应体")
		}
		current := account.CodexTicketSnapshot("gpt-6-astra")
		if kind == "response.failed" && current.State != old {
			t.Fatal("失败终态更新了票据")
		}
		if kind == "response.completed" && (current.State != next || current.Cookie != "ticket=updated") {
			t.Fatal("成功终态未更新票据")
		}
	}
}
