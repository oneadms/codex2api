package admin

import (
	"context"
	"database/sql"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
)

func newImagesSettingsHandler(t *testing.T) (*Handler, *database.DB, string) {
	t.Helper()
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	path := filepath.Join(t.TempDir(), "images-settings.sqlite")
	db, err := database.New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	settings := defaultBootstrapSettings()
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	proxy.ApplyRuntimeSettingsFromSystem(settings)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	return NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret"), db, path
}

func TestImagesSettingsPartialUpdatePersistenceAndClear(t *testing.T) {
	handler, db, _ := newImagesSettingsHandler(t)
	t.Setenv("CODEX_IMAGES_MAIN_MODEL", "gpt-5.5")
	for _, tc := range []struct {
		patch map[string]any
		want  string
	}{
		{map[string]any{"codex_images_main_model": " gpt-5.6-sol "}, "gpt-5.6-sol"},
		{map[string]any{"site_name": "image settings test"}, "gpt-5.6-sol"},
		{map[string]any{"codex_images_main_model": ""}, ""},
	} {
		response := invokeResponseCacheSettingsAdmin(t, handler, http.MethodPut, tc.patch)
		if response.Code != http.StatusOK {
			t.Fatalf("PUT status=%d: %s", response.Code, response.Body.String())
		}
		got := decodeResponseCacheSettingsResponse(t, response)
		if got.CodexImagesMainModel != tc.want || got.CodexImagesDefaultMainModel != "gpt-5.5" || proxy.CurrentRuntimeSettings().CodexImagesMainModel != tc.want {
			t.Fatalf("PUT/runtime model=%q, default=%q; want %q", got.CodexImagesMainModel, got.CodexImagesDefaultMainModel, tc.want)
		}
		persisted, err := db.GetSystemSettings(context.Background())
		if err != nil || persisted == nil || persisted.CodexImagesMainModel != tc.want {
			t.Fatalf("persisted model mismatch: %v", err)
		}
		// Simulate startup loading the persisted value after runtime settings were reset.
		proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings())
		proxy.ApplyRuntimeSettingsFromSystem(persisted)
		get := invokeResponseCacheSettingsAdmin(t, handler, http.MethodGet, nil)
		if get.Code != http.StatusOK || decodeResponseCacheSettingsResponse(t, get).CodexImagesMainModel != tc.want {
			t.Fatalf("GET after reload failed: %d", get.Code)
		}
	}
}

func TestImagesSettingsInvalidModelRejectedBeforeMutation(t *testing.T) {
	handler, db, _ := newImagesSettingsHandler(t)
	before := proxy.CurrentRuntimeSettings().CodexImagesMainModel
	response := invokeResponseCacheSettingsAdmin(t, handler, http.MethodPut, map[string]any{
		"codex_images_main_model": "gpt-image-2.5-flare",
		"test_model":              "gpt-6-astra",
	})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", response.Code)
	}
	persisted, err := db.GetSystemSettings(context.Background())
	if err != nil || persisted.CodexImagesMainModel != before || proxy.CurrentRuntimeSettings().CodexImagesMainModel != before || handler.store.GetTestModel() == "gpt-6-astra" {
		t.Fatal("invalid image settings changed runtime or storage")
	}
}

func TestImagesSettingsPersistenceFailureDoesNotApplyModel(t *testing.T) {
	handler, db, path := newImagesSettingsHandler(t)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`CREATE TRIGGER reject_image_settings BEFORE INSERT ON system_settings BEGIN SELECT RAISE(ABORT, 'forced settings write failure'); END`); err != nil {
		t.Fatal(err)
	}
	response := invokeResponseCacheSettingsAdmin(t, handler, http.MethodPut, map[string]any{"codex_images_main_model": "gpt-5.6-sol"})
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", response.Code)
	}
	persisted, err := db.GetSystemSettings(context.Background())
	if err != nil || persisted.CodexImagesMainModel != "" || proxy.CurrentRuntimeSettings().CodexImagesMainModel != "" {
		t.Fatal("failed persistence applied the image driver")
	}
}
