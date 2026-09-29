package diag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type WorkerConfig struct {
	Repo          string
	Remote        string
	Base          string
	LogPath       string
	StateDir      string
	Window        time.Duration
	Cooldown      time.Duration
	MinCount      int
	MaxPerRun     int
	MaxAttempts   int
	MinConfidence float64
	Publish       bool
}

type Worker struct {
	Config   WorkerConfig
	Analyzer Analyzer
	Exec     Executor
}

type Entry struct {
	Status    string    `json:"status"`
	Attempts  int       `json:"attempts"`
	UpdatedAt time.Time `json:"updated_at"`
	PRURL     string    `json:"pr_url,omitempty"`
	Error     string    `json:"error,omitempty"`
}

type Report struct {
	Status     string          `json:"status"`
	Group      Group           `json:"incident"`
	BaseSHA    string          `json:"base_sha"`
	Branch     string          `json:"branch"`
	Requests   []SourceRequest `json:"sources"`
	Diagnosis  Diagnosis       `json:"diagnosis"`
	Validation string          `json:"validation"`
}

type Outcome struct {
	Fingerprint string `json:"fingerprint"`
	Status      string `json:"status"`
	Artifacts   string `json:"artifacts,omitempty"`
	PRURL       string `json:"pr_url,omitempty"`
	Error       string `json:"error,omitempty"`
}

type RunResult struct {
	Outcomes  []Outcome `json:"outcomes"`
	Skipped   int       `json:"skipped"`
	Malformed int       `json:"malformed_lines"`
	Overflow  int       `json:"overflow_events"`
}

func DefaultWorkerConfig() WorkerConfig {
	return WorkerConfig{Repo: ".", Remote: "origin", Base: RepairBaseBranch, LogPath: LogPath(), StateDir: "data/diag", Window: 24 * time.Hour, Cooldown: 6 * time.Hour, MinCount: 3, MaxPerRun: 1, MaxAttempts: 3, MinConfidence: 0.8}
}

