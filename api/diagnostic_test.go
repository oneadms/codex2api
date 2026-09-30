package api

import (
	"errors"
	"net/http"
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

func TestDiagnosticFailureContextAndFingerprint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	t.Setenv("LOG_DISABLED", "false")
	t.Setenv("CODEX_DIAG_REVISION", "")
	collector, err := diag.StartManagedCollector(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = collector.Close() })
	r := gin.New()
	r.Use(DiagnosticMiddleware(), RequestContextMiddleware())
	r.POST("/v1/responses", func(c *gin.Context) {
		code := c.GetHeader("X-Test-Error")
		if code == "stream" {
			SetDiagnosticUpstream(c, diag.UpstreamAttempt{
				Status: 503, ErrorKind: "upstream_overloaded", AccountID: 42,
				RequestID: "upstream-request-9", Attempt: 2,
				Message: "provider busy access_token=private-upstream-key",
			})
			c.Header("Content-Type", "text/event-stream")
			c.String(200, "data: private-stream-content\n\n")
			c.Writer.Flush()
			SetDiagnosticError(c, NewAPIError("upstream_overloaded", "provider unavailable", ErrorTypeUpstream), 503)
			return
		}
		// Both causes have the same status and text: the code must split them.
		c.JSON(503, gin.H{
			"error":    gin.H{"code": code, "type": "server_error", "message": "Service unavailable api_key=private-key", "details": "private-details"},
			"messages": []string{"private-response"},
		})
	})
	for _, code := range []string{"no_available_account", "account_pool_concurrency_saturated", "stream"} {
		req := httptest.NewRequest("POST", "/v1/responses?token=private-query", strings.NewReader(`{"input":"private-prompt"}`))
		req.Header.Set("X-Test-Error", code)
		req.Header.Set("X-Request-ID", "request-"+code)
		req.Header.Set("Authorization", "Bearer private-client-key")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		want := 503
		if code == "stream" {
			want = 200
		}
		if w.Code != want {
			t.Fatalf("diagnostic writer changed response: %d, want %d", w.Code, want)
		}
		if code != "stream" && !strings.Contains(w.Body.String(), "private-response") {
			t.Fatal("response body changed")
		}
		if code == "stream" && !w.Flushed {
			t.Fatal("SSE flush lost")
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
		t.Fatalf("distinct failures were merged: %+v", scan)
	}
	for _, group := range scan.Groups {
		e := group.Sample
		if e.Status != 503 || e.ErrorCode == "" {
			t.Fatalf("missing error context: %+v", e)
		}
		if e.ErrorCode == "upstream_overloaded" {
			if e.RequestID != "request-stream" || e.Upstream == nil || e.Upstream.AccountID != 42 || e.Upstream.Attempt != 2 || e.Upstream.RequestID != "upstream-request-9" {
				t.Fatalf("upstream correlation lost: %+v", e)
			}
		} else if e.SchedulerState != e.ErrorCode || e.RequestID != "request-"+e.ErrorCode {
			t.Fatalf("scheduler correlation lost: %+v", e)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-") {
		t.Fatalf("sensitive data leaked: %s", data)
	}
}

func TestDiagnosticWriterBoundsAndSkipsBodies(t *testing.T) {
	for _, tc := range []struct {
		contentType string
		status      int
		body        string
	}{
		{"application/json", 503, strings.Repeat("x", diagnosticBodyLimit+1)},
		{"text/html", 503, "private-html-body"},
		{"application/json", 200, "private-success-body"},
		{"text/event-stream", 200, "data: private-stream-body\n\n"},
	} {
		raw := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(raw)
		w := &diagnosticResponseWriter{ResponseWriter: c.Writer}
		w.Header().Set("Content-Type", tc.contentType)
		w.WriteHeader(tc.status)
		if _, err := w.WriteString(tc.body); err != nil {
			t.Fatal(err)
		}
		if w.body.Len() != 0 {
			t.Fatal("unwanted response content retained")
		}
		if raw.Body.String() != tc.body || raw.Code != tc.status {
			t.Fatal("response changed")
		}
		if w.Unwrap() != c.Writer {
			t.Fatal("writer unwrap lost")
		}
	}
	// Even a valid JSON prefix is discarded once the capture limit is reached.
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	w := &diagnosticResponseWriter{ResponseWriter: c.Writer}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`{"error":{"code":"prefix"}}`))
	_, _ = w.Write([]byte(strings.Repeat(" ", diagnosticBodyLimit)))
	if w.body.Len() != 0 || !w.oversized {
		t.Fatal("oversized response prefix retained")
	}
}
