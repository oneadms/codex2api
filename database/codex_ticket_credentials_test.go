package database

import (
	"context"
	"path/filepath"
	"testing"
)

// 门票按动态键名（codex_ticket:<model>）存在账号凭据里，必须能通过 UpdateCredentials
// 的部分合并写入、并且不破坏凭据里的其它字段。SQLite 下这种带冒号的键会走整块
// 读改写路径，所以这里同时验证合并语义。
func TestUpdateCredentialsPersistsDynamicTicketKeys(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "codex-ticket-credentials.db"))
	if err != nil {
		t.Fatalf("New(sqlite) error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	result, err := db.conn.ExecContext(ctx, `
		INSERT INTO accounts (name, credentials, status) VALUES ('ticket-account', '{"refresh_token":"rt-1"}', 'active')
	`)
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("account id: %v", err)
	}

	ticket := map[string]interface{}{
		"model": "gpt-5.5", "state": "gAAAAAstate", "length": 12,
	}
	if err := db.UpdateCredentials(ctx, id, map[string]interface{}{
		"codex_ticket:gpt-5.5":       ticket,
		"codex_ticket:gpt-5.5:probe": map[string]interface{}{"result": "success"},
	}); err != nil {
		t.Fatalf("UpdateCredentials() error = %v", err)
	}

	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID() error = %v", err)
	}
	entries := row.CredentialEntries()
	if _, ok := entries["codex_ticket:gpt-5.5"]; !ok {
		t.Fatalf("ticket key missing, entries=%v", entries)
	}
	if _, ok := entries["codex_ticket:gpt-5.5:probe"]; !ok {
		t.Fatalf("probe key missing, entries=%v", entries)
	}
	// 既有的 refresh_token 不能被这次部分更新抹掉。
	if got := row.GetCredential("refresh_token"); got != "rt-1" {
		t.Fatalf("existing credential lost: %q", got)
	}

	// 第二次写入另一个模型时，第一个模型的票必须还在。
	if err := db.UpdateCredentials(ctx, id, map[string]interface{}{
		"codex_ticket:gpt-5.1-codex": ticket,
	}); err != nil {
		t.Fatalf("second UpdateCredentials() error = %v", err)
	}
	row, err = db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID() error = %v", err)
	}
	entries = row.CredentialEntries()
	for _, key := range []string{"codex_ticket:gpt-5.5", "codex_ticket:gpt-5.5:probe", "codex_ticket:gpt-5.1-codex"} {
		if _, ok := entries[key]; !ok {
			t.Fatalf("key %q lost after merge, entries=%v", key, entries)
		}
	}
}
