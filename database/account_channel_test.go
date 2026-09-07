package database

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

func TestAPIKeyLimitsResolveUpstreamChannel(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "auto empty", in: "", want: UpstreamChannelAuto},
		{name: "codex", in: " CODEX ", want: UpstreamChannelCodex},
		{name: "grok", in: "Grok", want: UpstreamChannelGrok},
		{name: "antigravity", in: " Antigravity ", want: UpstreamChannelAntigravity},
		{name: "traecn", in: " TRAECN ", want: UpstreamChannelTraeCN},
		{name: "claude", in: " Claude ", want: UpstreamChannelClaude},
		{name: "unknown", in: "other", want: UpstreamChannelAuto},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (APIKeyLimits{UpstreamChannel: tt.in}).ResolveUpstreamChannel(); got != tt.want {
				t.Fatalf("ResolveUpstreamChannel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInsertAccountWithUpstreamIfRefreshTokenAbsentIsAtomic(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "traecn-rt-reservation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	credentials := func() map[string]any {
		return map[string]any{"upstream_type": "traecn", "refresh_token": "same-rt"}
	}
	type result struct {
		id       int64
		inserted bool
		err      error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, inserted, insertErr := db.InsertAccountWithUpstreamIfRefreshTokenAbsent(ctx, "trae", "trae", "traecn", "same-rt", credentials(), "")
			results <- result{id: id, inserted: inserted, err: insertErr}
		}()
	}
	wg.Wait()
	close(results)
	insertedCount := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("atomic RT reservation: %v", result.err)
		}
		if result.inserted {
			insertedCount++
			if result.id <= 0 {
				t.Fatalf("inserted reservation returned id %d", result.id)
			}
		}
	}
	if insertedCount != 1 {
		t.Fatalf("inserted reservations = %d, want exactly one", insertedCount)
	}
	count, err := db.CountAll(ctx)
	if err != nil || count != 1 {
		t.Fatalf("account count = %d, err=%v, want 1", count, err)
	}
}

