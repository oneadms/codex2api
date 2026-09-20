package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestRefreshCodexTicketsSkipsIneligibleAccounts(t *testing.T) {
	withCodexTicketGate(t, "gpt-6-astra")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer codex-at" {
			t.Error("an ineligible account credential reached the Codex endpoint")
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	previousURL := codexTicketProbeURLForTest
	codexTicketProbeURLForTest = server.URL
	t.Cleanup(func() { codexTicketProbeURLForTest = previousURL })

	accounts := []*auth.Account{
		{DBID: 1, UpstreamType: "claude", AccessToken: "claude-at"},
		{DBID: 2, UpstreamType: "grok", AccessToken: "grok-at"},
		{DBID: 3, UpstreamType: "traecn", AccessToken: "trae-at"},
		{DBID: 4, UpstreamType: "antigravity", AccessToken: "google-at"},
		{DBID: 5, UpstreamType: "openai_responses", APIKey: "relay-key", AccessToken: "relay-at"},
		{DBID: 6, UpstreamType: "future-provider", AccessToken: "other-at"},
		{DBID: 7, CodexAuthMode: auth.CodexAuthModeAgentIdentity, AccessToken: "stale-at"},
		{DBID: 8, Disabled: 1, AccessToken: "disabled-at"},
		{DBID: 9, DispatchPaused: 1, AccessToken: "paused-at"},
		{DBID: 10, Status: auth.StatusError, AccessToken: "error-at"},
		{DBID: 11, HealthTier: auth.HealthTierBanned, AccessToken: "banned-at"},
		{DBID: 12, Status: auth.StatusCooldown, CooldownUtil: time.Now().Add(time.Hour), AccessToken: "cooldown-at"},
		{DBID: 13},
		{DBID: 14, UpstreamType: "codex", AccessToken: "codex-at", ExpiresAt: time.Now().Add(time.Hour)},
	}
	store := &auth.Store{}
	store.SetAccountsForTest(accounts)
	settings := auth.ConfiguredCodexTicketSettings()
	settings.HarvestProxyURL = server.URL
	// Excluded accounts must not consume the only probe slot.
	settings.MaxProbesPerRound = 1
	refreshCodexTickets(context.Background(), nil, store, settings)
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}
	for _, account := range accounts[:len(accounts)-1] {
		if len(account.CodexTicketProbes) != 0 {
			t.Errorf("ineligible account %d received a probe result", account.DBID)
		}
	}
	if accounts[len(accounts)-1].CodexTicketProbes["gpt-6-astra"] == nil {
		t.Fatal("eligible Codex account was starved by ineligible accounts")
	}
}

