package proxy

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestTraeCNDiagnosticCapturesThreeStages(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	t.Setenv("TRAECN_DIAGNOSTIC_DIR", dir)
	t.Setenv("TRAECN_DIAGNOSTIC_REQUESTS", "1")
	t.Setenv("TRAECN_DIAGNOSTIC_RAW", "1")
	// Keep secrets long enough that replacing them cannot obscure unrelated field names.
	secret := "diagnostic-secret-token-should-not-appear"
	var wire []byte
	provider := "event: output\ndata: {\"response\":\"接下来检查……\",\"tool_calls\":[],\"message\":{\"tool_calls\":[{\"id\":\"call_read\",\"function_call\":{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"README.md\\\"}\"}}]}}\n\nevent: done\ndata: {}\n\n"
	handler := newTraeCNContextTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		wire, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, provider)
	})
	request := map[string]any{"model": "doubao-seed-code", "stream": true, "input": "inspect README.md", "access_token": secret, "tools": []any{map[string]any{"type": "function", "name": "read_file", "parameters": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}}}}
	recorder := invokeTraeCNContextTestRequest(t, handler, 91234, database.UpstreamChannelTraeCN, request)
	completed, ok := findCanonicalEvent(canonicalSSEEvents(t, recorder.Body.Bytes()), "response.completed")
	if !ok || completed.Get("response.output.#(type==\"function_call\").name").String() != "read_file" {
		t.Fatal("expected executable read_file")
	}
	dirs, _ := filepath.Glob(filepath.Join(dir, "traecn-*"))
	if len(dirs) != 1 {
		t.Fatalf("capture directories=%d", len(dirs))
	}
	capture := dirs[0]
	for _, file := range []string{"inbound.json", "canonical.json", "outbound.json", "upstream-0.sse", "responses.sse", "metadata.jsonl"} {
		raw, err := os.ReadFile(filepath.Join(capture, file))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), secret) {
			t.Fatalf("credential leaked in %s", file)
		}
		stat, _ := os.Stat(filepath.Join(capture, file))
		if stat.Mode().Perm() != 0600 {
			t.Fatalf("permissions for %s: %v", file, stat.Mode())
		}
		switch file {
		case "inbound.json":
			if gjson.GetBytes(raw, "model").String() != "doubao-seed-code" || gjson.GetBytes(raw, "input").String() != "inspect README.md" {
				t.Fatal("ingress snapshot is not original input")
			}
		case "outbound.json":
			if string(raw) != string(wire) {
				t.Fatal("capture differs from actual outbound body")
			}
			if gjson.GetBytes(raw, "tools.0.function.parameters").Type != gjson.String {
				t.Fatal("Trae schema must remain encoded once")
			}
		case "upstream-0.sse":
			if string(raw) != provider {
				t.Fatal("raw SSE was transformed before capture")
			}
		case "responses.sse":
			if !strings.Contains(string(raw), "response.function_call_arguments.done") {
				t.Fatal("missing executable event capture")
			}
		case "metadata.jsonl":
			for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
				if gjson.Get(line, "request_id").String() != strings.TrimPrefix(filepath.Base(capture), "traecn-") {
					t.Fatal("uncorrelated diagnostic")
				}
			}
			if !strings.Contains(string(raw), "gateway_default") || !strings.Contains(string(raw), "message.tool_calls") {
				t.Fatal("missing raw source or parser evidence")
			}
		}
	}
}