func TestEnsureAccountCredentialFamilyIDInitializesLegacyTraeCNAccount(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "traecn-legacy-family.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "legacy-traecn", "trae", "traecn", map[string]any{
		"upstream_type": "traecn",
		"refresh_token": "legacy-refresh-token",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a row written before the canonical family column was introduced.
	if _, err := db.conn.ExecContext(ctx, `UPDATE accounts SET credential_family_id='', credentials=json_remove(credentials, '$.credential_family_id') WHERE id=$1`, id); err != nil {
		t.Fatalf("clear legacy family: %v", err)
	}

	family, err := db.EnsureAccountCredentialFamilyID(ctx, id, "")
	if err != nil {
		t.Fatalf("EnsureAccountCredentialFamilyID: %v", err)
	}
	if family == "" {
		t.Fatal("legacy Trae CN account received an empty credential family")
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.CredentialFamilyID != family {
		t.Fatalf("canonical family = %q, want %q", row.CredentialFamilyID, family)
	}
	// The compatibility JSON field is synchronized by the first credential CAS
	// after the lease initialization; the initialization itself only owns the
	// canonical column and must not mutate the RT reservation.
	if row.GetCredential("refresh_token") != "legacy-refresh-token" {
		t.Fatalf("legacy refresh token changed during family initialization")
	}
}

func TestNormalizeAccountGroupChannel(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "codex", want: AccountGroupChannelCodex},
		{in: " GROK ", want: AccountGroupChannelGrok},
		{in: "Antigravity", want: AccountGroupChannelAntigravity},
		{in: "TRAECN", want: AccountGroupChannelTraeCN},
		{in: "", want: AccountGroupChannelCodex},
		{in: "other", want: AccountGroupChannelCodex},
	}
	for _, tt := range tests {
		if got := NormalizeAccountGroupChannel(tt.in); got != tt.want {
			t.Errorf("NormalizeAccountGroupChannel(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSQLiteListAccountListProjectionByChannel(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "account-list-projection-channel.db"))
	if err != nil {
		t.Fatalf("New(sqlite) error: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	codexID, err := db.InsertAccount(ctx, "codex", "codex-refresh", "")
	if err != nil {
		t.Fatalf("insert codex account: %v", err)
	}
	grokID, err := db.InsertAccountWithUpstream(ctx, "grok", "xai", "oauth", map[string]interface{}{
		"upstream_type": "grok",
		"api_key":       "grok-secret",
	}, "")
	if err != nil {
		t.Fatalf("insert grok account: %v", err)
	}
	antigravityID, err := db.InsertAccountWithUpstream(ctx, "antigravity", "google", "oauth", map[string]interface{}{
		"upstream_type":            "antigravity",
		"refresh_token":            "antigravity-secret",
		"avatar_url":               "https://example.com/avatar.png",
		"verified_email":           true,
		"project_id":               "project-1",
		"antigravity_sync_error":   "sync failed",
		"antigravity_sync_warning": "credits snapshot preserved",
		"antigravity_permissions":  `{"allowed":false}`,
		"antigravity_quota":        `{"models":[],"forbidden":true}`,
	}, "")
	if err != nil {
		t.Fatalf("insert antigravity account: %v", err)
	}
	traeID, err := db.InsertAccountWithUpstream(ctx, "traecn", "trae", "traecn", map[string]interface{}{
		"upstream_type":           "traecn",
		"refresh_token":           "trae-refresh",
		"access_token":            "trae-access",
		"traecn_host":             "https://trae.example",
		"traecn_user_id":          "trae-user",
		"traecn_upstream_models":  []string{"deepseek-v3", "auto"},
		"traecn_model_allowlist":  []string{"deepseek-v3"},
		"traecn_models_synced_at": "2026-08-31T00:00:00Z",
	}, "")
	if err != nil {
		t.Fatalf("insert traecn account: %v", err)
	}
	claudeID, err := db.InsertAccountWithUpstream(ctx, "claude", "anthropic", "oauth", map[string]interface{}{
		"upstream_type":            "claude",
		"access_token":             "claude-secret",
		"claude_usage_probe_at":    "2026-08-29T05:00:00Z",
		"claude_usage_probe_error": "",
		"models":                   []string{"claude-sonnet-4-5"},
	}, "")
	if err != nil {
		t.Fatalf("insert Claude account: %v", err)
	}

	tests := []struct {
		channel string
		wantID  int64
	}{
		{channel: UpstreamChannelCodex, wantID: codexID},
		{channel: UpstreamChannelGrok, wantID: grokID},
		{channel: UpstreamChannelAntigravity, wantID: antigravityID},
		{channel: UpstreamChannelTraeCN, wantID: traeID},
		{channel: UpstreamChannelClaude, wantID: claudeID},
	}
	for _, tt := range tests {
		t.Run(tt.channel, func(t *testing.T) {
			rows, err := db.ListAccountListProjection(ctx, tt.channel)
			if err != nil {
				t.Fatalf("ListAccountListProjection(%q) error: %v", tt.channel, err)
			}
			if len(rows) != 1 || rows[0].ID != tt.wantID {
				t.Fatalf("ListAccountListProjection(%q) = %+v, want id %d", tt.channel, rows, tt.wantID)
			}
			if tt.channel == UpstreamChannelAntigravity && (rows[0].GetCredential("avatar_url") == "" || !rows[0].GetCredentialBool("verified_email") || rows[0].GetCredential("project_id") != "project-1" || rows[0].GetCredential("antigravity_sync_error") != "sync failed" || rows[0].GetCredential("antigravity_sync_warning") == "" || rows[0].GetCredential("antigravity_permissions") == "" || rows[0].GetCredential("antigravity_quota") == "") {
				t.Fatalf("Antigravity projection omitted control-plane status fields: %#v", rows[0].Credentials)
			}
			if tt.channel == UpstreamChannelTraeCN && (rows[0].GetCredential("traecn_host") != "https://trae.example" || rows[0].GetCredential("traecn_user_id") != "trae-user" || len(rows[0].GetCredentialStringSlice("traecn_upstream_models")) != 2 || len(rows[0].GetCredentialStringSlice("traecn_model_allowlist")) != 1 || rows[0].GetCredential("traecn_models_synced_at") == "") {
				t.Fatalf("Trae CN projection omitted account fields: %#v", rows[0].Credentials)
			}
			if tt.channel == UpstreamChannelClaude && (rows[0].GetCredential("claude_usage_probe_at") == "" || rows[0].GetCredential("claude_usage_probe_error") != "" || len(rows[0].GetCredentialStringSlice("models")) != 1) {
				t.Fatalf("Claude projection omitted sampling metadata: %#v", rows[0].Credentials)
			}
		})
	}
}

func TestReplaceAccountCredentialsCASUpdatesCanonicalFamily(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "antigravity-family-cas.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "antigravity", "google", "oauth", map[string]interface{}{
		"upstream_type":        "antigravity",
		"refresh_token":        "old-refresh",
		"credential_family_id": "ag_old",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	newGeneration, applied, err := db.ReplaceAccountCredentialsCAS(ctx, id, 1, "ag_new", map[string]any{
		"refresh_token": "new-refresh",
	})
	if err != nil || !applied || newGeneration != 2 {
		t.Fatalf("ReplaceAccountCredentialsCAS() = generation %d applied %v err %v", newGeneration, applied, err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.CredentialGeneration != 2 || row.CredentialFamilyID != "ag_new" || row.GetCredential("credential_family_id") != "ag_new" || row.GetCredential("refresh_token") != "new-refresh" {
		t.Fatalf("updated row = generation %d family %q credentials %#v", row.CredentialGeneration, row.CredentialFamilyID, row.Credentials)
	}
	if _, applied, err := db.ReplaceAccountCredentialsCAS(ctx, id, 1, "ag_stale", map[string]any{"refresh_token": "stale"}); err != nil || applied {
		t.Fatalf("stale ReplaceAccountCredentialsCAS() applied=%v err=%v", applied, err)
	}
}

func TestUpdateAccountCredentialsCASKeepsEmbeddedFamilyCanonical(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "antigravity-family-refresh-cas.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "antigravity", "google", "oauth", map[string]interface{}{
		"upstream_type":        "antigravity",
		"refresh_token":        "old-refresh",
		"credential_family_id": "ag_canonical",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	newGeneration, applied, err := db.UpdateAccountCredentialsCAS(ctx, id, 1, map[string]any{
		"refresh_token":        "rotated-refresh",
		"credential_family_id": "ag_drifted",
	})
	if err != nil || !applied || newGeneration != 2 {
		t.Fatalf("UpdateAccountCredentialsCAS() = generation %d applied %v err %v", newGeneration, applied, err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.CredentialGeneration != 2 || row.CredentialFamilyID != "ag_canonical" || row.GetCredential("credential_family_id") != "ag_canonical" || row.GetCredential("refresh_token") != "rotated-refresh" {
		t.Fatalf("updated row = generation %d family %q credentials %#v", row.CredentialGeneration, row.CredentialFamilyID, row.Credentials)
	}
}

func TestSQLiteListAccountListProjectionCarriesClaudeAuthKind(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "account-list-projection-claude-auth-kind.db"))
	if err != nil {
		t.Fatalf("New(sqlite) error: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	setupID, err := db.InsertAccountWithUpstream(ctx, "claude-setup", "anthropic", "claude", map[string]interface{}{
		"upstream_type":    "claude",
		"access_token":     "sk-ant-oat01-secret",
		"claude_auth_kind": "setup_token",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	oauthID, err := db.InsertAccountWithUpstream(ctx, "claude-oauth", "anthropic", "claude", map[string]interface{}{
		"upstream_type": "claude",
		"access_token":  "at",
		"refresh_token": "rt",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	apiID, err := db.InsertAccountWithUpstream(ctx, "claude-api", "anthropic", "claude", map[string]interface{}{
		"upstream_type": "claude", "claude_auth_kind": "api_key", "claude_base_url": "https://example.com/v1", "access_token": "api-secret",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.ListAccountListProjection(ctx, UpstreamChannelClaude)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]*AccountRow{}
	for _, row := range rows {
		byID[row.ID] = row
	}
	if got := byID[apiID]; got == nil || got.GetCredential("claude_auth_kind") != "api_key" || got.GetCredential("access_token") != "" || got.GetCredential("refresh_token") != "" {
		t.Fatal("API key projection must carry auth kind without secret credentials")
	}
	if got := byID[setupID].GetCredential("claude_auth_kind"); got != "setup_token" {
		t.Fatalf("setup token projection auth kind = %q", got)
	}
	if got := byID[setupID].GetCredential("access_token"); got != "" {
		t.Fatalf("projection must never carry the access token, got %q", got)
	}
	if got := byID[oauthID].GetCredential("claude_auth_kind"); got != "" {
		t.Fatalf("legacy oauth row must keep an empty auth kind in the projection, got %q", got)
	}
	if got := byID[oauthID].GetCredential("refresh_token"); got != "configured" {
		t.Fatalf("refresh token presence marker = %q", got)
	}
}
