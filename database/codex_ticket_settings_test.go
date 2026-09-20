package database

import (
	"context"
	"path/filepath"
	"testing"
)

// 打票配置存在独立的 codex_ticket_config 列：读写往返、默认值、以及「通用设置保存
// 不会覆盖它」三件事都要成立。
func TestCodexTicketConfigRoundTrip(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "codex-ticket.db"))
	if err != nil {
		t.Fatalf("New(sqlite) error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	// 从未写过时读出空对象（由 auth 侧补默认值）。
	raw, err := db.LoadCodexTicketConfig(ctx)
	if err != nil {
		t.Fatalf("LoadCodexTicketConfig() error = %v", err)
	}
	if raw != "{}" {
		t.Fatalf("fresh config = %q, want {}", raw)
	}

	payload := `{"enabled":true,"harvest_proxy_url":"socks5h://127.0.0.1:1080","models":["gpt-5.5"]}`
	if err := db.SaveCodexTicketConfig(ctx, payload); err != nil {
		t.Fatalf("SaveCodexTicketConfig() error = %v", err)
	}
	raw, err = db.LoadCodexTicketConfig(ctx)
	if err != nil {
		t.Fatalf("reload error = %v", err)
	}
	if raw != payload {
		t.Fatalf("config = %q, want %q", raw, payload)
	}

	// 通用设置保存不能覆盖独立配置。
	settings, err := db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("GetSystemSettings() error = %v", err)
	}
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		t.Fatalf("UpdateSystemSettings() error = %v", err)
	}
	latest, err := db.LoadCodexTicketConfig(ctx)
	if err != nil {
		t.Fatalf("reload after settings save error = %v", err)
	}
	if latest != payload {
		t.Fatalf("config overwritten by generic settings save: %q", latest)
	}

	// 空串落库退回空对象，避免把空值读成"坏 JSON"。
	if err := db.SaveCodexTicketConfig(ctx, "   "); err != nil {
		t.Fatalf("save blank error = %v", err)
	}
	raw, err = db.LoadCodexTicketConfig(ctx)
	if err != nil || raw != "{}" {
		t.Fatalf("blank save = %q err=%v", raw, err)
	}
}
