package diag

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type ConfigStore interface {
	LoadDiagnosticConfig(context.Context) (ServerConfig, error)
	SaveDiagnosticConfig(context.Context, ServerConfig) error
}

type RuntimeStatus struct {
	Running            bool       `json:"running"`
	Collecting         bool       `json:"collecting"`
	GitAvailable       bool       `json:"git_available"`
	GitHubCLIAvailable bool       `json:"github_cli_available"`
	LastStarted        *time.Time `json:"last_started"`
	LastFinished       *time.Time `json:"last_finished"`
	NextRun            *time.Time `json:"next_run"`
	LastError          string     `json:"last_error"`
	LastResult         RunResult  `json:"last_result"`
	DroppedEvents      uint64     `json:"dropped_events"`
}

type Manager struct {
	mu        sync.Mutex
	store     ConfigStore
	root      string
	cfg       ServerConfig
	status    RuntimeStatus
	collector *Collector
	ctx       context.Context
	cancel    context.CancelFunc
	runCancel context.CancelFunc
	wake      chan struct{}
	done      chan struct{}
	wg        sync.WaitGroup
	closed    bool
	execute   func(context.Context, ServerConfig) (RunResult, error) // test seam, defaults to managedRun
}

func NewManager(store ConfigStore, dataDir string) *Manager {
	if dataDir == "" {
		dataDir = "data"
	}
	return &Manager{store: store, root: filepath.Join(dataDir, "diagnostics"), cfg: DefaultServerConfig(), wake: make(chan struct{}, 1), done: make(chan struct{})}
}

func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx != nil || m.closed {
		return
	}
	m.ctx, m.cancel = context.WithCancel(ctx)
	cfg, err := m.store.LoadDiagnosticConfig(ctx)
	if err == nil {
		err = cfg.Validate()
	}
	if err == nil && cfg.Enabled {
		m.collector, err = StartManagedCollector(m.logPath())
	}
	if err != nil {
		m.status.LastError = SafeText(err.Error())
		cfg.Enabled = false
		cfg.AutoRun = false
	}
	m.cfg = cfg
	if cfg.AutoRun {
		now := time.Now().UTC()
		m.status.NextRun = &now
	}
	go m.loop()
}

func (m *Manager) loop() {
	defer close(m.done)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		m.mu.Lock()
		due := m.cfg.AutoRun && m.status.NextRun != nil && !time.Now().Before(*m.status.NextRun)
		m.mu.Unlock()
		if due {
			_ = m.RunNow()
		}
		select {
		case <-m.ctx.Done():
			return
		case <-m.wake:
		case <-ticker.C:
		}
	}
}

