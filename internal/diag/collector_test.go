package diag

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCollectionRedactsGroupsAndToleratesIncompleteTail(t *testing.T) {
	name := filepath.Join(t.TempDir(), "diagnostics.jsonl")
	c, err := NewCollector(name, defaultLogLimit, strings.Repeat("a", 40))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		c.Record(Event{Kind: "upstream", Route: "/v1/responses?token=private-query", Status: 502, Model: "model-x", Message: fmt.Sprintf("request %d failed; Authorization: Bearer super-secret-token email=user@example.com access_token=another-secret https://user:pass@host.test/path?api_key=query-key", i)})
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"super-secret-token", "private-query", "user@example.com", "another-secret", "user:pass", "query-key"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("secret %q leaked in %s", secret, data)
		}
	}
	f, err := os.OpenFile(name, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("not-json\n{\"kind\":")
	_ = f.Close()
	scan, err := ScanLogs(name, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(scan.Groups) != 1 || scan.Groups[0].Count != 3 || scan.Malformed != 1 {
		t.Fatalf("unexpected aggregation: %+v", scan)
	}
	if scan.Groups[0].Sample.Revision != strings.Repeat("a", 40) {
		t.Fatal("missing revision")
	}
	info, _ := os.Stat(name)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("unsafe file mode: %v", info.Mode())
	}
	other := scan.Groups[0].Sample
	other.Status = 503
	if Fingerprint(other) == scan.Groups[0].Fingerprint {
		t.Fatal("different HTTP statuses were merged")
	}
}

func TestCollectorRotationAndConcurrentClose(t *testing.T) {
	name := filepath.Join(t.TempDir(), "events.jsonl")
	c, err := NewCollector(name, 1000, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		c.Record(Event{Kind: "panic", Status: 500, Message: strings.Repeat("x", 200)})
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{name, name + ".1"} {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) > 1000 {
			t.Fatalf("rotation exceeded limit: %d", len(data))
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if !json.Valid([]byte(line)) {
				t.Fatal("invalid JSON after rotation")
			}
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.Record(Event{Kind: "http", Status: 500})
			}
			_ = c.Close()
		}()
	}
	wg.Wait()
}

func TestStartRespectsOptInAndLogDisable(t *testing.T) {
	t.Setenv("CODEX_DIAG_LOG_PATH", filepath.Join(t.TempDir(), "diag.jsonl"))
	t.Setenv("CODEX_DIAG_ENABLED", "")
	c, err := StartFromEnv()
	if c != nil || err != nil || Enabled() {
		t.Fatal("collection enabled by default")
	}
	t.Setenv("CODEX_DIAG_ENABLED", "true")
	t.Setenv("LOG_DISABLED", "true")
	c, err = StartFromEnv()
	if c != nil || err != nil {
		t.Fatal("LOG_DISABLED was ignored")
	}
	t.Setenv("LOG_DISABLED", "false")
	t.Setenv("CODEX_DIAG_REVISION", "not-a-sha")
	if _, err = StartFromEnv(); err == nil {
		t.Fatal("invalid revision accepted")
	}
	t.Setenv("CODEX_DIAG_REVISION", strings.Repeat("b", 40))
	c, err = StartFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if !Enabled() {
		t.Fatal("collector not installed")
	}
	Record(Event{Kind: "http", Status: 500, Message: "failed"})
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	if Enabled() {
		t.Fatal("collector not removed on close")
	}
}

func TestUpstreamMessageOmitsPayloads(t *testing.T) {
	message := UpstreamMessage([]byte(`{"error":{"type":"upstream_error","message":"failed Bearer super-secret","code":"broken","prompt":"private-prompt"},"messages":["private-chat"]}`))
	if !strings.Contains(message, "broken") || strings.Contains(message, "super-secret") || strings.Contains(message, "private-") {
		t.Fatalf("bad envelope: %s", message)
	}
	if strings.Contains(UpstreamMessage([]byte("private HTML body")), "private") {
		t.Fatal("non-JSON body leaked")
	}
	if strings.Contains(UpstreamMessage([]byte(`{"message":"private body"}`)), "private") {
		t.Fatal("unknown envelope leaked")
	}
	if strings.Contains(UpstreamMessage([]byte(`{"error":{"message":"failed","code":{"prompt":"private payload"}}}`)), "private") {
		t.Fatal("nested error code leaked a payload")
	}
}

func TestScanDropsStaleEventsAndBoundsLines(t *testing.T) {
	name := filepath.Join(t.TempDir(), "events.jsonl")
	old, _ := json.Marshal(Event{Time: time.Now().Add(-48 * time.Hour), Kind: "http", Status: 500, Message: "old"})
	if err := os.WriteFile(name, append(old, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	scan, err := ScanLogs(name, time.Now().Add(-24*time.Hour))
	if err != nil || len(scan.Groups) != 0 {
		t.Fatalf("stale scan: %+v %v", scan, err)
	}
	if err := os.WriteFile(name, []byte(strings.Repeat("x", 70<<10)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ScanLogs(name, time.Time{}); err == nil {
		t.Fatal("oversized line accepted")
	}
}
