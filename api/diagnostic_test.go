package api

import (
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/internal/diag"
	"github.com/gin-gonic/gin"
)

func TestDiagnosticMiddlewareCapturesPanicAndStreamingFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	t.Setenv("CODEX_DIAG_ENABLED", "true")
	t.Setenv("CODEX_DIAG_LOG_PATH", path)
	t.Setenv("CODEX_DIAG_REVISION", "")
	t.Setenv("LOG_DISABLED", "false")
	collector, err := diag.StartFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = collector.Close() })
	r := gin.New()
	r.Use(DiagnosticMiddleware(), RecoveryMiddleware(), RequestContextMiddleware())
	r.POST("/items/:id", func(c *gin.Context) {
		_ = c.Error(errors.New("local failure access_token=private-token"))
		c.Status(500)
	})
	r.GET("/panic", func(c *gin.Context) { panic("counter panic") })
	r.GET("/stream", func(c *gin.Context) { c.String(200, "event: error\n"); c.Set("x-access-log-status", 503) })
	r.GET("/ok", func(c *gin.Context) { c.Status(200) })
	r.GET("/bad-input", func(c *gin.Context) { c.Status(400) })
	for _, tc := range []struct {
		method, path string
		status       int
	}{{"POST", "/items/private-item?secret=query-secret", 500}, {"GET", "/panic", 500}, {"GET", "/stream", 200}, {"GET", "/ok", 200}, {"GET", "/bad-input", 400}} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"messages":["private-prompt"]}`))
		req.Header.Set("Authorization", "Bearer private-api-key")
		req.Header.Set("X-Request-ID", "request-7")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Fatalf("%s: response changed to %d", tc.path, w.Code)
		}
	}
	if err := collector.Close(); err != nil {
		t.Fatal(err)
	}
	scan, err := diag.ScanLogs(path, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Groups) != 3 {
		t.Fatalf("unexpected event groups (panic must appear once): %+v", scan)
	}
	seen := map[string]diag.Event{}
	for _, g := range scan.Groups {
		if g.Count != 1 {
			t.Fatal("duplicate diagnostic event")
		}
		seen[g.Sample.Route] = g.Sample
	}
	if seen["/stream"].Status != 503 {
		t.Fatal("streaming status override missing")
	}
	if seen["/panic"].Kind != "panic" || !strings.Contains(seen["/panic"].Stack, "api/diagnostic_test.go:") {
		t.Fatalf("panic stack missing: %+v", seen["/panic"])
	}
	if seen["/items/:id"].RequestID != "request-7" {
		t.Fatal("request correlation missing")
	}
	data, _ := os.ReadFile(path)
	for _, secret := range []string{"private-token", "private-item", "query-secret", "private-prompt", "private-api-key"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("sensitive request data leaked: %s", secret)
		}
	}
}
