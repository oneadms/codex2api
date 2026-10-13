package proxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func TestCodexHarvest780Continuation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		budget int
		change string
		want   bool
	}{
		{"verified", 2, "", true}, {"reissued", 2, "state", false}, {"route changed", 2, "cookie", false},
		{"partial pair", 2, "partial", false}, {"incomplete", 2, "incomplete", false},
		{"wrong model", 2, "model", false}, {"rate limited", 2, "429", false}, {"insufficient budget", 1, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withCodexTicketGate(t, "gpt-6-astra")
			settings := auth.ConfiguredCodexTicketSettings()
			settings.TargetLength = 780
			auth.SetConfiguredCodexTicketSettings(settings)
			state := testTicketState(time.Now(), 33)
			claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"host":"any-gateway.example"}`, time.Now().Add(time.Hour).Unix())))
			cookie := "__cflb=route; __oailb=h." + claims + ".s"
			var requests atomic.Int64
			var session atomic.Value
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := requests.Add(1)
				if n == 1 {
					session.Store(r.Header.Get("session_id"))
					if r.Header.Get("Cookie") != "" || r.Header.Get(codexTurnStateHeader) != "" {
						t.Error("首轮泄漏了旧材料")
					}
				} else {
					if r.Header.Get("session_id") != session.Load() || r.Header.Get("Cookie") != cookie || r.Header.Get(codexTurnStateHeader) != state {
						t.Error("续接未回带同会话完整材料")
					}
					if tc.change == "429" {
						w.WriteHeader(429)
						return
					}
				}
				issued := state
				if n == 2 && tc.change == "state" {
					issued = testTicketState(time.Now().Add(-time.Second), 33)
				}
				w.Header().Set(codexTurnStateHeader, issued)
				if n == 1 || tc.change == "cookie" || tc.change == "partial" {
					value := "route"
					if n == 2 {
						value = "changed"
					}
					w.Header().Add("Set-Cookie", "__cflb="+value+"; Path=/")
					if n == 1 || tc.change != "partial" {
						w.Header().Add("Set-Cookie", "__oailb=h."+claims+".s; Path=/")
					}
				}
				if n == 2 && tc.change == "incomplete" {
					_, _ = io.WriteString(w, "data: {\"type\":\"response.created\"}\n\n")
					return
				}
				model := "gpt-6-astra"
				if tc.change == "model" {
					model = "other-model"
				}
				_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"model\":%q}}\n\n", model)
			}))
			defer server.Close()
			old := codexTicketProbeURLForTest
			codexTicketProbeURLForTest = server.URL
			defer func() { codexTicketProbeURLForTest = old }()
			var used atomic.Int64
			ctx := context.WithValue(t.Context(), harvestReserveKey{}, func(context.Context) error {
				if !reserveHarvestRequest(&used, tc.budget) {
					return fmt.Errorf("预算已用完")
				}
				return nil
			})
			account := &auth.Account{AccessToken: "test-only", CustomHeaders: map[string]string{"Cookie": "old=private", codexTurnStateHeader: "old-state"}}
			result := fireCodexTicketHarvest(ctx, account, "gpt-6-astra", server.URL, 3*time.Second)
			if (result.Result == "success") != tc.want {
				t.Fatalf("result=%s err=%v", result.Result, result.Err)
			}
			if requests.Load() > int64(tc.budget) || used.Load() != requests.Load() {
				t.Fatal("请求预算失配")
			}
			if tc.want && (result.Binding == nil || !result.Binding.ProbeVerified || result.Binding.Transport != "sse" || result.Cookies.Header != cookie) {
				t.Fatal("未保存验收材料")
			}
			if !tc.want && result.State != "" {
				t.Fatal("失败候选泄漏为可用票")
			}
		})
	}
}

func TestCodexHarvestParallelBudget(t *testing.T) {
	var used atomic.Int64
	var admitted atomic.Int64
	var workers sync.WaitGroup
	for range 100 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if reserveHarvestRequest(&used, 7) {
				admitted.Add(1)
			}
		}()
	}
	workers.Wait()
	if used.Load() != 7 || admitted.Load() != 7 {
		t.Fatal("并行预算超限或丢失")
	}
}

func TestCodexHarvest780PersistenceAndProtocol(t *testing.T) {
	m, account := newHarvestManagerTest(t)
	settings := auth.ConfiguredCodexTicketSettings()
	settings.TargetLength = 780
	auth.SetConfiguredCodexTicketSettings(settings)
	claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix())))
	cookies := codexTicketCookies{Header: "__cflb=a; __oailb=h." + claims + ".s", CapturedAt: time.Now()}
	state := testTicketState(time.Now(), 33)
	binding := &auth.CodexTicket{ProbeVerified: true, Transport: "sse", HarvestProxyURL: "http://127.0.0.1:3101", HarvestSessionID: "verified-session"}
	if !publishCodexTicket(m.DB, m.Store, account, "gpt-6-astra", state, codexTicketIssuedAt(state), cookies, binding) {
		t.Fatal("已验收票未入库")
	}
	if err := m.Store.LoadAccountByID(t.Context(), account.ID()); err != nil {
		t.Fatal(err)
	}
	account = m.Store.FindByID(account.ID())
	if ticket := account.CodexTicketForModel("gpt-6-astra", time.Now(), 780); ticket == nil || !ticket.ProbeVerified {
		t.Fatal("重载丢失验收标记")
	}
	ctx, _, headers := prepareCodexTurnStateInjection(t.Context(), account, []byte(`{"model":"gpt-6-astra"}`), nil, false)
	if headers.Get(codexTurnStateHeader) != state {
		t.Fatal("SSE 票未正确注入")
	}
	ws, _, _ := prepareCodexTurnStateInjection(t.Context(), account, []byte(`{"model":"gpt-6-astra"}`), nil, true)
	if _, blocked := CodexTicketGateBlocked(ws, account, "", "gpt-6-astra"); !blocked {
		t.Fatal("SSE 票错误地用于 WebSocket")
	}
	selected := codexTicketFromContext(ctx)
	observeCodexTicketFeedback(account, state, testTicketState(time.Now().Add(-time.Second), 33), 200, cookies, selected)
	if account.CodexTicketForModel("gpt-6-astra", time.Now(), 780) != nil {
		t.Fatal("业务重签后仍复用了旧验收标记")
	}
	if strings.Contains(fmt.Sprint(account.CodexTicketStatuses([]string{"gpt-6-astra"}, time.Now(), 780)), state) {
		t.Fatal("状态响应泄漏了票体")
	}
}
