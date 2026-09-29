package diag

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type memoryConfigStore struct {
	mu   sync.Mutex
	cfg  ServerConfig
	fail bool
}

func (s *memoryConfigStore) LoadDiagnosticConfig(context.Context) (ServerConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg, nil
}
func (s *memoryConfigStore) SaveDiagnosticConfig(_ context.Context, cfg ServerConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("database unavailable")
	}
	s.cfg = cfg
	return nil
}

func TestManagedConfigAppliesPersistsAndRestarts(t *testing.T) {
	t.Setenv("LOG_DISABLED", "false")
	t.Setenv("CODEX_DIAG_REVISION", "")
	store := &memoryConfigStore{cfg: DefaultServerConfig()}
	root := t.TempDir()
	m := NewManager(store, root)
	m.Start(t.Context())
	t.Cleanup(m.Close)
	if m.Status().Collecting {
		t.Fatal("collector enabled by default")
	}
	cfg := m.Config()
	cfg.Enabled = true
	cfg.APIKey = "private-model-key"
	cfg.GitHubToken = "private-github-key"
	if err := m.Update(t.Context(), cfg, false, false); err != nil {
		t.Fatal(err)
	}
	if !m.Status().Collecting || !Enabled() {
		t.Fatal("UI update did not start collection")
	}
	public, _ := json.Marshal(m.Config().Public())
	if strings.Contains(string(public), "private-") {
		t.Fatal("settings expose credentials")
	}
	cfg.APIKey = ""
	cfg.GitHubToken = ""
	cfg.MinCount = 8
	if err := m.Update(t.Context(), cfg, false, false); err != nil {
		t.Fatal(err)
	}
	if m.Config().APIKey != "private-model-key" || m.Config().GitHubToken != "private-github-key" {
		t.Fatal("blank fields erased existing keys")
	}
	Record(Event{Kind: "http", Status: 500, Message: "test error"})
	m.Close()
	m2 := NewManager(store, root)
	m2.Start(t.Context())
	t.Cleanup(m2.Close)
	if !m2.Status().Collecting || m2.Config().MinCount != 8 {
		t.Fatal("settings did not survive restart")
	}
	scan, err := m2.Incidents()
	if err != nil || len(scan.Groups) != 1 {
		t.Fatalf("persisted events missing: %+v %v", scan, err)
	}
	cfg = m2.Config()
	cfg.Enabled = false
	cfg.AutoRun = false
	if err := m2.Update(t.Context(), cfg, true, true); err != nil {
		t.Fatal(err)
	}
	if Enabled() || m2.Status().Collecting || m2.Config().APIKey != "" || m2.Config().GitHubToken != "" {
		t.Fatal("disable or secret deletion not applied")
	}
}

func TestManagedRunRejectsConcurrentAndCancelsOnDisable(t *testing.T) {
	t.Setenv("LOG_DISABLED", "false")
	t.Setenv("CODEX_DIAG_REVISION", "")
	store := &memoryConfigStore{cfg: DefaultServerConfig()}
	m := NewManager(store, t.TempDir())
	started := make(chan struct{})
	stopped := make(chan struct{})
	m.execute = func(ctx context.Context, _ ServerConfig) (RunResult, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return RunResult{}, ctx.Err()
	}
	m.Start(t.Context())
	t.Cleanup(m.Close)
	cfg := m.Config()
	cfg.Enabled = true
	cfg.ModelURL = "http://127.0.0.1/v1"
	cfg.Model = "test"
	cfg.APIKey = "test-key"
	if err := m.Update(t.Context(), cfg, false, false); err != nil {
		t.Fatal(err)
	}
	if err := m.RunNow(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("run did not start")
	}
	if err := m.RunNow(); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("concurrent scan accepted: %v", err)
	}
	cfg.Enabled = false
	if err := m.Update(t.Context(), cfg, false, false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("disable did not cancel active run")
	}
}

func TestManagedSaveFailureDoesNotEnableCollector(t *testing.T) {
	t.Setenv("LOG_DISABLED", "false")
	t.Setenv("CODEX_DIAG_REVISION", "")
	store := &memoryConfigStore{cfg: DefaultServerConfig(), fail: true}
	m := NewManager(store, t.TempDir())
	m.Start(t.Context())
	t.Cleanup(m.Close)
	cfg := m.Config()
	cfg.Enabled = true
	if err := m.Update(t.Context(), cfg, false, false); err == nil {
		t.Fatal("store error ignored")
	}
	if Enabled() || m.Status().Collecting || m.Config().Enabled {
		t.Fatal("failed save changed running configuration")
	}
}

func TestManagedEmptyScanNeedsNoCloneOrModelRequest(t *testing.T) {
	t.Setenv("LOG_DISABLED", "false")
	t.Setenv("CODEX_DIAG_REVISION", "")
	store := &memoryConfigStore{cfg: DefaultServerConfig()}
	m := NewManager(store, t.TempDir())
	m.Start(t.Context())
	t.Cleanup(m.Close)
	cfg := m.Config()
	cfg.Enabled = true
	cfg.ModelURL = "http://127.0.0.1:1/v1"
	cfg.Model = "test"
	if err := m.Update(t.Context(), cfg, false, false); err != nil {
		t.Fatal(err)
	}
	result, err := m.managedRun(t.Context(), cfg)
	if err != nil || len(result.Outcomes) != 0 {
		t.Fatalf("empty scan called external services: %+v %v", result, err)
	}
	if _, err = os.Stat(m.repoRoot(cfg)); !os.IsNotExist(err) {
		t.Fatal("empty scan created a repository")
	}
	if _, _, err = m.Report("../../private"); err == nil {
		t.Fatal("report path traversal accepted")
	}
}

func TestManagedConfigRejectsUnsafeSettings(t *testing.T) {
	for _, repository := range []string{"https://github.com/a/b", "../repo", "a/../b", "a/b.git", "a/b\n"} {
		cfg := DefaultServerConfig()
		cfg.Repository = repository
		if err := cfg.Validate(); err == nil {
			t.Fatalf("unsafe repository accepted: %q", repository)
		}
	}
	cfg := DefaultServerConfig()
	cfg.AutoRun = true
	if err := cfg.Validate(); err == nil {
		t.Fatal("automatic scan without collection accepted")
	}
	cfg.Enabled = true
	cfg.ModelURL = "http://127.0.0.1/v1"
	cfg.Model = "test"
	cfg.APIKey = "test-key"
	cfg.Publish = true
	if err := cfg.Validate(); err == nil {
		t.Fatal("publication without GitHub token accepted")
	}
	cfg.GitHubToken = "token"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestCommandsUseConfiguredTokenWithoutGlobalEnvMutation(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("POSIX executable fixture")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n[ -z \"$DIAG_LLM_API_KEY\" ] || exit 9\nprintf '%s' \"$GH_TOKEN\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GH_TOKEN", "inherited-token")
	t.Setenv("DIAG_LLM_API_KEY", "unrelated-secret")
	got, err := (Commands{GitHubToken: "ui-token"}).Run(t.Context(), dir, "gh", "--version")
	if err != nil || got != "ui-token" || os.Getenv("GH_TOKEN") != "inherited-token" {
		t.Fatalf("token isolation failed: %q %v", got, err)
	}
}
