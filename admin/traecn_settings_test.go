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

// 「前置元数据立即下发」是 TRAECN 独立的开关：默认关闭，可与模型映射分开提交，
// 且不会被映射保存/清空或通用设置保存连带重置。
func TestTraeCNSettingsPreflightPassthroughRoundTrip(t *testing.T) {
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
	get := func() string {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/settings/traecn", nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("get status=%d", recorder.Code)
		}
		return recorder.Body.String()
	}

	// 默认关闭，且 GET 必须显式暴露该字段（前端据此渲染开关初值）。
	if auth.TraeCNPreflightSSEPassthroughEnabled() {
		t.Fatal("preflight passthrough must default to off")
	}
	if body := get(); !strings.Contains(body, `"preflight_sse_passthrough":false`) {
		t.Fatalf("get default: %s", body)
	}

	// 只提交开关时不应触碰映射。
	if rec := put(`{"model_mapping":{"my-code":"doubao-seed-code"}}`); rec.Code != http.StatusOK {
		t.Fatalf("seed mapping: %s", rec.Body.String())
	}
	if rec := put(`{"preflight_sse_passthrough":true}`); rec.Code != http.StatusOK {
		t.Fatalf("enable: %s", rec.Body.String())
	}
	if !auth.TraeCNPreflightSSEPassthroughEnabled() {
		t.Fatal("enable did not take effect")
	}
	if auth.TraeCNRequestModel("my-code") != "doubao-seed-code" {
		t.Fatal("enabling the switch dropped the mapping")
	}

	// 持久化到独立的 traecn_config，重启后仍生效。
	raw, err := db.LoadTraeCNConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := auth.ParseTraeCNSettings(raw)
	if err != nil || !parsed.PreflightSSEPassthrough {
		t.Fatalf("persisted=%s err=%v", raw, err)
	}
	if parsed.ModelMapping["my-code"] != "doubao-seed-code" {
		t.Fatalf("persisted mapping lost: %s", raw)
	}

	// 只提交映射时不应把开关重置回默认。
	if rec := put(`{"model_mapping":{"my-code":"doubao-seed-code","other-code":"glm-5.1"}}`); rec.Code != http.StatusOK {
		t.Fatalf("update mapping: %s", rec.Body.String())
	}
	if !auth.TraeCNPreflightSSEPassthroughEnabled() {
		t.Fatal("saving the mapping reset the preflight switch")
	}
	if body := get(); !strings.Contains(body, `"preflight_sse_passthrough":true`) {
		t.Fatalf("get enabled: %s", body)
	}

	// 记录本次持久化的完整配置，供「通用设置保存不能覆盖」对比。
	raw, err = db.LoadTraeCNConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// 通用设置保存不能覆盖独立配置。
	settings, err := db.GetSystemSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateSystemSettings(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	latest, err := db.LoadTraeCNConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if latest != raw {
		t.Fatalf("config overwritten: %s err=%v", latest, err)
	}

	// 关回去同样要生效。
	if rec := put(`{"preflight_sse_passthrough":false}`); rec.Code != http.StatusOK {
		t.Fatalf("disable: %s", rec.Body.String())
	}
	if auth.TraeCNPreflightSSEPassthroughEnabled() {
		t.Fatal("disable did not take effect")
	}

	// 空对象、类型错误与未知字段都不能静默改配置。
	for _, bad := range []string{`{}`, `{"preflight_sse_passthrough":"yes"}`, `{"preflight_sse_passthrough":null}`} {
		if rec := put(bad); rec.Code != http.StatusBadRequest {
			t.Errorf("invalid request=%s status=%d body=%s", bad, rec.Code, rec.Body.String())
		}
	}
}
