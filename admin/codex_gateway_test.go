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

// newGatewayHandler builds a minimal handler without kicking off the harvest
// background loop; gateway handlers must not depend on it to serve reads.
func newGatewayHandler(t *testing.T) *Handler {
	t.Helper()
	db := newTestAdminDB(t)
	repo := db.HarvestRepository()
	return &Handler{codexHarvest: &proxy.CodexHarvestManager{DB: db, Store: &auth.Store{}, Repository: repo, Controls: harvest.NewCodexHarvestService(repo, repo, nil), DataDir: t.TempDir()}}
}

func TestCodexGatewaySettingsValidationAndPersistence(t *testing.T) {
	h := newGatewayHandler(t)
	router := gin.New()
	router.GET("/gateway", h.GetCodexGateway)
	router.PUT("/gateway", h.UpdateCodexGateway)
	router.GET("/status", h.GetCodexGatewayStatus)
	router.GET("/history", h.GetCodexGatewayHistory)

	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/gateway", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"revision":""`) {
		t.Fatalf("默认配置读取失败: %d %s", response.Code, response.Body.String())
	}

	// 来源与目标不重叠是保存前置条件；任何一项为空也不能启用。
	for _, tc := range []struct{ body string }{
		{`{"cookie_pool":{"enabled":true,"source_account_ids":[1],"target_account_ids":[1]}}`},
		{`{"cookie_pool":{"enabled":true,"source_account_ids":[],"target_account_ids":[2]}}`},
		{`{"cookie_pool":{"enabled":true,"source_account_ids":[1],"target_account_ids":[],"ttl_seconds":30}}`},
		{`{"cookie_pool":{"enabled":true,"source_account_ids":[1],"target_account_ids":[2],"ttl_seconds":10}}`},
		{`{"cookie_pool":{"enabled":true,"source_account_ids":[0],"target_account_ids":[2]}}`},
	} {
		request := httptest.NewRequest(http.MethodPut, "/gateway", strings.NewReader(tc.body))
		request.Header.Set("Content-Type", "application/json")
		response = httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("非法配置被接受(%d): %s %s", response.Code, tc.body, response.Body.String())
		}
	}

	// 无 Cookie 池时可单独保存调度配置。
	request := httptest.NewRequest(http.MethodPut, "/gateway", strings.NewReader(`{"cookie_pool":{"source_account_ids":[],"target_account_ids":[]},"ticket_pool_scheduling":true,"scheduling_mode":"groups","scheduling_group_ids":[5]}`))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("调度配置保存失败: %d %s", response.Code, response.Body.String())
	}
	first := response.Body.String()

	// 旧 revision 重复提交必须返回 409，不允许覆盖新配置。
	request = httptest.NewRequest(http.MethodPut, "/gateway", strings.NewReader(first))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("携带最新 revision 的保存应成功: %d %s", response.Code, response.Body.String())
	}
	second := response.Body.String()
	if second == first {
		t.Fatal("保存后 revision 未变化")
	}
	_ = second
	request = httptest.NewRequest(http.MethodPut, "/gateway", strings.NewReader(first))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("过期 revision 必须返回 409: %d %s", response.Code, response.Body.String())
	}

	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/status", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"mode":"disabled"`) {
		t.Fatalf("状态快照错误: %d %s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/history?limit=10", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"total":0`) {
		t.Fatalf("历史读取错误: %d %s", response.Code, response.Body.String())
	}
}

func TestCodexGatewayTestActionValidation(t *testing.T) {
	h := newGatewayHandler(t)
	router := gin.New()
	router.POST("/test", h.TestCodexGateway)
	for _, tc := range []struct{ body string }{
		{`{"action":"prepare"}`},
		{`{"action":"prepare","account_id":-1}`},
		{`{"action":"verify","account_id":1}`},
		{`{"action":"invalid","account_id":1}`},
	} {
		request := httptest.NewRequest(http.MethodPost, "/test", strings.NewReader(tc.body))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("诊断请求未被拒绝(%d): %s -> %s", response.Code, tc.body, response.Body.String())
		}
	}
}
