package database

import (
	"context"
	"os"
	"testing"
)

func TestImagesSettingsPersistencePostgres(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("requires an isolated CODEX2API_TEST_POSTGRES_DSN database")
	}
	db, err := New("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	for _, model := range []string{"gpt-5.6-sol", "gpt-5.6-terra", ""} {
		settings, err := db.GetSystemSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if settings == nil {
			settings = &SystemSettings{}
		}
		settings.CodexImagesMainModel = model
		if err := db.UpdateSystemSettings(ctx, settings); err != nil {
			t.Fatal(err)
		}
		got, err := db.GetSystemSettings(ctx)
		if err != nil || got == nil || got.CodexImagesMainModel != model {
			t.Fatalf("image driver did not persist %q: %v", model, err)
		}
	}
}
