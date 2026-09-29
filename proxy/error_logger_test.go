package proxy

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/internal/diag"
)

func TestErrorLogDirUsesEnv(t *testing.T) {
	t.Setenv("LOG_DIR", "/tmp/codex2api-logs")
	if got, want := errorLogDir(), "/tmp/codex2api-logs"; got != want {
		t.Fatalf("errorLogDir() = %q, want %q", got, want)
	}
}

func TestUpstreamErrorsFeedDiagnosticCollector(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LOG_DIR", dir)
	t.Setenv("LOG_DISABLED", "false")
	t.Setenv("CODEX_DIAG_ENABLED", "true")
	t.Setenv("CODEX_DIAG_LOG_PATH", filepath.Join(dir, "diagnostics.jsonl"))
	t.Setenv("CODEX_DIAG_REVISION", "")
	collector, err := diag.StartFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	oldBad, oldServer := badRequestLogger, serverErrorLogger
	badRequestLogger = &fileLogger{path: "bad_request.log"}
	serverErrorLogger = &fileLogger{path: "server_error.log"}
	t.Cleanup(func() {
		_ = collector.Close()
		CloseErrorLogger()
		badRequestLogger, serverErrorLogger = oldBad, oldServer
	})
	for _, status := range []int{200, 400, 429, 502} {
		logUpstreamError("/v1/responses", status, "model", 42, []byte(`{"error":{"message":"provider failed api_key=private-key","type":"upstream"},"messages":["private-prompt"]}`))
	}
	if err := collector.Close(); err != nil {
		t.Fatal(err)
	}
	scan, err := diag.ScanLogs(filepath.Join(dir, "diagnostics.jsonl"), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Groups) != 2 {
		t.Fatalf("expected 400 and 502 diagnostics: %+v", scan)
	}
	for _, group := range scan.Groups {
		if group.Sample.Kind != "upstream" || strings.Contains(group.Sample.Message, "private-") {
			t.Fatalf("unsafe upstream event: %+v", group)
		}
	}
}
