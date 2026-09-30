package diag

import (
	"encoding/json"
	"os"
	"os/exec"
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

func TestCodexChildGoEnvironmentIsOffline(t *testing.T) {
	t.Setenv("GOMODCACHE", "/bundled/go-mod")
	t.Setenv("GOCACHE", "/writable/go-build")
	t.Setenv("GOPROXY", "https://private-proxy.invalid")
	t.Setenv("GOTOOLCHAIN", "auto")
	t.Setenv("GITHUB_TOKEN", "private-github-token")
	t.Setenv("DATABASE_URL", "private-database-url")
	env := codexChildEnvironment()
	for key, want := range map[string]string{
		"GOMODCACHE": "/bundled/go-mod", "GOCACHE": "/writable/go-build",
		"GOPROXY": "off", "GOSUMDB": "off", "GOTOOLCHAIN": "local",
		"GOENV": "off", "CGO_ENABLED": "0", "GOFLAGS": "-mod=readonly",
	} {
		if env[key] != want {
			t.Fatalf("%s = %q, want %q", key, env[key], want)
		}
	}
	for _, key := range []string{"GITHUB_TOKEN", "DATABASE_URL"} {
		if _, ok := env[key]; ok {
			t.Fatalf("service secret %s passed to Codex", key)
		}
	}
}

func TestCodexMissingGoFailsBeforeModelInvocation(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	runner := CodexSDKRunner{BaseURL: "http://localhost:8080/v1", Model: "model"}
	_, err := runner.Repair(t.Context(), t.TempDir(), "diagnose")
	if err == nil || !strings.Contains(err.Error(), "缺少 Go") {
		t.Fatalf("missing toolchain did not produce actionable error: %v", err)
	}
}

func TestCodexChildCanRunOfflineGoTest(t *testing.T) {
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, content := range map[string]string{
		"go.mod":          "module diag-offline-test\n\ngo 1.20\n",
		"offline_test.go": "package offline\nimport \"testing\"\nfunc TestOffline(t *testing.T) { if 2+2 != 4 { t.Fatal(\"bad result\") } }\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(t.Context(), goPath, "test", "./...")
	cmd.Dir = dir
	for key, value := range codexChildEnvironment() {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("offline Go test failed: %s: %v", out, err)
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
