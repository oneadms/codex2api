package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestResinSettingsChannelPlatformsRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousRuntime, previousResin := proxy.CurrentRuntimeSettings(), proxy.GetResinConfig()
	previousEnabled, previousDecorator := auth.ResinEgressEnabled(), auth.ResinRequestResolver
	t.Cleanup(func() {
		proxy.ApplyRuntimeSettings(previousRuntime)
		proxy.SetResinConfig(previousResin)
		auth.SetResinEgressEnabled(previousEnabled)
		auth.ResinRequestResolver = previousDecorator
	})
	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	settings := defaultBootstrapSettings()
	if err := db.UpdateSystemSettings(t.Context(), settings); err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")

	for _, step := range []struct {
		body, mode, shared, codex, traecn, effectiveCodex, effectiveTraeCN string
	}{
		{body: `{"resin_url":"http://resin.test/token","resin_platform_name":"shared","resin_codex_platform_name":"codex1,codex2","resin_traecn_platform_name":"trae1,trae2"}`, shared: "shared", codex: "codex1,codex2", traecn: "trae1,trae2", effectiveCodex: "codex1,codex2", effectiveTraeCN: "trae1,trae2"},
		{body: `{"resin_traecn_platform_name":"trae3"}`, shared: "shared", codex: "codex1,codex2", traecn: "trae3", effectiveCodex: "codex1,codex2", effectiveTraeCN: "trae3"},
		{body: `{"site_name":"resin-test"}`, shared: "shared", codex: "codex1,codex2", traecn: "trae3", effectiveCodex: "codex1,codex2", effectiveTraeCN: "trae3"},
		{body: `{"resin_codex_platform_name":""}`, shared: "shared", traecn: "trae3", effectiveCodex: "shared", effectiveTraeCN: "trae3"},
		{body: `{"resin_platform_name":""}`, traecn: "trae3", effectiveTraeCN: "trae3"},
		{body: `{"resin_mode":"forward"}`, mode: "forward", traecn: "trae3", effectiveTraeCN: "trae3"},
		{body: `{"site_name":"keep-forward"}`, mode: "forward", traecn: "trae3", effectiveTraeCN: "trae3"},
		{body: `{"resin_mode":"reverse"}`, mode: "reverse", traecn: "trae3", effectiveTraeCN: "trae3"},
	} {
		mode := step.mode
		if mode == "" {
			mode = "reverse"
		}
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(step.body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		handler.UpdateSettings(ctx)
		if recorder.Code != http.StatusOK {
			t.Fatalf("PUT %s: %d %s", step.body, recorder.Code, recorder.Body)
		}
		for _, method := range []string{http.MethodPut, http.MethodGet} {
			if method == http.MethodGet {
				recorder = httptest.NewRecorder()
				ctx, _ = gin.CreateTestContext(recorder)
				ctx.Request = httptest.NewRequest(method, "/api/admin/settings", nil)
				handler.GetSettings(ctx)
			}
			var response settingsResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.ResinMode != mode {
				t.Fatalf("%s resin_mode=%s, want %s", method, response.ResinMode, mode)
			}
			if response.ResinPlatformName != step.shared || response.ResinCodexPlatformName != step.codex || response.ResinTraeCNPlatformName != step.traecn {
				t.Fatalf("%s settings=%q/%q/%q", method, response.ResinPlatformName, response.ResinCodexPlatformName, response.ResinTraeCNPlatformName)
			}
			if response.CodexEgress.ResinPlatformName != step.effectiveCodex || response.TraeCNEgress.ResinPlatformName != step.effectiveTraeCN {
				t.Fatalf("%s summaries=%+v/%+v", method, response.CodexEgress, response.TraeCNEgress)
			}
		}
		saved, err := db.GetSystemSettings(t.Context())
		if err != nil || saved.ResinMode != mode || saved.ResinCodexPlatformName != step.codex || saved.ResinTraeCNPlatformName != step.traecn {
			t.Fatalf("saved settings=%+v err=%v", saved, err)
		}
		// 测连必须读取已保存配置，即使当前实例的全局配置尚未同步。
		proxy.SetResinConfig(nil)
		for _, channel := range []struct{ kind, platforms string }{{platforms: step.effectiveCodex}, {kind: auth.UpstreamTraeCN, platforms: step.effectiveTraeCN}} {
			account := &auth.Account{DBID: 42, UpstreamType: channel.kind, AccessToken: "token"}
			requestCtx, err := handler.prepareCodexConnectionTestContext(context.Background(), account)
			if err != nil {
				t.Fatal(err)
			}
			if cfg := proxy.ResinConfigFromContext(requestCtx); cfg != nil && cfg.Mode != mode {
				t.Fatalf("测连模式=%q, want %q", cfg.Mode, mode)
			}
			want := strings.Split(channel.platforms, ",")[0]
			if got := proxy.ResinPlatformFromContext(requestCtx); got != want || proxy.IsResinEnabledForContext(requestCtx) != (want != "") {
				t.Fatalf("测连平台=%q, want %q", got, want)
			}
		}
		proxy.SetResinConfig(&proxy.ResinConfig{Mode: saved.ResinMode, BaseURL: saved.ResinURL, PlatformName: saved.ResinPlatformName, CodexPlatformName: saved.ResinCodexPlatformName, TraeCNPlatformName: saved.ResinTraeCNPlatformName})
	}

	for _, body := range []string{`{"resin_url":""}`, `{"resin_traecn_platform_name":""}`, `{"resin_mode":"invalid"}`, `{"resin_mode":"forward","resin_url":"http://resin.test/prefix/token"}`} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		handler.UpdateSettings(ctx)
		if recorder.Code != http.StatusBadRequest || proxy.CurrentTraeCNEgressSummary().ResinPlatformName != "trae3" {
			t.Fatalf("不完整配置不应生效: %d %s", recorder.Code, recorder.Body)
		}
	}
}
