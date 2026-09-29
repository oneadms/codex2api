package diag

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeAnalyzer struct {
	selects, diagnoses int
	failure            bool
	noFix              bool
}

func (a *fakeAnalyzer) Select(_ context.Context, _ Group, files []string) ([]SourceRequest, error) {
	a.selects++
	if a.failure {
		return nil, errors.New("model unavailable")
	}
	return []SourceRequest{{Path: "counter.go"}}, nil
}
func (a *fakeAnalyzer) Diagnose(_ context.Context, _ Group, sources []Source) (Diagnosis, error) {
	a.diagnoses++
	d := validDiagnosis()
	if a.noFix {
		d.CanFix = false
		d.Edits = nil
	}
	return d, nil
}

type fakeGitHub struct {
	calls          [][]string
	url            string
	createFailures int
	creates        int
	pushes         int
}

func (f *fakeGitHub) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	if name == "gh" {
		if len(args) > 1 && args[1] == "list" {
			if f.url != "" {
				return `[{"url":"` + f.url + `","state":"OPEN"}]`, nil
			}
			return "[]", nil
		}
		if len(args) > 1 && args[1] == "create" {
			f.creates++
			if f.createFailures > 0 {
				f.createFailures--
				return "", errors.New("simulated GitHub timeout")
			}
			f.url = "https://github.com/example/counter/pull/1"
			return f.url, nil
		}
		return "", errors.New("unexpected gh command")
	}
	if reflect.DeepEqual(args, []string{"remote", "get-url", "origin"}) {
		return "https://github.com/example/counter.git", nil
	}
	if len(args) > 0 && args[0] == "push" {
		f.pushes++
	}
	return (Commands{}).Run(ctx, dir, name, args...)
}

func fixtureWorker(t *testing.T) (*Worker, *fakeAnalyzer, *fakeGitHub, string) {
	t.Helper()
	repo, remote, _ := fixtureRepository(t)
	root := t.TempDir()
	c := DefaultWorkerConfig()
	c.Repo = repo
	c.LogPath = filepath.Join(root, "diagnostics.jsonl")
	c.StateDir = filepath.Join(root, "state")
	c.Cooldown = time.Nanosecond
	collector, err := NewCollector(c.LogPath, defaultLogLimit, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		collector.Record(Event{Kind: "http", Status: 500, Route: "/step", Message: "wrong counter"})
	}
	if err = collector.Close(); err != nil {
		t.Fatal(err)
	}
	a := &fakeAnalyzer{}
	ex := &fakeGitHub{}
	return &Worker{Config: c, Analyzer: a, Exec: ex}, a, ex, remote
}

func TestWorkerPreparePublishAndDeduplicate(t *testing.T) {
	w, a, ex, remote := fixtureWorker(t)
	ctx := context.Background()
	before := gitTest(t, w.Config.Repo, "rev-parse", "HEAD")
	result, err := w.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Outcomes) != 1 || result.Outcomes[0].Status != "prepared" {
		t.Fatalf("prepare failed: %+v", result)
	}
	outcome := result.Outcomes[0]
	if _, err = os.Stat(filepath.Join(outcome.Artifacts, "repair.patch")); err != nil {
		t.Fatal(err)
	}
	for _, call := range ex.calls {
		if call[0] == "gh" || (len(call) > 1 && call[1] == "push") {
			t.Fatalf("preview published: %v", call)
		}
	}
	result, err = w.Run(ctx)
	if err != nil || len(result.Outcomes) != 0 || a.selects != 1 {
		t.Fatalf("preview deduplication failed: %+v %v", result, err)
	}
	w.Config.Publish = true
	result, err = w.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Outcomes) != 1 || result.Outcomes[0].Status != "published" || result.Outcomes[0].PRURL == "" {
		t.Fatalf("publish failed: %+v", result)
	}
	if a.selects != 1 || a.diagnoses != 1 {
		t.Fatal("publishing a prepared repair called the model again")
	}
	if ex.pushes != 1 || ex.creates != 1 {
		t.Fatalf("unexpected publication calls: %+v", ex)
	}
	for _, call := range ex.calls {
		for i, arg := range call {
			if arg == "--base" && (i+1 >= len(call) || call[i+1] != RepairBaseBranch) {
				t.Fatalf("wrong PR base: %v", call)
			}
			if arg == "push" && !strings.HasPrefix(call[len(call)-1], "HEAD:refs/heads/codex/autofix/") {
				t.Fatalf("unsafe push: %v", call)
			}
		}
	}
	if got := gitTest(t, remote, "rev-parse", "refs/heads/"+RepairBaseBranch); got != before {
		t.Fatal("publish modified custom/main directly")
	}
	if got := gitTest(t, w.Config.Repo, "rev-parse", "HEAD"); got != before {
		t.Fatal("publish changed local HEAD")
	}
	result, err = w.Run(ctx)
	if err != nil || len(result.Outcomes) != 0 || ex.creates != 1 {
		t.Fatalf("publish dedup failed: %+v %v", result, err)
	}
	if got := gitTest(t, w.Config.Repo, "worktree", "list", "--porcelain"); strings.Count(got, "worktree ") != 1 {
		t.Fatalf("leaked worktree: %s", got)
	}
}

func TestWorkerRecoversAfterPushAndPRFailure(t *testing.T) {
	w, a, ex, _ := fixtureWorker(t)
	w.Config.Publish = true
	ex.createFailures = 1
	first, err := w.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Outcomes) != 1 || first.Outcomes[0].Status != "failed" || ex.pushes != 1 {
		t.Fatalf("expected push then failure: %+v", first)
	}
	second, err := w.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Outcomes) != 1 || second.Outcomes[0].Status != "published" || ex.pushes != 1 || a.selects != 1 {
		t.Fatalf("recovery regenerated or repushed: %+v calls=%+v", second, ex)
	}
	if ex.creates != 2 {
		t.Fatalf("PR was not retried: %d", ex.creates)
	}
}