func TestHarvestOneTicketRefreshesCredentialsAndPublishes(t *testing.T) {
	for _, tc := range []struct {
		name         string
		access       string
		expired      bool
		noRefresh    bool
		refreshFails bool
		status       int
		alwaysReject bool
		budget       int
		wantProbes   int32
		wantRefresh  int32
		wantSuccess  bool
	}{
		{name: "refresh-token-only", budget: 2, wantProbes: 1, wantRefresh: 1, wantSuccess: true},
		{name: "expired access token", access: "old-at", expired: true, budget: 2, wantProbes: 1, wantRefresh: 1, wantSuccess: true},
		{name: "401 refresh then retry", access: "old-at", status: 401, budget: 2, wantProbes: 2, wantRefresh: 1, wantSuccess: true},
		{name: "persistent 401 is bounded", access: "old-at", status: 401, alwaysReject: true, budget: 4, wantProbes: 2, wantRefresh: 1},
		{name: "403 does not rotate token", access: "old-at", status: 403, budget: 2, wantProbes: 1},
		{name: "retry respects round budget", access: "old-at", status: 401, budget: 1, wantProbes: 1, wantRefresh: 1},
		{name: "401 AT-only needs reauthorization", access: "old-at", noRefresh: true, status: 401, budget: 2, wantProbes: 1},
		{name: "refresh failure after 401", access: "old-at", refreshFails: true, status: 401, budget: 2, wantProbes: 1, wantRefresh: 1},
		{name: "refresh failure prevents probe", refreshFails: true, budget: 2, wantRefresh: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withCodexTicketGate(t, "gpt-6-astra")
			var refreshes, probes atomic.Int32
			issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				refreshes.Add(1)
				if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != "old-rt" {
					t.Error("unexpected OAuth refresh request")
				}
				w.Header().Set("Content-Type", "application/json")
				if tc.refreshFails {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
					return
				}
				_, _ = w.Write([]byte(`{"access_token":"new-at","refresh_token":"new-rt","expires_in":3600}`))
			}))
			t.Cleanup(issuer.Close)
			previousDecorator := auth.ResinRequestDecorator
			auth.ResinRequestDecorator = func(targetURL, _ string) string {
				if targetURL != auth.TokenURL {
					t.Error("unexpected refresh endpoint")
				}
				return issuer.URL
			}
			t.Cleanup(func() { auth.ResinRequestDecorator = previousDecorator })

			state := testTicketState(time.Now(), auth.CodexTicketPersonalBlocks)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				probes.Add(1)
				if tc.status != 0 && (tc.alwaysReject || r.Header.Get("Authorization") == "Bearer old-at") {
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(`{"detail":"Could not parse your authentication token. Please try signing in again."}`))
					return
				}
				if r.Header.Get("Authorization") != "Bearer new-at" {
					t.Error("probe did not use the refreshed access token")
				}
				w.Header().Set(codexTurnStateHeader, state)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"type\":\"response.completed\"}\n\n"))
			}))
			t.Cleanup(server.Close)
			previousURL := codexTicketProbeURLForTest
			codexTicketProbeURLForTest = server.URL
			t.Cleanup(func() { codexTicketProbeURLForTest = previousURL })

			db, err := database.New("sqlite", filepath.Join(t.TempDir(), "tickets.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			expiresAt := time.Now().Add(time.Hour)
			if tc.expired {
				expiresAt = time.Now().Add(-time.Minute)
			}
			refreshToken := "old-rt"
			if tc.noRefresh {
				refreshToken = ""
			}
			id, err := db.InsertAccountWithCredentials(context.Background(), "ticket-test", map[string]any{
				"upstream_type": "codex", "plan_type": "plus", "access_token": tc.access,
				"refresh_token": refreshToken, "expires_at": expiresAt.Format(time.RFC3339),
			}, "")
			if err != nil {
				t.Fatal(err)
			}
			store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1})
			t.Cleanup(store.Stop)
			if err := store.LoadAccountByID(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			account := store.FindByID(id)
			budget := tc.budget
			success := harvestOneTicket(context.Background(), db, store, account, "gpt-6-astra", server.URL, 5*time.Second, &budget)
			if success != tc.wantSuccess || probes.Load() != tc.wantProbes || refreshes.Load() != tc.wantRefresh {
				t.Fatalf("success=%v probes=%d refreshes=%d, want success=%v probes=%d refreshes=%d", success, probes.Load(), refreshes.Load(), tc.wantSuccess, tc.wantProbes, tc.wantRefresh)
			}
			if budget < 0 {
				t.Fatal("round budget exceeded")
			}
			row, err := db.GetAccountByID(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantRefresh > 0 && !tc.refreshFails && (row.GetCredential("refresh_token") != "new-rt" || account.GetAccessToken() != "new-at") {
				t.Fatal("refreshed credentials were not durably published")
			}
			probe := account.CodexTicketProbes["gpt-6-astra"]
			if probe == nil {
				t.Fatal("probe summary missing")
			}
			if tc.wantSuccess {
				persisted := auth.ParseCodexTicket("gpt-6-astra", row.Credentials[auth.CodexTicketCredentialKey("gpt-6-astra")])
				if persisted == nil || persisted.State != state || account.CodexTicketForModel("gpt-6-astra", time.Now(), 292) == nil {
					t.Fatal("ticket missing from database or runtime")
				}
				if probe.Result != "success" || probe.NextProbeAt != nil {
					t.Fatal("successful probe should be ready, not cooling down")
				}
			} else if probe.Result != "token_error" || probe.NextProbeAt == nil || !probe.NextProbeAt.After(time.Now()) {
				t.Fatal("failed authentication must record a cooldown, not success")
			}
		})
	}
}

func TestHarvestOneTicketPersistenceFailureIsNotSuccess(t *testing.T) {
	withCodexTicketGate(t, "gpt-6-astra")
	state := testTicketState(time.Now(), auth.CodexTicketPersonalBlocks)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(codexTurnStateHeader, state)
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\"}\n\n"))
	}))
	t.Cleanup(server.Close)
	previousURL := codexTicketProbeURLForTest
	codexTicketProbeURLForTest = server.URL
	t.Cleanup(func() { codexTicketProbeURLForTest = previousURL })
	account := &auth.Account{DBID: 1, AccessToken: "at", ExpiresAt: time.Now().Add(time.Hour)}
	store := &auth.Store{}
	store.SetAccountsForTest([]*auth.Account{account})
	budget := 1
	if harvestOneTicket(context.Background(), nil, store, account, "gpt-6-astra", server.URL, 5*time.Second, &budget) {
		t.Fatal("missing database must not be reported as success")
	}
	probe := account.CodexTicketProbes["gpt-6-astra"]
	if probe == nil || probe.Result != "error" || probe.NextProbeAt == nil || len(account.CodexTickets) != 0 {
		t.Fatal("failed persistence must leave a failure summary and no ticket")
	}
}
