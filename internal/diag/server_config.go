package diag

import (
	"errors"
	"math"
	"strings"
)

// ServerConfig is persisted by the host application. Secrets never appear in GET responses.
type ServerConfig struct {
	Enabled         bool    `json:"enabled"`
	AutoRun         bool    `json:"auto_run"`
	Publish         bool    `json:"publish"`
	Repository      string  `json:"repository"`
	ModelURL        string  `json:"model_url"`
	Model           string  `json:"model"`
	APIKey          string  `json:"api_key,omitempty"`
	GitHubToken     string  `json:"github_token,omitempty"`
	IntervalMinutes int     `json:"interval_minutes"`
	WindowHours     int     `json:"window_hours"`
	MinCount        int     `json:"min_count"`
	MaxPerRun       int     `json:"max_per_run"`
	MinConfidence   float64 `json:"min_confidence"`
}

func DefaultServerConfig() ServerConfig {
	return ServerConfig{Repository: "oneadms/codex2api", IntervalMinutes: 5, WindowHours: 24, MinCount: 3, MaxPerRun: 1, MinConfidence: 0.8}
}

func (c ServerConfig) Normalized() ServerConfig {
	c.Repository = strings.TrimSpace(c.Repository)
	c.ModelURL = strings.TrimSpace(c.ModelURL)
	c.Model = strings.TrimSpace(c.Model)
	c.APIKey = strings.TrimSpace(c.APIKey)
	c.GitHubToken = strings.TrimSpace(c.GitHubToken)
	return c
}

func (c ServerConfig) Validate() error {
	if !githubRepoPattern.MatchString(c.Repository) || strings.Contains(c.Repository, "..") || strings.HasSuffix(c.Repository, ".git") {
		return errors.New("仓库格式应为 owner/repository，例如 oneadms/codex2api")
	}
	if c.IntervalMinutes < 1 || c.IntervalMinutes > 1440 || c.WindowHours < 1 || c.WindowHours > 168 || c.MinCount < 1 || c.MinCount > 10000 || c.MaxPerRun < 1 || c.MaxPerRun > 10 || math.IsNaN(c.MinConfidence) || math.IsInf(c.MinConfidence, 0) || c.MinConfidence < 0 || c.MinConfidence > 1 {
		return errors.New("扫描间隔须为 1–1440 分钟，回溯 1–168 小时，阈值 1–10000，单次问题数 1–10，置信度 0–1")
	}
	if len(c.APIKey) > 8192 || len(c.GitHubToken) > 8192 || len(c.Model) > 128 || len(c.ModelURL) > 2048 || strings.ContainsAny(c.APIKey+c.GitHubToken, "\r\n") {
		return errors.New("模型或凭据字段格式无效")
	}
	if c.ModelURL != "" {
		a := ChatAnalyzer{URL: c.ModelURL, Model: "validate-url"}
		if err := a.Validate(); err != nil {
			return err
		}
	}
	if c.AutoRun && !c.Enabled {
		return errors.New("请先开启日志采集，再启用自动诊断")
	}
	if c.AutoRun {
		return c.ValidateRun()
	}
	return nil
}

func (c ServerConfig) ValidateRun() error {
	if !c.Enabled {
		return errors.New("请先开启日志采集")
	}
	if err := (&ChatAnalyzer{URL: c.ModelURL, Model: c.Model}).Validate(); err != nil {
		return err
	}
	if c.Publish && c.GitHubToken == "" {
		return errors.New("创建草稿 PR 需要填写 GitHub Token")
	}
	return nil
}

func (c ServerConfig) Public() map[string]any {
	return map[string]any{"enabled": c.Enabled, "auto_run": c.AutoRun, "publish": c.Publish, "repository": c.Repository, "model_url": c.ModelURL, "model": c.Model, "interval_minutes": c.IntervalMinutes, "window_hours": c.WindowHours, "min_count": c.MinCount, "max_per_run": c.MaxPerRun, "min_confidence": c.MinConfidence, "has_api_key": c.APIKey != "", "has_github_token": c.GitHubToken != "", "base_branch": RepairBaseBranch}
}
