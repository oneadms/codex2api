package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

func TestTraeCNModelSyncSettingsRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := auth.ConfiguredTraeCNSettings()
	t.Cleanup(func() { auth.SetConfiguredTraeCNSettings(previous) })
	auth.SetConfiguredTraeCNSettings(auth.TraeCNSettings{})
	db := newTestAdminDB(t)
	h := &Handler{db: db}
	router := gin.New()
	router.GET("/settings/traecn", h.GetTraeCNSettings)
	router.PUT("/settings/traecn", h.UpdateTraeCNSettings)
	put := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/settings/traecn", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		return rec
	}
	get := func() traeCNSettingsResponse {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings/traecn", nil))
		var result traeCNSettingsResponse
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &result) != nil {
			t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
		}
		return result
	}
	if got := get(); got.ModelSyncEnabled || got.ModelSyncTime != "00:00" || got.ModelSyncTimezone != "Asia/Shanghai" {
		t.Fatalf("unexpected defaults: %+v", got)
	}
	for _, body := range []string{
		`{"model_mapping":{"my-code":"doubao-seed-code"},"preflight_sse_passthrough":true}`,
		`{"model_sync_enabled":true,"model_sync_time":"03:25","model_sync_timezone":"UTC"}`,
		`{"model_mapping":{"other-code":"glm-5.1"}}`,
		`{"preflight_sse_passthrough":false}`,
	} {
		if rec := put(body); rec.Code != http.StatusOK {
			t.Fatalf("PUT %s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	got := get()
	if !got.ModelSyncEnabled || got.ModelSyncTime != "03:25" || got.ModelSyncTimezone != "UTC" || got.ModelMapping["other-code"] != "glm-5.1" || got.PreflightSSEPassthrough {
		t.Fatalf("partial updates dropped settings: %+v", got)
	}
	raw, err := db.LoadTraeCNConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := auth.ParseTraeCNSettings(raw)
	if err != nil || !parsed.ModelSyncEnabled || parsed.ModelSyncTime != "03:25" || parsed.ModelSyncTimezone != "UTC" {
		t.Fatalf("persisted schedule=%s err=%v", raw, err)
	}
	// General settings writes and a process reload must preserve the schedule.
	general, err := db.GetSystemSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateSystemSettings(t.Context(), general); err != nil {
		t.Fatal(err)
	}
	reloaded, err := db.LoadTraeCNConfig(t.Context())
	if err != nil || reloaded != raw {
		t.Fatalf("general settings changed schedule: %s err=%v", reloaded, err)
	}
	auth.SetConfiguredTraeCNSettings(parsed)
	for _, body := range []string{
		`{}`, `{"model_sync_time":"24:00"}`, `{"model_sync_time":"1:00"}`,
		`{"model_sync_time":""}`, `{"model_sync_timezone":""}`,
		`{"model_sync_timezone":"Local"}`, `{"model_sync_timezone":"bad-zone"}`,
		`{"model_sync_enabled":"true"}`, `{"model_sync_enabled":null}`,
	} {
		if rec := put(body); rec.Code != http.StatusBadRequest {
			t.Errorf("invalid schedule %s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if got := get(); got.ModelSyncTime != "03:25" || !got.ModelSyncEnabled {
		t.Fatalf("invalid update mutated schedule: %+v", got)
	}
	// An old mapping can become stale after a catalog refresh; it must not
	// block turning off the scheduler or changing its time.
	parsed.ModelMapping = map[string]string{"old-name": "removed-upstream-model"}
	auth.SetConfiguredTraeCNSettings(parsed)
	if rec := put(`{"model_sync_enabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("stale mapping blocked schedule update: %s", rec.Body.String())
	}
	if got := get(); got.ModelSyncEnabled || got.ModelMapping["old-name"] != "removed-upstream-model" {
		t.Fatalf("schedule-only update modified mapping: %+v", got)
	}
}
