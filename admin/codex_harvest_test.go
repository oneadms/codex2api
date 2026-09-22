package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/harvest"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestCodexHarvestAdminValidationAndPersistence(t *testing.T) {
	db := newTestAdminDB(t)
	repo := db.HarvestRepository()
	h := &Handler{codexHarvest: &proxy.CodexHarvestManager{DB: db, Store: &auth.Store{}, Repository: repo, Controls: harvest.NewCodexHarvestService(repo, repo, nil), DataDir: t.TempDir()}}
	router := gin.New()
	router.PUT("/scope", h.UpdateCodexHarvestScope)
	router.PUT("/controls", h.UpdateCodexHarvestControls)
	router.GET("/harvest", h.GetCodexHarvest)
	for _, tc := range []struct {
		path, body string
		status     int
	}{
		{"/scope", `{"mode":"selected","group_ids":[1,1],"account_policy":"schedulable_only"}`, 400},
		{"/scope", `{"mode":"selected","group_ids":[1],"account_policy":"schedulable_only","skipped_account_ids":[8]}`, 200},
		{"/controls", `{"version":1,"speed":{"round_interval_seconds":0}}`, 400},
		{"/controls", `{"version":1,"node_memory_enabled":true,"speed":{"round_interval_seconds":1,"probe_interval_seconds":0,"attempt_timeout_seconds":15,"cooldown_seconds":1,"max_requests_per_round":20,"max_node_attempts":5,"refresh_before_seconds":60}}`, 200},
	} {
		request := httptest.NewRequest(http.MethodPut, tc.path, strings.NewReader(tc.body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != tc.status {
			t.Fatalf("%s 状态 %d: %s", tc.path, response.Code, response.Body.String())
		}
	}
	scope, err := h.codexHarvest.Scope(t.Context())
	if err != nil || len(scope.SkippedAccountIDs) != 1 || scope.SkippedAccountIDs[0] != 8 {
		t.Fatal("账号跳过配置未保存")
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/harvest", nil))
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"max_requests_per_round":20`) {
		t.Fatalf("工作台快照错误: %s", response.Body.String())
	}
}
