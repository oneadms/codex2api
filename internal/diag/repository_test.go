package diag

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := (Commands{}).Run(context.Background(), dir, "git", args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func fixtureRepository(t *testing.T) (repo, remote, sha string) {
	t.Helper()
	root := t.TempDir()
	repo = filepath.Join(root, "repo")
	remote = filepath.Join(root, "origin.git")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "init", "-b", RepairBaseBranch)
	for name, content := range map[string]string{
		"go.mod":           "module example.com/counter\n\ngo 1.26.6\n",
		"counter.go":       "package counter\n\nfunc Step(x int) int { return x - 1 }\n",
		"existing_test.go": "package counter\n\n// Existing tests must remain intact.\n",
		".env":             "VERY_PRIVATE_TOKEN=should-never-be-context\n",
	} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	gitTest(t, repo, "add", ".")
	gitTest(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-m", "fixture")
	sha = gitTest(t, repo, "rev-parse", "HEAD")
	gitTest(t, root, "init", "--bare", remote)
	gitTest(t, repo, "remote", "add", "origin", remote)
	gitTest(t, repo, "push", "origin", RepairBaseBranch)
	return
}

func validDiagnosis() Diagnosis {
	return Diagnosis{Title: "Increment counter", RootCause: "Step subtracts instead of adding one.", Confidence: 0.95, CanFix: true, Edits: []Edit{
		{Path: "counter.go", Old: "return x - 1", New: "return x + 1"},
		{Path: "step_regression_test.go", New: "package counter\n\nimport \"testing\"\n\nfunc TestStepRegression(t *testing.T) { if Step(1) != 2 { t.Fatal(\"wrong step\") } }\n"},
	}}
}

func TestWorkspaceRepairsImmutableSnapshotWithoutChangingCheckout(t *testing.T) {
	repo, _, sha := fixtureRepository(t)
	if err := os.WriteFile(filepath.Join(repo, "counter.go"), []byte("local uncommitted work\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before := gitTest(t, repo, "status", "--porcelain")
	w, err := NewWorkspace(context.Background(), Commands{}, repo, sha)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := w.Close(); err != nil {
			t.Error(err)
		}
	}()
	if strings.Contains(strings.Join(w.Inventory(), "\n"), ".env") {
		t.Fatal("secret file in inventory")
	}
	sources, err := w.Sources([]SourceRequest{{Path: "counter.go"}})
	if err != nil {
		t.Fatal(err)
	}
	diff, err := w.Apply(context.Background(), validDiagnosis(), sources)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "return x + 1") || !strings.Contains(diff, "step_regression_test.go") {
		t.Fatalf("missing repair diff: %s", diff)
	}
	if got := gitTest(t, repo, "status", "--porcelain"); got != before {
		t.Fatalf("working tree changed: %s -> %s", before, got)
	}
	if got := gitTest(t, repo, "branch", "--show-current"); got != RepairBaseBranch {
		t.Fatalf("checkout switched to %s", got)
	}
	if got := gitTest(t, repo, "rev-parse", "HEAD"); got != sha {
		t.Fatal("repair changed original HEAD")
	}
}

func TestWorkspaceRejectsUnsafeOrAmbiguousEdits(t *testing.T) {
	repo, _, sha := fixtureRepository(t)
	cases := []struct {
		name  string
		edits []Edit
	}{
		{"traversal", []Edit{{Path: "../escape.go", New: "package escape"}}},
		{"workflow", []Edit{{Path: ".github/workflows/ci.yml", New: "broken"}}},
		{"new-production", []Edit{{Path: "backdoor.go", New: "package counter"}}},
		{"existing-test", []Edit{{Path: "existing_test.go", Old: "Existing tests", New: "Deleted tests"}}},
		{"unknown-context", []Edit{{Path: "counter.go", Old: "not present", New: "replacement"}}},
		{"overlap", []Edit{{Path: "counter.go", Old: "return x - 1", New: "return x + 1"}, {Path: "counter.go", Old: "x - 1", New: "x + 2"}}},
		{"syntax", []Edit{{Path: "counter.go", Old: "return x - 1", New: "not valid Go $$$"}}},
		{"self-modification", []Edit{{Path: "internal/diag/event.go", Old: "anything", New: "anything else"}}},
		{"oversized", []Edit{{Path: "counter.go", Old: "return x - 1", New: strings.Repeat("x", 65<<10)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, err := NewWorkspace(context.Background(), Commands{}, repo, sha)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := w.Close(); err != nil {
					t.Error(err)
				}
			}()
			sources, err := w.Sources([]SourceRequest{{Path: "counter.go"}, {Path: "existing_test.go"}})
			if err != nil {
				t.Fatal(err)
			}
			d := validDiagnosis()
			d.Edits = tc.edits
			if _, err := w.Apply(context.Background(), d, sources); err == nil {
				t.Fatal("unsafe edit accepted")
			}
		})
	}
}

func TestInventoryRejectsSymlinks(t *testing.T) {
	repo, _, _ := fixtureRepository(t)
	if err := os.Symlink(".env", filepath.Join(repo, "leak.go")); err != nil {
		t.Fatal(err)
	}
	gitTest(t, repo, "add", "leak.go")
	gitTest(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-m", "symlink fixture")
	sha := gitTest(t, repo, "rev-parse", "HEAD")
	w, err := NewWorkspace(context.Background(), Commands{}, repo, sha)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if _, err = w.Sources([]SourceRequest{{Path: "leak.go"}}); err == nil {
		t.Fatal("symlink exposed secret")
	}
}

func TestLargeSourceExcerptUsesQuery(t *testing.T) {
	content := strings.Repeat("// filler\n", 4000) + "func BrokenStep() {}\n" + strings.Repeat("// trailer\n", 4000)
	got := excerpt(content, SourceRequest{Path: "counter.go", Query: "BrokenStep"})
	if !strings.Contains(got, "func BrokenStep()") || len(got) > 24<<10 || strings.Contains(got, strings.Repeat("// filler\n", 100)) {
		t.Fatal("excerpt failed to bound and select context")
	}
}

func TestFetchBasePreservesDeveloperFetchHead(t *testing.T) {
	repo, _, sha := fixtureRepository(t)
	gitTest(t, repo, "fetch", "origin", RepairBaseBranch)
	name := filepath.Join(repo, ".git", "FETCH_HEAD")
	before, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	got, err := FetchBase(context.Background(), Commands{}, repo, "origin", RepairBaseBranch)
	if err != nil || got != sha {
		t.Fatalf("wrong immutable baseline: %s %v", got, err)
	}
	after, err := os.ReadFile(name)
	if err != nil || string(before) != string(after) {
		t.Fatal("developer FETCH_HEAD was overwritten")
	}
	if refs := gitTest(t, repo, "for-each-ref", "refs/codex-diag/"); refs != "" {
		t.Fatalf("temporary fetch ref leaked: %s", refs)
	}
}