func (m *Manager) Close() {
	m.mu.Lock()
	if m.ctx == nil {
		m.closed = true
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.cancel()
	c := m.collector
	m.collector = nil
	m.mu.Unlock()
	<-m.done
	m.wg.Wait()
	if c != nil {
		_ = c.Close()
	}
}

func (m *Manager) Config() ServerConfig { m.mu.Lock(); defer m.mu.Unlock(); return m.cfg }

func (m *Manager) Update(ctx context.Context, cfg ServerConfig, clearKey, clearToken bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx == nil || m.ctx.Err() != nil {
		return errors.New("诊断服务已停止")
	}
	cfg = cfg.Normalized()
	if clearKey {
		cfg.APIKey = ""
	} else if cfg.APIKey == "" {
		cfg.APIKey = m.cfg.APIKey
	}
	if clearToken {
		cfg.GitHubToken = ""
	} else if cfg.GitHubToken == "" {
		cfg.GitHubToken = m.cfg.GitHubToken
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	var started *Collector
	if cfg.Enabled && m.collector == nil {
		var err error
		started, err = StartManagedCollector(m.logPath())
		if err != nil {
			return err
		}
	}
	if err := m.store.SaveDiagnosticConfig(ctx, cfg); err != nil {
		if started != nil {
			_ = started.Close()
		}
		return err
	}
	if m.runCancel != nil {
		m.runCancel()
	}
	if started != nil {
		m.collector = started
	}
	if !cfg.Enabled && m.collector != nil {
		_ = m.collector.Close()
		m.collector = nil
	}
	m.cfg = cfg
	m.status.LastError = ""
	m.status.NextRun = nil
	if cfg.AutoRun {
		next := time.Now().UTC().Add(time.Duration(cfg.IntervalMinutes) * time.Minute)
		m.status.NextRun = &next
	}
	select {
	case m.wake <- struct{}{}:
	default:
	}
	return nil
}

func (m *Manager) Status() RuntimeStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := m.status
	_, gitErr := exec.LookPath("git")
	_, ghErr := exec.LookPath("gh")
	status.GitAvailable, status.GitHubCLIAvailable = gitErr == nil, ghErr == nil
	status.Collecting = m.collector != nil
	if m.collector != nil {
		status.DroppedEvents = m.collector.Dropped()
	}
	return status
}

var ErrAlreadyRunning = errors.New("已有诊断任务运行中")

func (m *Manager) RunNow() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx == nil || m.ctx.Err() != nil {
		return errors.New("诊断服务已停止")
	}
	if m.status.Running {
		return ErrAlreadyRunning
	}
	if err := m.cfg.ValidateRun(); err != nil {
		return err
	}
	if m.collector == nil {
		return errors.New("诊断日志采集未启动")
	}
	cfg := m.cfg
	ctx, cancel := context.WithTimeout(m.ctx, 15*time.Minute)
	m.runCancel = cancel
	now := time.Now().UTC()
	m.status.Running = true
	m.status.LastStarted = &now
	m.status.LastError = ""
	if cfg.AutoRun {
		next := now.Add(time.Duration(cfg.IntervalMinutes) * time.Minute)
		m.status.NextRun = &next
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		run := m.execute
		if run == nil {
			run = m.managedRun
		}
		result, err := run(ctx, cfg)
		m.mu.Lock()
		defer m.mu.Unlock()
		m.status.Running = false
		m.runCancel = nil
		finished := time.Now().UTC()
		m.status.LastFinished = &finished
		m.status.LastResult = result
		if err != nil {
			m.status.LastError = runtimeError(err, cfg)
		} else {
			for _, outcome := range result.Outcomes {
				if outcome.Status == "failed" {
					m.status.LastError = runtimeError(errors.New(outcome.Error), cfg)
					break
				}
			}
		}
		if m.cfg.AutoRun {
			next := finished.Add(time.Duration(m.cfg.IntervalMinutes) * time.Minute)
			m.status.NextRun = &next
		}
	}()
	return nil
}

func runtimeError(err error, cfg ServerConfig) string {
	message := err.Error()
	for _, secret := range []string{cfg.APIKey, cfg.GitHubToken} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	return bounded(SafeText(message), 2000)
}

func (m *Manager) logPath() string { return filepath.Join(m.root, "diagnostics.jsonl") }
func (m *Manager) repoRoot(cfg ServerConfig) string {
	sum := sha256.Sum256([]byte(strings.ToLower(cfg.Repository)))
	return filepath.Join(m.root, "repositories", hex.EncodeToString(sum[:16]))
}
func (m *Manager) stateDir(cfg ServerConfig) string { return filepath.Join(m.repoRoot(cfg), "state") }

