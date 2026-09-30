package proxy

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/diag"
	"github.com/gin-gonic/gin"
)

func TestDiagnosticSchedulerAndUpstreamCorrelation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	t.Setenv("LOG_DISABLED", "false")
	t.Setenv("CODEX_DIAG_REVISION", "")
	collector, err := diag.StartManagedCollector(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = collector.Close() })
	r := gin.New()
	r.Use(api.DiagnosticMiddleware(), api.RequestContextMiddleware())
	r.POST("/v1/responses", func(c *gin.Context) {
		mode := c.GetHeader("X-Test-Mode")
		if mode == "upstream" || mode == "recovered" {
			h := &Handler{}
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID: 7, StatusCode: 503, AttemptIndex: 1, IsRetryAttempt: true,
				UpstreamErrorKind: "overloaded", ErrorMessage: "busy Bearer private-token",
			})
			status := 503
			if mode == "recovered" {
				status = 200
			}
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID: 8, StatusCode: status, AttemptIndex: 2,
				UpstreamRequestID: "upstream-2", UpstreamErrorKind: "overloaded",
			})
			c.JSON(status, gin.H{"error": gin.H{"code": "overloaded", "message": "busy"}})
			return
		}
		if strings.HasSuffix(mode, "-stream") {
			c.Header("Content-Type", "text/event-stream")
			_, _ = c.Writer.WriteString(": keepalive\n\n")
			c.Writer.Flush()
		}
		var selectionErr error = auth.ErrSchedulerQueueFull
		if strings.HasPrefix(mode, "timeout") {
			selectionErr = context.DeadlineExceeded
		}
		if !writeSchedulerQueueError(c, selectionErr, continuousRetryProtocolResponses) {
			t.Error("scheduler error not handled")
		}
	})
	for _, mode := range []string{"queue", "timeout", "queue-stream", "timeout-stream", "upstream", "recovered"} {
		req := httptest.NewRequest("POST", "/v1/responses", nil)
		req.Header.Set("X-Test-Mode", mode)
		req.Header.Set("X-Request-ID", mode)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		want := 503
		if mode == "recovered" || strings.HasSuffix(mode, "-stream") {
			want = 200
		}
		if w.Code != want {
			t.Fatalf("%s: status %d, want %d", mode, w.Code, want)
		}
	}
	if err := collector.Close(); err != nil {
		t.Fatal(err)
	}
	scan, err := diag.ScanLogs(path, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, group := range scan.Groups {
		total += group.Count
		e := group.Sample
		if e.Status != 503 || e.RequestID == "recovered" {
			t.Fatalf("incorrect failure recorded: %+v", e)
		}
		if e.RequestID == "upstream" {
			if e.Upstream == nil || e.Upstream.AccountID != 8 || e.Upstream.Attempt != 2 || e.Upstream.Retry || e.Upstream.RequestID != "upstream-2" {
				t.Fatalf("latest upstream attempt lost: %+v", e)
			}
		} else {
			want := "queue_full"
			if strings.HasPrefix(e.RequestID, "timeout") {
				want = "selection_timeout"
			}
			if e.SchedulerState != want || e.Message == "Service Unavailable" {
				t.Fatalf("scheduler cause lost: %+v", e)
			}
		}
	}
	if total != 5 {
		t.Fatalf("recorded %d failed requests, want 5", total)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-token") {
		t.Fatal("upstream credential leaked")
	}
}
