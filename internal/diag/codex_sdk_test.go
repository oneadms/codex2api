package diag

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexSDKRunnerInvokesProjectEndpoint(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-node")
	content := "#!/bin/sh\ncat > \"$DIAG_SDK_CAPTURE\"\nprintf '%s' '{\"title\":\"Fix counter\",\"root_cause\":\"Boundary check is missing.\",\"confidence\":0.91,\"can_fix\":true}'\n"
	if err := os.WriteFile(script, []byte(content), 0700); err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(dir, "input.json")
	t.Setenv("DIAG_SDK_CAPTURE", capture)
	runner := CodexSDKRunner{NodePath: script, Script: script, BaseURL: "https://codex.example.test/v1", APIKey: "project-api-key", Model: "gpt-5.5"}
	got, err := runner.Repair(t.Context(), dir, "repair the incident")
	if err != nil {
		t.Fatal(err)
	}
	if !got.CanFix || got.Confidence != 0.91 {
		t.Fatalf("unexpected result: %+v", got)
	}
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err = json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["baseUrl"] != "https://codex.example.test/v1" || payload["apiKey"] != "project-api-key" || payload["model"] != "gpt-5.5" {
		t.Fatalf("SDK payload mismatch: %s", raw)
	}
	if err = (CodexSDKRunner{BaseURL: "https://codex.example.test/v1/chat/completions", Model: "gpt-5.5", APIKey: "key"}).Validate(); err == nil {
		t.Fatal("chat completions URL accepted")
	}
}

func TestCodexWorkspaceDiffRejectsProtectedPaths(t *testing.T) {
	repo, _, sha := fixtureRepository(t)
	ws, err := NewWorkspace(t.Context(), Commands{}, repo, sha)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	if err = os.WriteFile(filepath.Join(ws.Root, "counter.go"), []byte("package counter\n\nfunc Step(x int) int { return x + 1 }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	diff, err := (Commands{}).Run(t.Context(), ws.Root, "git", "diff", "--binary", "HEAD")
	if err != nil || !strings.Contains(diff, "x + 1") {
		t.Fatal(err)
	}
	if err = validateCodexDiff(t.Context(), Commands{}, ws, diff); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(ws.Root, ".env"), []byte("TOKEN=changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = validateCodexDiff(t.Context(), Commands{}, ws, diff); err == nil {
		t.Fatal("protected file change accepted")
	}
}
