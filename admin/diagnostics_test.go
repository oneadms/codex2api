package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/internal/diag"
	"github.com/gin-gonic/gin"
)

func TestDiagnosticAdminRoutesPersistAndProtectSettings(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("LOG_DISABLED", "false")
	t.Setenv("LOG_DIR", t.TempDir())
	t.Setenv("CODEX_DIAG_REVISION", "")
	db := newTestAdminDB(t)
	h := &Handler{db: db, adminSecretEnv: "test-admin-diagnostics"}
	root := t.TempDir()
	h.StartDiagnostics(t.Context(), root)
	t.Cleanup(h.StopDiagnostics)
	router := gin.New()
	h.RegisterRoutes(router)
	request := func(method, path string, body any, authenticated bool) *httptest.ResponseRecorder {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, "/api/admin/diagnostics/"+path, bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		if authenticated {
			req.Header.Set("X-Admin-Key", "test-admin-diagnostics")
		}
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	for _, route := range []struct{ method, path string }{
		{"GET", "settings"}, {"PUT", "settings"}, {"GET", "status"}, {"POST", "run"},
		{"GET", "history"}, {"GET", "incidents"}, {"GET", "runs/invalid"},
	} {
		if response := request(route.method, route.path, nil, false); response.Code != http.StatusUnauthorized {
			t.Fatalf("unprotected %s %s: %d", route.method, route.path, response.Code)
		}
	}
	settings := diag.DefaultServerConfig().Public()
	settings["enabled"] = true
	settings["model_url"] = "http://127.0.0.1:1/v1"
	settings["model"] = "fixture"
	settings["api_key"] = "private-model-key"
	settings["github_token"] = "private-github-token"
	response := request("PUT", "settings", settings, true)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "private-") {
		t.Fatalf("save did not mask credentials: %d %s", response.Code, response.Body.String())
	}
	settings["api_key"], settings["github_token"] = "", ""
	settings["min_count"] = 6
	response = request("PUT", "settings", settings, true)
	if response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	stored, err := db.LoadDiagnosticConfig(t.Context())
	if err != nil || stored.APIKey != "private-model-key" || stored.GitHubToken != "private-github-token" || stored.MinCount != 6 {
		t.Fatalf("blank fields did not preserve stored keys: %v", err)
	}
	settings["base_branch"] = "main"
	if response := request("PUT", "settings", settings, true); response.Code != http.StatusBadRequest {
		t.Fatalf("main accepted: %d", response.Code)
	}
	settings["base_branch"] = diag.RepairBaseBranch
	h.StopDiagnostics()
	h.diagnostics = nil
	h.StartDiagnostics(t.Context(), root)
	response = request("GET", "settings", nil, true)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"has_api_key":true`) || strings.Contains(response.Body.String(), "private-") || !h.diagnostics.Status().Collecting {
		t.Fatalf("restart lost persisted configuration: %d %s", response.Code, response.Body.String())
	}
	if response := request("POST", "run", nil, true); response.Code != http.StatusAccepted {
		t.Fatalf("manual run not accepted: %d %s", response.Code, response.Body.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	for h.diagnostics.Status().Running && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	status := h.diagnostics.Status()
	if status.Running || status.LastError != "" || status.LastFinished == nil {
		t.Fatalf("empty scan failed: %+v", status)
	}
	settings["enabled"], settings["clear_api_key"], settings["clear_github_token"] = false, true, true
	if response := request("PUT", "settings", settings, true); response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	stored, err = db.LoadDiagnosticConfig(t.Context())
	if err != nil || stored.APIKey != "" || stored.GitHubToken != "" || h.diagnostics.Status().Collecting {
		t.Fatal("clear credentials or disable collection failed")
	}
	if response := request("GET", "runs/invalid", nil, true); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid report id accepted: %d", response.Code)
	}
}
