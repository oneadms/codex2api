package proxy

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/harvest"
)

func TestCodexHarvestPoolParallelPersistsSingleVerifiedWinner(t *testing.T) {
	m, account := newHarvestManagerTest(t)
	var firstShots, requests atomic.Int32
	ready := make(chan struct{})
	fixture := harvestSuccessFixture(testTicketState(time.Now(), 33), 780)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get(codexTurnStateHeader) == "" {
			if firstShots.Add(1) == 2 {
				close(ready)
			}
			select {
			case <-ready:
			case <-r.Context().Done():
				return
			}
		}
		fixture(w, r)
	})
	first := httptest.NewServer(handler)
	defer first.Close()
	second := httptest.NewServer(handler)
	defer second.Close()
	configureHarvestPoolTest(t, m, 780, []string{gatewayModel}, first.URL, second.URL)
	job := waitHarvestJobTest(t, m, harvest.ManualRequest{AccountID: account.ID(), Models: []string{gatewayModel}, CollectLanes: 128, MaxAttempts: 4, RateLimitCooldownSeconds: 1, NodeSwitchRule: "every_request"})
	if firstShots.Load() != 2 || requests.Load() > 4 || job.Attempts > 4 || job.TicketsStored != 1 || job.LastEvent.Result != "success" {
		t.Fatalf("coordinator did not retain one winner: calls=%d first=%d job=%+v", requests.Load(), firstShots.Load(), job)
	}
	row, err := m.DB.GetAccountByID(t.Context(), account.ID())
	if err != nil {
		t.Fatal(err)
	}
	ticket := auth.ParseCodexTicket(gatewayModel, row.Credentials[auth.CodexTicketCredentialKey(gatewayModel)])
	if ticket == nil || !ticket.ProbeVerified || ticket.Standby != nil || ticket.ManagedNode || ticket.HarvestNodeID != "" || ticket.Identity != account.CodexTicketIdentitySnapshot() {
		t.Fatal("winner bundle was not persisted atomically")
	}
	if ticket.HarvestProxyURL != first.URL && ticket.HarvestProxyURL != second.URL {
		t.Fatal("winner lost its concrete proxy")
	}
}

func TestCodexHarvestPoolRemovalCancelsContinuationAndPublication(t *testing.T) {
	for _, lanes := range []int{1, 2} {
		for _, removeAt := range []int32{1, 2} {
			m, account := newHarvestManagerTest(t)
			var calls atomic.Int32
			var id int64
			fixture := harvestSuccessFixture(testTicketState(time.Now(), 33), 780)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == removeAt {
					enabled := false
					if err := m.DB.UpdateProxy(r.Context(), id, nil, nil, &enabled); err != nil {
						t.Error(err)
					}
				}
				fixture(w, r)
			}))
			id = configureHarvestPoolTest(t, m, 780, []string{gatewayModel}, server.URL)[0]
			job := waitHarvestJobTest(t, m, harvest.ManualRequest{AccountID: account.ID(), Models: []string{gatewayModel}, CollectLanes: lanes, MaxAttempts: 6, RateLimitCooldownSeconds: 1, NodeSwitchRule: "every_request"})
			server.Close()
			if calls.Load() != removeAt || job.TicketsStored != 0 || account.CodexTicketSnapshot(gatewayModel) != nil {
				t.Fatalf("disabled exit still admitted: lanes=%d removal=%d calls=%d job=%+v", lanes, removeAt, calls.Load(), job)
			}
		}
	}
}

func TestCodexHarvestAutomaticRetryAfterStopsImmediateRetries(t *testing.T) {
	m, account := newHarvestManagerTest(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", time.Now().Add(600*time.Second).UTC().Format(http.TimeFormat))
		w.WriteHeader(503)
	}))
	defer server.Close()
	configureHarvestPoolTest(t, m, 780, []string{gatewayModel}, server.URL)
	controls, _, _ := m.Controls.Controls(t.Context())
	controls.Speed.MaxRequestsPerRound, controls.Speed.MaxNodeAttempts = 10, 5
	if err := m.SaveControls(t.Context(), controls); err != nil {
		t.Fatal(err)
	}
	m.RunRound(t.Context())
	m.RunRound(t.Context())
	if calls.Load() != 1 || !account.CodexTicketProbeCoolingDown(gatewayModel, time.Now().Add(590*time.Second)) {
		t.Fatal("automatic retry ignored HTTP-date Retry-After")
	}
}