func (m *Manager) managedRun(ctx context.Context, cfg ServerConfig) (RunResult, error) {
	result := RunResult{Outcomes: []Outcome{}}
	scan, err := ScanLogs(m.logPath(), time.Now().Add(-time.Duration(cfg.WindowHours)*time.Hour))
	if err != nil {
		return result, err
	}
	eligible := false
	for _, g := range scan.Groups {
		if g.Count >= cfg.MinCount {
			eligible = true
			break
		}
	}
	if !eligible {
		result.Skipped = len(scan.Groups)
		result.Malformed = scan.Malformed
		result.Overflow = scan.Overflow
		return result, nil
	}
	if _, err = exec.LookPath("git"); err != nil {
		return result, errors.New("当前运行环境缺少 Git，请使用内置 worker 的新版 Docker 镜像")
	}
	if cfg.GitHubToken != "" {
		if _, err = exec.LookPath("gh"); err != nil {
			return result, errors.New("当前运行环境缺少 GitHub CLI，请使用新版 Docker 镜像")
		}
	}
	ex := Commands{GitHubToken: cfg.GitHubToken}
	root := m.repoRoot(cfg)
	repo := filepath.Join(root, "repo.git")
	if err = os.MkdirAll(root, 0700); err != nil {
		return result, err
	}
	remote := "https://github.com/" + cfg.Repository + ".git"
	if _, err = os.Stat(filepath.Join(repo, "HEAD")); os.IsNotExist(err) {
		tmp := filepath.Join(root, "clone-"+rand.Text())
		defer os.RemoveAll(tmp)
		if _, err = ex.Run(ctx, root, "git", "clone", "--bare", "--depth", "1", "--single-branch", "--branch", RepairBaseBranch, "--", remote, tmp); err != nil {
			return result, err
		}
		if err = os.Rename(tmp, repo); err != nil {
			return result, err
		}
	} else if err != nil {
		return result, err
	}
	actual, err := ex.Run(ctx, repo, "git", "remote", "get-url", "origin")
	if err != nil {
		return result, err
	}
	if !strings.EqualFold(actual, remote) {
		return result, errors.New("托管仓库的 origin 与配置不符，已停止扫描")
	}
	c := DefaultWorkerConfig()
	c.Repo = repo
	c.LogPath = m.logPath()
	c.StateDir = m.stateDir(cfg)
	c.MinCount, c.MaxPerRun, c.MinConfidence, c.Publish = cfg.MinCount, cfg.MaxPerRun, cfg.MinConfidence, cfg.Publish
	c.Window = time.Duration(cfg.WindowHours) * time.Hour
	w := Worker{Config: c, Exec: ex, Analyzer: &ChatAnalyzer{URL: cfg.ModelURL, Model: cfg.Model, APIKey: cfg.APIKey}}
	return w.Run(ctx)
}

type HistoryItem struct {
	ID string `json:"id"`
	Entry
}

var reportIDPattern = regexp.MustCompile(`^[0-9a-f]{64}-[0-9a-f]{40}$`)

func (m *Manager) History() ([]HistoryItem, error) {
	items := []HistoryItem{}
	state := map[string]Entry{}
	err := readJSON(filepath.Join(m.stateDir(m.Config()), "state.json"), &state)
	if os.IsNotExist(err) {
		return items, nil
	}
	if err != nil {
		return nil, err
	}
	for id, entry := range state {
		if reportIDPattern.MatchString(id) {
			items = append(items, HistoryItem{ID: id, Entry: entry})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].UpdatedAt.After(items[j].UpdatedAt) })
	if len(items) > 50 {
		items = items[:50]
	}
	return items, nil
}

func (m *Manager) Incidents() (Scan, error) {
	cfg := m.Config()
	if _, err := os.Stat(m.logPath()); os.IsNotExist(err) {
		return Scan{Groups: []Group{}}, nil
	}
	scan, err := ScanLogs(m.logPath(), time.Now().Add(-time.Duration(cfg.WindowHours)*time.Hour))
	if len(scan.Groups) > 50 {
		scan.Groups = scan.Groups[:50]
	}
	return scan, err
}

func (m *Manager) Report(id string) (Report, string, error) {
	if !reportIDPattern.MatchString(id) {
		return Report{}, "", errors.New("无效的诊断记录 ID")
	}
	dir := filepath.Join(m.stateDir(m.Config()), "runs", id)
	var report Report
	if err := readJSON(filepath.Join(dir, "report.json"), &report); err != nil {
		return report, "", err
	}
	patch, err := os.ReadFile(filepath.Join(dir, "repair.patch"))
	if os.IsNotExist(err) {
		err = nil
	}
	if len(patch) > 128<<10 {
		return report, "", fmt.Errorf("补丁超过展示上限")
	}
	return report, string(patch), err
}
