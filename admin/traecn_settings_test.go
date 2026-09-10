package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

func TestTraeCNSettingsRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := auth.ConfiguredTraeCNSettings()
	t.Cleanup(func() { auth.SetConfiguredTraeCNSettings(previous) })
	auth.SetConfiguredTraeCNSettings(auth.TraeCNSettings{})
	db := newTestAdminDB(t)
	handler := &Handler{db: db}
	router := gin.New()
	router.GET("/settings/traecn", handler.GetTraeCNSettings)
	router.PUT("/settings/traecn", handler.UpdateTraeCNSettings)
	put := func(body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPut, "/settings/traecn", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)
		return recorder
	}
	if rec := put(`{"model_mapping":{"my-code":"doubao-seed-code"}}`); rec.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", rec.Code, rec.Body.String())
	}
	if auth.TraeCNRequestModel("my-code") != "doubao-seed-code" {
		t.Fatal("saved mapping not applied")
	}
	raw, err := db.LoadTraeCNConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := auth.ParseTraeCNSettings(raw)
	if err != nil || parsed.ModelMapping["my-code"] != "doubao-seed-code" {
		t.Fatalf("persisted=%s err=%v", raw, err)
	}
	// 通用配置保存不能覆盖独立配置，启动时重新读取也应得到同一映射。
	settings, err := db.GetSystemSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateSystemSettings(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	auth.SetConfiguredTraeCNSettings(auth.TraeCNSettings{})
	reloaded, err := db.LoadTraeCNConfig(t.Context())
	if err != nil || reloaded != raw {
		t.Fatalf("config overwritten: %s err=%v", reloaded, err)
	}
	auth.SetConfiguredTraeCNSettings(parsed)
	for _, bad := range []string{`{}`, `{"model_mapping":null}`, `{"model_mapping":{"x":"missing-target"}}`, `{"model_mapping":{"A":"glm-5.1","a":"doubao-seed-code"}}`, `{"model_mapping":{"x":42}}`} {
		if rec := put(bad); rec.Code != http.StatusBadRequest {
			t.Errorf("invalid request=%s status=%d body=%s", bad, rec.Code, rec.Body.String())
		}
	}
	if auth.TraeCNRequestModel("my-code") != "doubao-seed-code" {
		t.Fatal("rejected update changed the mapping")
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/settings/traecn", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"my-code":"doubao-seed-code"`) {
		t.Fatalf("get: %s", recorder.Body.String())
	}
	if rec := put(`{"model_mapping":{}}`); rec.Code != http.StatusOK {
		t.Fatalf("clear: %s", rec.Body.String())
	}
	if auth.TraeCNRequestModel("my-code") != "my-code" {
		t.Fatal("mapping was not cleared")
	}
}
