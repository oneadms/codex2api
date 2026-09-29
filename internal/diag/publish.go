package diag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var githubRepoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func GitHubRepository(remote string) (string, error) {
	var name string
	if strings.HasPrefix(remote, "git@github.com:") {
		name = strings.TrimPrefix(remote, "git@github.com:")
	} else {
		u, err := url.Parse(remote)
		if err != nil || u.Host != "github.com" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "ssh") {
			return "", errors.New("publishing requires a github.com HTTPS or SSH remote")
		}
		if u.User != nil && (u.Scheme != "ssh" || u.User.String() != "git") {
			return "", errors.New("do not embed credentials in the Git remote URL")
		}
		name = strings.TrimPrefix(u.Path, "/")
	}
	name = strings.TrimSuffix(name, ".git")
	if !githubRepoPattern.MatchString(name) || strings.Contains(name, "..") {
		return "", errors.New("invalid GitHub owner/repository")
	}
	return name, nil
}

type Publisher struct {
	Exec                       Executor
	Repo, Remote, Base, GitHub string
}

var repairBranchPattern = regexp.MustCompile(`^codex/autofix/[0-9a-f]{12}-[0-9a-f]{12}$`)

func (p Publisher) validate(branch string) error {
	if p.Base != RepairBaseBranch || !repairBranchPattern.MatchString(branch) {
		return errors.New("only codex/autofix repair branches targeting custom/main may be published")
	}
	return nil
}

func (p Publisher) Existing(ctx context.Context, branch string) (string, error) {
	if err := p.validate(branch); err != nil {
		return "", err
	}
	out, err := p.Exec.Run(ctx, p.Repo, "gh", "pr", "list", "--repo", p.GitHub, "--base", p.Base, "--head", branch, "--state", "all", "--limit", "10", "--json", "url,state")
	if err != nil {
		return "", err
	}
	var prs []struct {
		URL   string `json:"url"`
		State string `json:"state"`
	}
	if err = json.Unmarshal([]byte(out), &prs); err != nil {
		return "", fmt.Errorf("read existing PRs: %w", err)
	}
	if len(prs) > 0 {
		return prs[0].URL, nil
	}
	// A base update must not create another PR while the same incident is open.
	fingerprint := strings.Split(strings.TrimPrefix(branch, "codex/autofix/"), "-")[0]
	out, err = p.Exec.Run(ctx, p.Repo, "gh", "pr", "list", "--repo", p.GitHub, "--base", p.Base, "--state", "open", "--search", `"diag-fingerprint:`+fingerprint+`" in:body`, "--limit", "10", "--json", "url,state")
	if err != nil {
		return "", err
	}
	if err = json.Unmarshal([]byte(out), &prs); err != nil {
		return "", fmt.Errorf("read matching PRs: %w", err)
	}
	if len(prs) > 0 {
		return prs[0].URL, nil
	}
	return "", nil
}

func (p Publisher) Publish(ctx context.Context, w *Workspace, branch string, report Report, artifactDir string) (string, error) {
	if existing, err := p.Existing(ctx, branch); err != nil || existing != "" {
		return existing, err
	}
	if _, err := p.Exec.Run(ctx, w.Root, "git", "-c", "user.name=Codex2API Diagnostics", "-c", "user.email=diag@users.noreply.github.com", "commit", "-m", "fix(diag): "+report.Group.Fingerprint[:12]); err != nil {
		return "", err
	}
	remoteHead, err := p.Exec.Run(ctx, w.Root, "git", "ls-remote", "--heads", p.Remote, "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	if remoteHead != "" {
		// Recover after a push succeeded but PR creation (or the worker) failed.
		remoteSHA, fetchErr := fetchCommit(ctx, p.Exec, w.Root, p.Remote, branch)
		if fetchErr != nil {
			return "", fetchErr
		}
		remoteTree, err := p.Exec.Run(ctx, w.Root, "git", "rev-parse", remoteSHA+"^{tree}")
		if err != nil {
			return "", err
		}
		localTree, err := p.Exec.Run(ctx, w.Root, "git", "rev-parse", "HEAD^{tree}")
		if err != nil {
			return "", err
		}
		if localTree != remoteTree {
			return "", errors.New("repair branch already exists with different content; refusing to overwrite it")
		}
	} else if _, err = p.Exec.Run(ctx, w.Root, "git", "push", p.Remote, "HEAD:refs/heads/"+branch); err != nil {
		return "", err
	}
	bodyPath := filepath.Join(artifactDir, "pr-body.md")
	body := fmt.Sprintf("AI 诊断生成的修复草稿，需要人工审核并等待 CI。\n\n- 错误指纹：`%s`\n- 目标分支：`%s`；分析基线：`%s`\n- 观察次数：%d；类型：`%s`；HTTP 状态：%d\n- 模型自报置信度：%.2f（不作为合并依据）\n\n根因分析：\n\n```text\n%s\n```\n\n验证：精确替换、路径/规模限制、Go 语法、gofmt、git diff 检查已通过。\n构建与回归测试由本 PR 的 GitHub Actions 执行，当前仍需查看 CI 结果；worker 不执行生成的代码。\n合并和部署均需人工操作。\n", report.Group.Fingerprint, p.Base, w.BaseSHA, report.Group.Count, report.Group.Sample.Kind, report.Group.Sample.Status, report.Diagnosis.Confidence, strings.ReplaceAll(strings.ReplaceAll(report.Diagnosis.RootCause, "`", "'"), "@", "＠"))
	body = "<!-- diag-fingerprint:" + report.Group.Fingerprint[:12] + " -->\n\n" + body
	if err = os.WriteFile(bodyPath, []byte(body), 0600); err != nil {
		return "", err
	}
	title := strings.ReplaceAll(strings.ReplaceAll(report.Diagnosis.Title, "@", "＠"), "\r", "")
	if title == "" {
		title = report.Group.Fingerprint[:12]
	}
	out, err := p.Exec.Run(ctx, w.Root, "gh", "pr", "create", "--repo", p.GitHub, "--base", p.Base, "--head", branch, "--draft", "--title", "fix(diag): "+title, "--body-file", bodyPath)
	if err != nil {
		return "", err
	}
	prURL := strings.TrimSpace(out)
	if !strings.HasPrefix(prURL, "https://github.com/"+p.GitHub+"/pull/") {
		return "", errors.New("GitHub returned an unexpected PR URL; the next run will reconcile it")
	}
	return prURL, nil
}