func TestTraeCNDiagnosticOptInBoundsAndRedaction(t *testing.T) {
	t.Setenv("TRAECN_DIAGNOSTIC_DIR", t.TempDir())
	t.Setenv("TRAECN_DIAGNOSTIC_REQUESTS", "0")
	off := newTraeCNDiagnostic(context.Background(), "off", []byte("{}"), nil)
	if off.dir != "" {
		t.Fatal("diagnostics enabled without a quota")
	}
	t.Setenv("TRAECN_DIAGNOSTIC_REQUESTS", "2")
	t.Setenv("TRAECN_DIAGNOSTIC_MAX_BYTES", "64")
	t.Setenv("TRAECN_DIAGNOSTIC_RAW", "0")
	meta := newTraeCNDiagnostic(context.Background(), "metadata", []byte("{\"input\":\"private prompt\"}"), nil)
	meta.artifact("not-written", []byte("private prompt"), 14)
	files, _ := os.ReadDir(meta.dir)
	if len(files) != 1 || files[0].Name() != "metadata.jsonl" {
		t.Fatal("raw body captured without RAW=1")
	}
	bytes, _ := os.ReadFile(filepath.Join(meta.dir, "metadata.jsonl"))
	if strings.Contains(string(bytes), "private prompt") {
		t.Fatal("prompt leaked into metadata")
	}
	t.Setenv("TRAECN_DIAGNOSTIC_RAW", "1")
	d := newTraeCNDiagnostic(context.Background(), "bounded", []byte("{}"), http.Header{"Authorization": []string{"Bearer hidden-secret-value"}})
	d.artifact("oversized", []byte(strings.Repeat("x", 1000)), 1000)
	stat, _ := os.Stat(filepath.Join(d.dir, "oversized"))
	if stat.Size() > 64 {
		t.Fatal("artifact exceeded cap")
	}
	if _, err := os.Stat(filepath.Join(d.dir, "oversized.truncated")); err != nil {
		t.Fatal("missing truncation marker")
	}
	d.artifact("echo", []byte("data: hidden-secret-value"), 25)
	safe, _ := os.ReadFile(filepath.Join(d.dir, "echo"))
	if strings.Contains(string(safe), "hidden-secret") {
		t.Fatal("header credential echoed into artifact")
	}
	for i := 0; i < 30000; i++ {
		d.record("test", map[string]any{"x": i})
	}
	stat, _ = os.Stat(filepath.Join(d.dir, "metadata.jsonl"))
	if stat.Size() > 256<<10 {
		t.Fatal("metadata exceeded cap")
	}
	d.record("terminal", map[string]any{"finish_reason": "stop", "finish_reason_source": traeCNFinishReasonDefaulted})
	terminal, err := os.ReadFile(filepath.Join(d.dir, "terminal.json"))
	if err != nil || gjson.GetBytes(terminal, "data.finish_reason_source").String() != traeCNFinishReasonDefaulted {
		t.Fatal("terminal provenance lost after metadata cap")
	}
	if next := newTraeCNDiagnostic(context.Background(), "quota", nil, nil); next.dir != "" {
		t.Fatal("capture quota not enforced")
	}
}

func TestTraeCNDiagnosticReaderDoesNotChangeStream(t *testing.T) {
	t.Setenv("TRAECN_DIAGNOSTIC_DIR", t.TempDir())
	t.Setenv("TRAECN_DIAGNOSTIC_REQUESTS", "1")
	t.Setenv("TRAECN_DIAGNOSTIC_RAW", "1")
	d := newTraeCNDiagnostic(context.Background(), "reader", nil, nil, "split-secret-value")
	original := "data: {\"content\":\"split-secret-value\"}\n\n"
	source := d.reader(io.NopCloser(strings.NewReader(original)), "test.sse")
	var got strings.Builder
	small := make([]byte, 2)
	for {
		n, err := source.Read(small)
		got.Write(small[:n])
		if err != nil {
			if err != io.EOF {
				t.Fatal(err)
			}
			break
		}
	}
	source.Close()
	if got.String() != original {
		t.Fatal("diagnostic modified the delivered stream")
	}
	captured, err := os.ReadFile(filepath.Join(d.dir, "test.sse"))
	if err != nil || strings.Contains(string(captured), "split-secret") || !strings.Contains(string(captured), "[REDACTED]") {
		t.Fatal("fragmented credential was not redacted")
	}
}