func TestWorkerLimitsFailedAttemptsAndSkipsOperationalIssues(t *testing.T) {
	w, a, _, _ := fixtureWorker(t)
	a.failure = true
	w.Config.MaxAttempts = 2
	for i := 0; i < 2; i++ {
		r, err := w.Run(context.Background())
		if err != nil || len(r.Outcomes) != 1 || r.Outcomes[0].Status != "failed" {
			t.Fatalf("failure not recorded: %+v %v", r, err)
		}
	}
	r, err := w.Run(context.Background())
	if err != nil || len(r.Outcomes) != 0 || a.selects != 2 {
		t.Fatalf("attempt limit not enforced: %+v %v", r, err)
	}
	w, a, _, _ = fixtureWorker(t)
	a.noFix = true
	r, err = w.Run(context.Background())
	if err != nil || len(r.Outcomes) != 1 || r.Outcomes[0].Status != "no_fix" {
		t.Fatalf("operational issue not stopped: %+v %v", r, err)
	}
	if _, err = os.Stat(filepath.Join(r.Outcomes[0].Artifacts, "repair.patch")); !os.IsNotExist(err) {
		t.Fatal("non-fix generated patch")
	}
}

func TestWorkerRefusesMainBeforeAnyExternalCommands(t *testing.T) {
	w, _, ex, _ := fixtureWorker(t)
	w.Config.Base = "main"
	w.Config.Publish = true
	if _, err := w.Run(context.Background()); err == nil || len(ex.calls) != 0 {
		t.Fatalf("main was accepted: %v calls=%v", err, ex.calls)
	}
	if _, err := FetchBase(context.Background(), ex, w.Config.Repo, "origin", "main"); err == nil || len(ex.calls) != 0 {
		t.Fatal("fetch bypassed main protection")
	}
	p := Publisher{Exec: ex, Repo: w.Config.Repo, Base: "main", GitHub: "example/counter"}
	if _, err := p.Existing(context.Background(), "codex/autofix/aaaaaaaaaaaa-bbbbbbbbbbbb"); err == nil || len(ex.calls) != 0 {
		t.Fatal("publisher bypassed main protection")
	}
}

func TestWorkerRejectsCorruptStateAndConcurrentLock(t *testing.T) {
	w, a, _, _ := fixtureWorker(t)
	if err := os.MkdirAll(w.Config.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(w.Config.StateDir, "state.json")
	if err := os.WriteFile(state, []byte("broken state"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Run(context.Background()); err == nil || a.selects != 0 {
		t.Fatal("corrupt state was ignored")
	}
	data, _ := os.ReadFile(state)
	if string(data) != "broken state" {
		t.Fatal("corrupt state overwritten")
	}
	if err := os.Mkdir(filepath.Join(w.Config.StateDir, ".lock"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "lock") {
		t.Fatal("concurrent lock ignored")
	}
}

func TestGitHubRepositoryValidation(t *testing.T) {
	for _, remote := range []string{"https://github.com/oneadms/codex2api.git", "git@github.com:oneadms/codex2api.git", "ssh://git@github.com/oneadms/codex2api.git"} {
		got, err := GitHubRepository(remote)
		if err != nil || got != "oneadms/codex2api" {
			t.Fatalf("parse %s: %s %v", remote, got, err)
		}
	}
	for _, remote := range []string{"https://token@github.com/a/b", "https://evil.example/a/b", "https://github.com/a/b?token=secret", "https://github.com/a/../../b"} {
		if _, err := GitHubRepository(remote); err == nil {
			t.Fatalf("unsafe remote accepted: %s", remote)
		}
	}
}

func TestWorkerWithHTTPAnalyzerEndToEnd(t *testing.T) {
	w, _, _, _ := fixtureWorker(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			_, _ = rw.Write([]byte(modelEnvelope(`{"files":[{"path":"counter.go"}]}`, "stop")))
			return
		}
		payload, _ := json.Marshal(validDiagnosis())
		_, _ = rw.Write([]byte(modelEnvelope(string(payload), "stop")))
	}))
	defer server.Close()
	w.Analyzer = &ChatAnalyzer{URL: server.URL, Model: "fixture-model"}
	result, err := w.Run(context.Background())
	if err != nil || len(result.Outcomes) != 1 || result.Outcomes[0].Status != "prepared" || requests != 2 {
		t.Fatalf("HTTP model integration failed: %+v %v calls=%d", result, err, requests)
	}
}

type matchingPRExecutor struct{ calls int }

func (e *matchingPRExecutor) Run(_ context.Context, _ string, name string, args ...string) (string, error) {
	if name != "gh" {
		return "", errors.New("unexpected non-GitHub command")
	}
	e.calls++
	for _, arg := range args {
		if arg == "--head" {
			return "[]", nil
		}
	}
	return `[{"url":"https://github.com/example/counter/pull/8","state":"OPEN"}]`, nil
}

func TestPublisherFindsIncidentPRAfterBaseChanges(t *testing.T) {
	ex := &matchingPRExecutor{}
	p := Publisher{Exec: ex, Base: RepairBaseBranch, GitHub: "example/counter"}
	url, err := p.Existing(context.Background(), "codex/autofix/aaaaaaaaaaaa-bbbbbbbbbbbb")
	if err != nil || url != "https://github.com/example/counter/pull/8" || ex.calls != 2 {
		t.Fatalf("open incident PR not reused: %q %v calls=%d", url, err, ex.calls)
	}
}
