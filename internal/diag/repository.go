package diag

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"go/format"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Executor interface {
	Run(context.Context, string, string, ...string) (string, error)
}

type Commands struct{}

type limitedOutput struct{ bytes.Buffer }

func (b *limitedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := (2 << 20) - b.Len(); remaining > 0 {
		_, _ = b.Buffer.Write(p[:min(remaining, len(p))])
	}
	return n, nil
}

func (Commands) Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if name == "git" {
		args = append([]string{"-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false", "-c", "commit.gpgsign=false"}, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.WaitDelay = 2 * time.Second
	// Git/gh need credentials, but no LLM keys, database DSNs or service secrets.
	for _, key := range []string{"PATH", "HOME", "USER", "TMPDIR", "SYSTEMROOT", "SSH_AUTH_SOCK", "XDG_CONFIG_HOME", "GH_CONFIG_DIR", "GH_TOKEN", "GITHUB_TOKEN"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GH_PROMPT_DISABLED=1")
	var stdout, stderr limitedOutput
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s failed: %w: %s", name, err, bounded(SafeText(stderr.String()), 2000))
	}
	if stdout.Len() >= 2<<20 {
		return "", fmt.Errorf("%s output exceeds 2 MiB", name)
	}
	return strings.TrimSuffix(stdout.String(), "\n"), nil
}

type Workspace struct {
	Root    string
	Repo    string
	BaseSHA string
	Exec    Executor
	files   map[string]bool
	parent  string
}

// RepairBaseBranch is the fork's only repair destination. main tracks upstream.
const RepairBaseBranch = "custom/main"

func FetchBase(ctx context.Context, ex Executor, repo, remote, branch string) (string, error) {
	if branch != RepairBaseBranch {
		return "", errors.New("repairs must target custom/main; main is reserved for upstream updates")
	}
	if remote == "" || strings.HasPrefix(remote, "-") || strings.ContainsAny(remote, " /:\\") {
		return "", errors.New("invalid remote name")
	}
	if branch == "" || strings.HasPrefix(branch, "-") {
		return "", errors.New("an explicit --base branch is required")
	}
	if _, err := ex.Run(ctx, repo, "git", "check-ref-format", "refs/heads/"+branch); err != nil {
		return "", err
	}
	return fetchCommit(ctx, ex, repo, remote, branch)
}

// A private temporary ref avoids sharing FETCH_HEAD with normal developer fetches.
func fetchCommit(ctx context.Context, ex Executor, repo, remote, branch string) (sha string, err error) {
	ref := "refs/codex-diag/" + rand.Text()
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, cleanupErr := ex.Run(cleanup, repo, "git", "update-ref", "-d", ref)
		err = errors.Join(err, cleanupErr)
	}()
	if _, err = ex.Run(ctx, repo, "git", "fetch", "--no-tags", "--no-write-fetch-head", remote, "refs/heads/"+branch+":"+ref); err != nil {
		return "", err
	}
	sha, err = ex.Run(ctx, repo, "git", "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	if !shaPattern.MatchString(sha) {
		return "", errors.New("invalid fetched commit SHA")
	}
	return sha, nil
}

func NewWorkspace(ctx context.Context, ex Executor, repo, sha string) (*Workspace, error) {
	if !shaPattern.MatchString(sha) {
		return nil, errors.New("workspace requires an immutable commit SHA")
	}
	parent, err := os.MkdirTemp("", "codex2api-diag-")
	if err != nil {
		return nil, err
	}
	w := &Workspace{Root: filepath.Join(parent, "source"), Repo: repo, BaseSHA: sha, Exec: ex, parent: parent, files: map[string]bool{}}
	if _, err := ex.Run(ctx, repo, "git", "worktree", "add", "--detach", w.Root, sha); err != nil {
		_ = os.RemoveAll(parent)
		return nil, err
	}
	out, err := ex.Run(ctx, w.Root, "git", "ls-files", "--stage", "-z")
	if err != nil {
		_ = w.Close()
		return nil, err
	}
	for _, entry := range strings.Split(out, "\x00") {
		fields := strings.SplitN(entry, "\t", 2)
		if len(fields) == 2 && strings.HasPrefix(fields[0], "100644 ") && allowedPath(fields[1]) {
			w.files[fields[1]] = true
		}
	}
	return w, nil
}

