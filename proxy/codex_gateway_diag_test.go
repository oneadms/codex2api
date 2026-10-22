package proxy

import (
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/harvest"
)

func TestSaveGatewaySettingsAccountEligibility(t *testing.T) {
	previousSettings := auth.ConfiguredCodexTicketSettings()
	previousManager, previousSink := codexHarvester.Load(), codexTicketFeedback.Load()
	t.Cleanup(func() {
		auth.SetConfiguredCodexTicketSettings(previousSettings)
		codexHarvester.Store(previousManager)
		codexTicketFeedback.Store(previousSink)
	})
	settings := previousSettings
	settings.Enabled = true
	settings.HarvestProxyURL = "socks5h://127.0.0.1:1080"
	settings.Models = []string{"gpt-6-astra"}
	auth.SetConfiguredCodexTicketSettings(settings)

	db, err := database.New("sqlite", t.TempDir()+"/gateway-accounts.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	source, err := db.InsertAccountWithCredentials(t.Context(), "gateway-source", map[string]any{"upstream_type": "codex", "access_token": "ws-token", "plan_type": "plus"}, "")
	if err != nil {
		t.Fatal(err)
	}
	target, err := db.InsertAccountWithCredentials(t.Context(), "gateway-target", map[string]any{"upstream_type": "codex", "access_token": "ws-token-2", "plan_type": "plus"}, "")
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 4})
	t.Cleanup(store.Stop)
	if err = store.LoadAccountByID(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	if err = store.LoadAccountByID(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	m := ConfigureCodexHarvest(db, store, t.TempDir())
	v := harvest.DefaultGatewaySettings()
	v.CookiePool.SourceAccountIDs = []int64{source}
	v.CookiePool.TargetAccountIDs = []int64{target}
	v.WSSession.Enabled = true
	v.WSSession.AccountIDs = []int64{target}
	saved, err := m.SaveGatewaySettings(t.Context(), v)
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if saved.Revision == "" {
		t.Fatal("保存后 revision 为空")
	}
	// 过期 revision 重复保存必须返回冲突而不是覆盖。
	v.WSSession.TTLSeconds = 120
	if _, err = m.SaveGatewaySettings(t.Context(), v); err != ErrGatewayRevisionConflict {
		t.Fatalf("过期 revision 保存应冲突，实际: %v", err)
	}
}
