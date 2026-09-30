package diag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// CodexSDKRunner invokes the official Codex SDK in an isolated repository checkout.
// The configured API key is sent as CODEX_API_KEY and the base URL targets this project's /v1 endpoint.
type CodexSDKRunner struct {
	NodePath        string
	SDKRoot         string
	Script          string
	BaseURL         string
	APIKey          string
	Model           string
	ReasoningEffort string
	EventLog        string
}

type CodexRepairResult struct {
	Title      string  `json:"title"`
	RootCause  string  `json:"root_cause"`
	Confidence float64 `json:"confidence"`
	CanFix     bool    `json:"can_fix"`
}

func (r CodexSDKRunner) Validate() error {
	u, err := url.Parse(strings.TrimSpace(r.BaseURL))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("Codex Base URL 必须是不含凭据和查询参数的 HTTP(S) /v1 地址")
	}
	if u.Scheme == "http" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" {
		return errors.New("Codex Base URL 除本机回环地址外必须使用 HTTPS")
	}
	if strings.Trim(u.Path, "/") != "v1" {
		return errors.New("Codex Base URL 应以 /v1 结尾，例如 https://your-codex2api.example/v1")
	}
	if strings.TrimSpace(r.Model) == "" || len(r.Model) > 128 || strings.ContainsAny(r.Model, "\r\n") {
		return errors.New("请填写 Codex 模型名称")
	}
	if len(r.APIKey) > 8192 || strings.ContainsAny(r.APIKey, "\r\n") {
		return errors.New("API Key 格式无效")
	}
	return nil
}

func (r CodexSDKRunner) Repair(ctx context.Context, workingDirectory, prompt string) (CodexRepairResult, error) {
	var result CodexRepairResult
	if err := r.Validate(); err != nil {
		return result, err
	}
	node := r.NodePath
	if node == "" {
		var err error
		node, err = exec.LookPath("node")
		if err != nil {
			return result, errors.New("当前运行环境缺少 Node.js，请使用包含 Codex SDK 的新版 Docker 镜像")
		}
	}
	script := r.Script
	if script == "" {
		script = filepath.Join(r.SDKRoot, "codex_runner.mjs")
	}
	payload, err := json.Marshal(map[string]any{
		"baseUrl": inputBaseURL(r.BaseURL), "apiKey": r.APIKey, "model": r.Model,
		"workingDirectory": workingDirectory, "prompt": prompt, "timeoutMs": int((12 * time.Minute).Milliseconds()),
		"idleTimeoutMs":   int((3 * time.Minute).Milliseconds()),
		"reasoningEffort": r.ReasoningEffort,
		"eventLog":        r.EventLog,
		"env":             codexChildEnvironment(),
	})
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(ctx, 13*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, script)
	cmd.Dir = r.SDKRoot
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Env = append(os.Environ(), "NODE_PATH="+filepath.Join(r.SDKRoot, "node_modules"))
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err = cmd.Run(); err != nil {
		return result, fmt.Errorf("Codex SDK 执行失败: %w: %s", err, bounded(SafeText(stderr.String()), 1000))
	}
	if err = json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
		return result, errors.New("Codex SDK 未返回要求的 JSON 结果")
	}
	if strings.TrimSpace(result.RootCause) == "" || result.Confidence < 0 || result.Confidence > 1 {
		return result, errors.New("Codex SDK 返回的诊断结果无效")
	}
	result.Title = bounded(SafeText(strings.ReplaceAll(result.Title, "\n", " ")), 120)
	result.RootCause = SafeText(result.RootCause)
	return result, nil
}

func inputBaseURL(raw string) string { return strings.TrimRight(strings.TrimSpace(raw), "/") }

func codexChildEnvironment() map[string]string {
	env := map[string]string{}
	for _, key := range []string{"PATH", "HOME", "USER", "TMPDIR", "LANG", "LC_ALL"} {
		if value, ok := os.LookupEnv(key); ok {
			env[key] = value
		}
	}
	return env
}