func (w *Workspace) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := w.Exec.Run(ctx, w.Repo, "git", "worktree", "remove", "--force", w.Root)
	if err != nil {
		return err
	}
	return os.RemoveAll(w.parent)
}

var goPathPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_./-]*\.go$`)

func allowedPath(name string) bool {
	if !goPathPattern.MatchString(name) || path.Clean(name) != name || strings.Contains(name, "..") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") {
			return false
		}
	}
	return !strings.HasPrefix(name, "internal/diag/") && !strings.HasPrefix(name, "cmd/diagnose/") && name != "api/diagnostic.go"
}

func (w *Workspace) Inventory() []string {
	files := make([]string, 0, len(w.files))
	for name := range w.files {
		files = append(files, name)
	}
	sort.Strings(files)
	return files
}

func (w *Workspace) Sources(requests []SourceRequest) ([]Source, error) {
	if len(requests) == 0 || len(requests) > 6 {
		return nil, errors.New("select between 1 and 6 files")
	}
	var sources []Source
	total := 0
	seen := map[string]bool{}
	for _, request := range requests {
		if !w.files[request.Path] || seen[request.Path] {
			return nil, fmt.Errorf("unavailable or duplicate source: %q", request.Path)
		}
		seen[request.Path] = true
		data, err := w.read(request.Path)
		if err != nil {
			return nil, err
		}
		content := string(data)
		if len(data) > 24<<10 {
			content = excerpt(content, request)
		}
		total += len(content)
		if total > 144<<10 {
			return nil, errors.New("selected source context exceeds 144 KiB")
		}
		sources = append(sources, Source{Path: request.Path, Content: content})
	}
	return sources, nil
}

func excerpt(content string, request SourceRequest) string {
	lines := strings.Split(content, "\n")
	var centers []int
	if request.Query != "" && len(request.Query) <= 256 {
		for i, line := range lines {
			if strings.Contains(line, request.Query) {
				centers = append(centers, i)
			}
			if len(centers) == 3 {
				break
			}
		}
	}
	if len(centers) == 0 {
		line := max(1, min(request.Line, len(lines)))
		centers = []int{line - 1}
	}
	var b strings.Builder
	end := 0
	for _, center := range centers {
		start := max(end, max(0, center-60))
		stop := min(len(lines), center+100)
		if start >= stop {
			continue
		}
		fmt.Fprintf(&b, "\n// EXCERPT %s lines %d-%d (marker is not source)\n", request.Path, start+1, stop)
		b.WriteString(strings.Join(lines[start:stop], "\n"))
		b.WriteByte('\n')
		end = stop
	}
	return bounded(b.String(), 24<<10)
}

func (w *Workspace) read(name string) ([]byte, error) {
	if !allowedPath(name) {
		return nil, errors.New("source path is outside the repair allowlist")
	}
	full := filepath.Join(w.Root, filepath.FromSlash(name))
	info, err := os.Lstat(full)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, fmt.Errorf("source %s must be a regular file no larger than 1 MiB", name)
	}
	return os.ReadFile(full)
}

// Apply creates a real Git diff from exact replacements. It never executes Go code.
func (w *Workspace) Apply(ctx context.Context, d Diagnosis, sources []Source) (string, error) {
	if !d.CanFix || len(d.Edits) == 0 || len(d.Edits) > 12 {
		return "", errors.New("diagnosis contains no bounded repair")
	}
	selected, dirs := map[string]string{}, map[string]bool{}
	for _, source := range sources {
		selected[source.Path] = source.Content
		if !strings.HasSuffix(source.Path, "_test.go") {
			dirs[path.Dir(source.Path)] = true
		}
	}
	grouped := map[string][]Edit{}
	total := 0
	for _, edit := range d.Edits {
		if !allowedPath(edit.Path) {
			return "", fmt.Errorf("disallowed repair path: %q", edit.Path)
		}
		total += len(edit.Old) + len(edit.New)
		if total > 64<<10 {
			return "", errors.New("repair replacements exceed 64 KiB")
		}
		if w.files[edit.Path] {
			if edit.Old == "" || !strings.Contains(selected[edit.Path], edit.Old) || strings.HasSuffix(edit.Path, "_test.go") {
				return "", errors.New("existing edits must match supplied production source; existing tests are read-only")
			}
		} else if edit.Old != "" || !strings.HasSuffix(edit.Path, "_test.go") || !dirs[path.Dir(edit.Path)] {
			return "", errors.New("only new regression tests in selected source directories may be added")
		}
		grouped[edit.Path] = append(grouped[edit.Path], edit)
	}
	if len(grouped) > 6 {
		return "", errors.New("repair changes more than 6 files")
	}
	var names []string
	for name, edits := range grouped {
		var changed string
		if w.files[name] {
			original, err := w.read(name)
			if err != nil {
				return "", err
			}
			changed = string(original)
			type replacement struct {
				start, end int
				text       string
			}
			var replacements []replacement
			for _, edit := range edits {
				if strings.Count(changed, edit.Old) != 1 {
					return "", fmt.Errorf("replacement in %s is not unique", name)
				}
				start := strings.Index(changed, edit.Old)
				replacements = append(replacements, replacement{start, start + len(edit.Old), edit.New})
			}
			sort.Slice(replacements, func(i, j int) bool { return replacements[i].start > replacements[j].start })
			last := len(changed)
			for _, r := range replacements {
				if r.end > last {
					return "", errors.New("overlapping source replacements")
				}
				changed = changed[:r.start] + r.text + changed[r.end:]
				last = r.start
			}
		} else {
			if len(edits) != 1 {
				return "", errors.New("new test files must have exactly one edit")
			}
			if _, err := os.Lstat(filepath.Join(w.Root, filepath.FromSlash(name))); !os.IsNotExist(err) {
				return "", errors.New("new test path already exists")
			}
			changed = edits[0].New
		}
		formatted, err := format.Source([]byte(changed))
		if err != nil {
			return "", fmt.Errorf("invalid Go syntax in %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(w.Root, filepath.FromSlash(name)), formatted, 0644); err != nil {
			return "", err
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if _, err := w.Exec.Run(ctx, w.Root, "git", append([]string{"add", "--"}, names...)...); err != nil {
		return "", err
	}
	if _, err := w.Exec.Run(ctx, w.Root, "git", "diff", "--cached", "--check"); err != nil {
		return "", err
	}
	diff, err := w.Exec.Run(ctx, w.Root, "git", "diff", "--cached", "--no-ext-diff", "--no-textconv", "--binary")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(diff) == "" || len(diff) > 128<<10 {
		return "", errors.New("repair diff is empty or exceeds 128 KiB")
	}
	// gofmt may expand a seemingly small edit; bound the resulting line delta too.
	stats, err := w.Exec.Run(ctx, w.Root, "git", "diff", "--cached", "--numstat")
	if err != nil {
		return "", err
	}
	lineDelta := 0
	for _, line := range strings.Split(stats, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			return "", errors.New("invalid repair diff statistics")
		}
		for _, n := range fields[:2] {
			count, e := strconv.Atoi(n)
			if e != nil {
				return "", errors.New("binary repairs are forbidden")
			}
			lineDelta += count
		}
	}
	if lineDelta > 800 {
		return "", errors.New("repair exceeds 800 changed lines")
	}
	return diff + "\n", nil
}
