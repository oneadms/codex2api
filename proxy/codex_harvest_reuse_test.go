package proxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/harvest"
)

func TestCodexHarvestReuseStoresOnlyTargetOwnedState(t *testing.T) {
	for _, same := range []bool{false, true} {
		t.Run(fmt.Sprintf("same_state_%v", same), func(t *testing.T) {
			withCodexTicketGate(t, "gpt-6-astra")
			settings := auth.ConfiguredCodexTicketSettings()
			settings.TargetLength = 780
			auth.SetConfiguredCodexTicketSettings(settings)
			sourceState := testTicketState(time.Now().Add(-time.Second), 33)
			targetState := testTicketState(time.Now(), 33)
			claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix())))
			cookies := "__cflb=a; __oailb=h." + claims + ".s"
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := requests.Add(1)
				want := sourceState
				if n > 1 {
					want = targetState
				}
				if r.Header.Get(codexTurnStateHeader) != want || r.Header.Get("Cookie") != cookies || r.Header.Get("Authorization") != "Bearer target-only" {
					t.Error("复用未使用目标身份和完整材料")
				}
				state := targetState
				if same {
					state = sourceState
				}
				w.Header().Set(codexTurnStateHeader, state)
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
			}))
			defer server.Close()
			old := codexTicketProbeURLForTest
			codexTicketProbeURLForTest = server.URL
			defer func() { codexTicketProbeURLForTest = old }()
			seed := &auth.CodexTicket{State: sourceState, Cookie: cookies, SourceAccountID: 10, CookieCapturedAt: time.Now()}
			ctx := context.WithValue(t.Context(), harvestSeedKey{}, seed)
			result := fireCodexTicketHarvest(ctx, &auth.Account{AccessToken: "target-only"}, "gpt-6-astra", server.URL, 3*time.Second)
			if same {
				if result.Result == "success" || result.State != "" {
					t.Fatal("来源原票被当成目标票")
				}
				return
			}
			if result.Result != "success" || result.State != targetState || requests.Load() != 2 || result.Binding.Acquisition != "reuse" || result.Binding.SourceAccountID != 10 {
				t.Fatalf("目标复用验收失败: %s %v", result.Result, result.Err)
			}
		})
	}
}

func TestCodexManualHarvestDoesNotPrepareOrBorrowSources(t *testing.T) {
	for _, tc := range []struct {
		name      string
		budget    int
		skipped   bool
		wantReuse bool
	}{
		{"manual remains self collection", 4, false, false},
		{"skip source", 4, true, false},
		{"insufficient preparation budget", 3, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, target := newHarvestManagerTest(t)
			id, err := m.DB.InsertAccountWithCredentials(t.Context(), "source", map[string]any{"upstream_type": "codex", "access_token": "source-only", "plan_type": "plus"}, "")
			if err != nil {
				t.Fatal(err)
			}
			if err = m.Store.LoadAccountByID(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			if tc.skipped {
				scope, _ := m.Scope(t.Context())
				scope.SkippedAccountIDs = []int64{id}
				if err = m.SaveScope(t.Context(), scope); err != nil {
					t.Fatal(err)
				}
			}
			sourceState := testTicketState(time.Now().Add(-time.Second), 33)
			targetState := testTicketState(time.Now(), 33)
			claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, time.Now().Add(time.Hour).Unix())))
			var sourceRequests, targetRequests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				state := targetState
				if r.Header.Get("Authorization") == "Bearer source-only" {
					sourceRequests.Add(1)
					state = sourceState
				} else {
					n := targetRequests.Add(1)
					if n == 1 && tc.wantReuse && r.Header.Get(codexTurnStateHeader) != sourceState {
						t.Error("目标没有借用来源启动材料")
					}
					if n == 1 && !tc.wantReuse && r.Header.Get(codexTurnStateHeader) != "" {
						t.Error("不合格来源仍被复用")
					}
				}
				w.Header().Set(codexTurnStateHeader, state)
				w.Header().Add("Set-Cookie", "__cflb=a; Path=/")
				w.Header().Add("Set-Cookie", "__oailb=h."+claims+".s; Path=/")
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\"}\n\n")
			}))
			defer server.Close()
			old := codexTicketProbeURLForTest
			codexTicketProbeURLForTest = server.URL
			defer func() { codexTicketProbeURLForTest = old }()
			settings := auth.ConfiguredCodexTicketSettings()
			settings.TargetLength, settings.HarvestProxyURL = 780, server.URL
			auth.SetConfiguredCodexTicketSettings(settings)
			controls := harvest.CodexHarvestControls{Transport: "sse", Speed: harvest.CodexHarvestSpeed{AttemptTimeoutSeconds: 3, CooldownSeconds: 1}}
			budget := tc.budget
			event := m.attempt(t.Context(), target, "gpt-6-astra", server.URL, controls, &budget, map[string]bool{}, nil, "manual", "", 1)
			ticket := target.CodexTicketForModel("gpt-6-astra", time.Now(), 780, "sse")
			if event.Result != "success" || ticket == nil || ticket.State != targetState {
				t.Fatalf("目标验收失败: %+v", event)
			}
			if (ticket.Acquisition == "reuse") != tc.wantReuse {
				t.Fatal("采集来源标记不符")
			}
			if tc.wantReuse && (ticket.SourceAccountID != id || m.Store.FindByID(id).CodexTicketSnapshot("gpt-6-astra").State != sourceState) {
				t.Fatal("来源与目标票据未独立保存")
			}
			want := int64(0)
			if tc.wantReuse {
				want = 2
			}
			if sourceRequests.Load() != want || targetRequests.Load() != 2 || budget != tc.budget-int(want)-2 {
				t.Fatal("来源补票绕过范围或共享预算")
			}
		})
	}
}
