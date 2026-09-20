package auth

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestEnsureCodexTicketAccessTokenRefreshesBeforeProbe(t *testing.T) {
	for _, tc := range []struct {
		name      string
		access    string
		expires   time.Time
		force     bool
		wantCalls int32
	}{
		{name: "refresh-token-only import", expires: time.Now().Add(time.Hour), wantCalls: 1},
		{name: "expired", access: "old-at", expires: time.Now().Add(-time.Minute), wantCalls: 1},
		{name: "near expiry", access: "old-at", expires: time.Now().Add(time.Minute), wantCalls: 1},
		{name: "unknown expiry", access: "old-at", wantCalls: 1},
		{name: "fresh", access: "old-at", expires: time.Now().Add(time.Hour)},
		{name: "401 forces refresh", access: "old-at", expires: time.Now().Add(time.Hour), force: true, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			store, db, id, _ := codexRefreshFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != "old-rt" {
					t.Error("refresh did not use the account's refresh token")
				}
				writeCodexRefreshedTokens(w)
			})
			if err := db.UpdateCredentials(context.Background(), id, map[string]any{
				"access_token": tc.access, "expires_at": tc.expires.Format(time.RFC3339),
			}); err != nil {
				t.Fatal(err)
			}
			row, err := db.GetAccountByID(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			account := store.FindByID(id)
			applyCodexCredentialValues(account, row.Credentials, row.CredentialGeneration)
			if err := store.EnsureCodexTicketAccessToken(context.Background(), account, tc.force); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != tc.wantCalls {
				t.Fatalf("OAuth calls = %d, want %d", calls.Load(), tc.wantCalls)
			}
			want := tc.access
			if tc.wantCalls > 0 {
				want = "new-at"
			}
			row, err = db.GetAccountByID(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if account.GetAccessToken() != want || row.GetCredential("access_token") != want {
				t.Fatal("prepared token must match both memory and durable credentials")
			}
			if tc.wantCalls > 0 && row.GetCredential("refresh_token") != "new-rt" {
				t.Fatal("rotated refresh token was not persisted")
			}
		})
	}
}

func TestEnsureCodexTicketAccessTokenSessionOnly(t *testing.T) {
	var calls atomic.Int32
	store, db, id, _ := codexRefreshFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		cookie, err := r.Cookie("__Secure-next-auth.session-token")
		if r.Method != http.MethodGet || err != nil || cookie.Value != "session-only" {
			t.Error("expected the existing session-token refresh flow")
		}
		_, _ = w.Write([]byte(`{"accessToken":"session-at"}`))
	})
	if err := db.UpdateCredentials(context.Background(), id, map[string]any{
		"access_token": "", "refresh_token": "", "session_token": "session-only", "expires_at": "",
	}); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAccountByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	account := store.FindByID(id)
	applyCodexCredentialValues(account, row.Credentials, row.CredentialGeneration)
	if err := store.EnsureCodexTicketAccessToken(context.Background(), account, false); err != nil {
		t.Fatal(err)
	}
	row, err = db.GetAccountByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || account.GetAccessToken() != "session-at" || row.GetCredential("access_token") != "session-at" {
		t.Fatal("session-only credentials must be refreshed and persisted before probing")
	}
}

func TestEnsureCodexTicketAccessTokenATOnly(t *testing.T) {
	store := &Store{}
	for _, tc := range []struct {
		name    string
		expires time.Time
		force   bool
		wantErr bool
	}{
		{name: "unknown expiry"},
		{name: "fresh", expires: time.Now().Add(time.Hour)},
		{name: "expired", expires: time.Now().Add(-time.Minute), wantErr: true},
		{name: "unauthorized requires reauthorization", force: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{AccessToken: "at-only", ExpiresAt: tc.expires}
			err := store.EnsureCodexTicketAccessToken(context.Background(), account, tc.force)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error=%v", err, tc.wantErr)
			}
		})
	}
}

func TestCanHarvestCodexTicketAcceptsRefreshableImports(t *testing.T) {
	for _, account := range []*Account{
		{AccessToken: "at"},
		{RefreshToken: "rt"},
		{SessionToken: "session"},
		{UpstreamType: " CODEX ", RefreshToken: "rt"},
		{Status: StatusCooldown, CooldownUtil: time.Now().Add(-time.Minute), RefreshToken: "rt"},
	} {
		if !account.CanHarvestCodexTicket(time.Now()) {
			t.Fatal("an active Codex account with usable or refreshable credentials must be eligible")
		}
	}
}
