package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/harvest"
)

func TestHarvestSchedulingPauseAdmission(t *testing.T) {
	for _, tc := range []struct {
		name                                    string
		owned, paused, disabled, cooldown, want bool
	}{
		{name: "ordinary enabled account", want: true},
		{name: "manual dispatch pause", paused: true},
		{name: "harvest-owned dispatch pause", owned: true, paused: true, want: true},
		{name: "hard disabled never exempt", owned: true, disabled: true},
		{name: "hard disabled with owned pause", owned: true, paused: true, disabled: true},
		{name: "cooldown still enforced", owned: true, paused: true, cooldown: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
			t.Cleanup(store.Stop)
			a := &auth.Account{DBID: 1, AccountID: "offline", AccessToken: "offline-token", PlanType: "plus", Status: auth.StatusReady}
			store.AddAccount(a)
			store.ApplyHarvestScheduling(a.ID(), tc.owned, false, nil)
			store.ApplyAccountEnabled(a.ID(), !tc.paused)
			if tc.disabled {
				atomic.StoreInt32(&a.Disabled, 1)
			}
			if tc.cooldown {
				a.Mu().Lock()
				a.Status, a.CooldownUtil = auth.StatusCooldown, time.Now().Add(time.Minute)
				a.Mu().Unlock()
			}
			if got := a.CanHarvestCodexTicket(time.Now()); got != tc.want {
				t.Fatalf("harvest admission = %v, want %v (owned=%v paused=%v disabled=%v)", got, tc.want, tc.owned, tc.paused, tc.disabled)
			}
		})
	}
}

func TestHarvestSchedulingPausedAccountHarvestsAndRecovers(t *testing.T) {
	m, account := newHarvestManagerTest(t)
	scope := harvest.DefaultScope()
	scope.AccountPolicy = "schedulable_only"
	if err := m.SaveScope(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	v := harvest.DefaultGatewaySettings()
	v.TicketPoolScheduling = true
	v.Revision, v.RouteRevision = "scheduling-recovery", "scheduling-recovery-route"
	seedGatewayRegressionSettings(t, m, v)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set(codexTurnStateHeader, testTicketState(time.Now(), auth.CodexTicketPersonalBlocks))
		w.Header().Add("Set-Cookie", "ticket=offline-recovered; Path=/")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-6-astra\"}}\n\n")
	}))
	defer server.Close()
	previousURL := codexTicketProbeURLForTest
	codexTicketProbeURLForTest = server.URL
	t.Cleanup(func() { codexTicketProbeURLForTest = previousURL })
	settings := auth.ConfiguredCodexTicketSettings()
	settings.TargetLength, settings.HarvestProxyURL = 292, server.URL
	auth.SetConfiguredCodexTicketSettings(settings)

	m.SyncHarvestScheduling(t.Context())
	if !account.HarvestOwnsPause() || atomic.LoadInt32(&account.DispatchPaused) != 1 {
		t.Fatal("fixture did not pause the ticketless account")
	}
	if !harvestAccountEligible(account, scope, true) {
		t.Fatal("the scheduler's own pause prevents ticket replenishment")
	}
	m.RunRound(t.Context())
	if calls.Load() != 1 || account.CodexTicketForModel(gatewayModel, time.Now(), 292, "sse") == nil {
		t.Fatalf("paused account did not acquire a ticket: calls=%d", calls.Load())
	}
	m.SyncHarvestScheduling(t.Context())
	if account.HarvestOwnsPause() || atomic.LoadInt32(&account.DispatchPaused) != 0 {
		t.Fatal("a replenished ticket failed to restore scheduling")
	}
	owned, err := m.Repository.HarvestSchedulingState(t.Context(), account.ID())
	if err != nil || owned.Mode != "" {
		t.Fatalf("scheduling ownership was not cleared: %+v %v", owned, err)
	}
	// Reload the account to check that enabled=true was restored durably as well.
	if err := m.Store.LoadAccountByID(t.Context(), account.ID()); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&m.Store.FindByID(account.ID()).DispatchPaused) != 0 {
		t.Fatal("restored scheduling did not survive account reload")
	}
}