func (w *Worker) Run(ctx context.Context) (RunResult, error) {
	result := RunResult{Outcomes: []Outcome{}}
	c := w.Config
	if c.Base != RepairBaseBranch {
		return result, errors.New("repairs must target custom/main; main is reserved for upstream updates")
	}
	if c.MinCount < 1 || c.MaxPerRun < 1 || c.MaxPerRun > 10 || c.MaxAttempts < 1 || c.MaxAttempts > 10 || c.Window <= 0 || c.Cooldown <= 0 || math.IsNaN(c.MinConfidence) || math.IsInf(c.MinConfidence, 0) || c.MinConfidence < 0 || c.MinConfidence > 1 {
		return result, errors.New("invalid worker configuration: counts/durations must be positive and limits at most 10")
	}
	var err error
	c.Repo, err = filepath.Abs(c.Repo)
	if err != nil {
		return result, err
	}
	c.StateDir, err = filepath.Abs(c.StateDir)
	if err != nil {
		return result, err
	}
	if err = os.MkdirAll(c.StateDir, 0700); err != nil {
		return result, err
	}
	lock := filepath.Join(c.StateDir, ".lock")
	if err = os.Mkdir(lock, 0700); err != nil {
		return result, fmt.Errorf("cannot acquire diagnostic worker lock %s: %w (if stale, verify the old worker has stopped before removing it)", lock, err)
	}
	defer os.RemoveAll(lock)
	_ = os.WriteFile(filepath.Join(lock, "owner"), []byte(fmt.Sprintf("pid=%d\nstarted=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339))), 0600)
	state := map[string]Entry{}
	stateFile := filepath.Join(c.StateDir, "state.json")
	if err = readJSON(stateFile, &state); err != nil && !os.IsNotExist(err) {
		return result, fmt.Errorf("read worker state: %w", err)
	}
	if state == nil {
		return result, errors.New("invalid null worker state")
	}
	scan, err := ScanLogs(c.LogPath, time.Now().Add(-c.Window))
	if err != nil {
		return result, err
	}
	result.Malformed, result.Overflow = scan.Malformed, scan.Overflow
	var eligible []Group
	for _, g := range scan.Groups {
		if g.Count >= c.MinCount {
			eligible = append(eligible, g)
		} else {
			result.Skipped++
		}
	}
	if len(eligible) == 0 {
		return result, nil
	}
	ex := w.Exec
	if ex == nil {
		ex = Commands{}
	}
	sha, err := FetchBase(ctx, ex, c.Repo, c.Remote, c.Base)
	if err != nil {
		return result, err
	}
	publisher := Publisher{Exec: ex, Repo: c.Repo, Remote: c.Remote, Base: c.Base}
	if c.Publish {
		remoteURL, err := ex.Run(ctx, c.Repo, "git", "remote", "get-url", c.Remote)
		if err != nil {
			return result, err
		}
		publisher.GitHub, err = GitHubRepository(remoteURL)
		if err != nil {
			return result, err
		}
	}
	for _, g := range eligible {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		key := g.Fingerprint + "-" + sha
		entry := state[key]
		if entry.Status == "published" || entry.Status == "no_fix" || (entry.Status == "prepared" && !c.Publish) || (entry.Status != "prepared" && (entry.Attempts >= c.MaxAttempts || (!entry.UpdatedAt.IsZero() && time.Since(entry.UpdatedAt) < c.Cooldown))) {
			result.Skipped++
			continue
		}
		if len(result.Outcomes) >= c.MaxPerRun {
			result.Skipped++
			continue
		}
		branch := "codex/autofix/" + g.Fingerprint[:12] + "-" + sha[:12]
		dir := filepath.Join(c.StateDir, "runs", key)
		entry.Attempts++
		entry.Status, entry.UpdatedAt, entry.Error = "diagnosing", time.Now().UTC(), ""
		state[key] = entry
		if err = writeJSON(stateFile, state); err != nil {
			return result, err
		}
		outcome, runErr := w.process(ctx, ex, publisher, c, g, sha, branch, dir)
		if runErr != nil {
			outcome = Outcome{Fingerprint: g.Fingerprint, Status: "failed", Artifacts: dir, Error: bounded(SafeText(runErr.Error()), 2000)}
		}
		entry.Status, entry.UpdatedAt, entry.PRURL, entry.Error = outcome.Status, time.Now().UTC(), outcome.PRURL, outcome.Error
		state[key] = entry
		if err = writeJSON(stateFile, state); err != nil {
			return result, err
		}
		result.Outcomes = append(result.Outcomes, outcome)
	}
	return result, nil
}

func (w *Worker) process(ctx context.Context, ex Executor, p Publisher, c WorkerConfig, g Group, sha, branch, dir string) (outcome Outcome, err error) {
	outcome = Outcome{Fingerprint: g.Fingerprint, Artifacts: dir}
	if c.Publish {
		url, err := p.Existing(ctx, branch)
		if err != nil {
			return outcome, err
		}
		if url != "" {
			outcome.Status, outcome.PRURL = "published", url
			return outcome, nil
		}
	}
	ws, err := NewWorkspace(ctx, ex, c.Repo, sha)
	if err != nil {
		return outcome, err
	}
	defer func() { err = errors.Join(err, ws.Close()) }()
	report := Report{}
	err = readJSON(filepath.Join(dir, "report.json"), &report)
	if err != nil && !os.IsNotExist(err) {
		return outcome, err
	}
	var sources []Source
	if err == nil && report.Status == "prepared" && report.BaseSHA == sha && report.Group.Fingerprint == g.Fingerprint {
		sources, err = ws.Sources(report.Requests)
		if err != nil {
			return outcome, err
		}
	} else {
		if w.Analyzer == nil {
			return outcome, errors.New("model analyzer is not configured")
		}
		inventory := ws.Inventory()
		if len(strings.Join(inventory, "\n")) > 128<<10 {
			return outcome, errors.New("repository inventory exceeds 128 KiB")
		}
		requests, err := w.Analyzer.Select(ctx, g, inventory)
		if err != nil {
			return outcome, err
		}
		report = Report{Group: g, BaseSHA: sha, Branch: branch, Requests: requests, Status: "no_fix"}
		if len(requests) == 0 {
			report.Diagnosis = Diagnosis{RootCause: "未选出相关代码，可能是运行环境或上游问题；需要人工诊断。"}
		} else {
			sources, err = ws.Sources(requests)
			if err != nil {
				return outcome, err
			}
			report.Diagnosis, err = w.Analyzer.Diagnose(ctx, g, sources)
			if err != nil {
				return outcome, err
			}
		}
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return outcome, err
	}
	if !report.Diagnosis.CanFix || report.Diagnosis.Confidence < c.MinConfidence || len(report.Diagnosis.Edits) == 0 {
		report.Status = "no_fix"
		report.Validation = "Evidence insufficient or confidence below threshold; no patch generated."
		if err = writeJSON(filepath.Join(dir, "report.json"), report); err != nil {
			return outcome, err
		}
		outcome.Status = "no_fix"
		return outcome, nil
	}
	diff, err := ws.Apply(ctx, report.Diagnosis, sources)
	if err != nil {
		return outcome, err
	}
	report.Status = "prepared"
	report.Validation = "Path/size/unique replacement checks, Go syntax, gofmt and git diff --check passed. Build and tests pending GitHub CI."
	if err = os.WriteFile(filepath.Join(dir, "repair.patch"), []byte(diff), 0600); err != nil {
		return outcome, err
	}
	if err = writeJSON(filepath.Join(dir, "report.json"), report); err != nil {
		return outcome, err
	}
	outcome.Status = "prepared"
	if c.Publish {
		outcome.PRURL, err = p.Publish(ctx, ws, branch, report, dir)
		if err != nil {
			return outcome, err
		}
		outcome.Status = "published"
	}
	return outcome, nil
}

func readJSON(name string, target any) error {
	info, err := os.Stat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return errors.New("JSON state/report must be a regular file of at most 4 MiB")
	}
	data, err := os.ReadFile(name)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func writeJSON(name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > 4<<20 {
		return errors.New("worker state/report exceeds 4 MiB; archive old runs before continuing")
	}
	f, err := os.CreateTemp(filepath.Dir(name), ".diag-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), name)
}
