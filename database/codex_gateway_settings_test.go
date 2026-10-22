package database

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/codex2api/internal/harvest"
)

func TestCodexGatewaySettingsPersistence(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "gateway.db")
			if driver == "postgres" {
				dsn = os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("isolated PostgreSQL test DSN not configured")
				}
			}
			db, err := New(driver, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ctx := context.Background()
			repo := db.HarvestRepository()
			initial := harvest.DefaultGatewaySettings()
			initial.Revision, initial.RouteRevision = "config-0", "route-0"
			raw, _ := json.Marshal(initial)
			if err := repo.Set(ctx, harvest.GatewaySettingsKey, string(raw)); err != nil {
				t.Fatal(err)
			}
			next := initial
			next.Revision, next.TicketPoolScheduling = "config-1", true
			if saved, err := repo.CompareAndSwapGatewaySettings(ctx, initial.Revision, next); err != nil || !saved {
				t.Fatalf("valid CAS failed: %v %v", saved, err)
			}
			stale := initial
			stale.Revision = "stale-overwrite"
			if saved, err := repo.CompareAndSwapGatewaySettings(ctx, initial.Revision, stale); err != nil || saved {
				t.Fatalf("stale CAS overwrote settings: %v %v", saved, err)
			}
			stored, err := repo.GetValue(ctx, harvest.GatewaySettingsKey)
			if err != nil {
				t.Fatal(err)
			}
			var got harvest.GatewaySettings
			if err = json.Unmarshal([]byte(stored), &got); err != nil {
				t.Fatal(err)
			}
			if got.Revision != "config-1" || got.RouteRevision != "route-0" || !got.TicketPoolScheduling {
				t.Fatalf("configuration or route revision lost: %+v", got)
			}
		})
	}
}
