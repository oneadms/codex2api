package admin

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/codex2api/internal/diag"
	"github.com/gin-gonic/gin"
)

func (h *Handler) StartDiagnostics(ctx context.Context, dataDir string) {
	if h.diagnostics != nil || h.db == nil {
		return
	}
	h.diagnostics = diag.NewManager(h.db, dataDir)
	h.diagnostics.Start(ctx)
}

func (h *Handler) StopDiagnostics() {
	if h.diagnostics != nil {
		h.diagnostics.Close()
	}
}

func (h *Handler) diagnosticManager(c *gin.Context) *diag.Manager {
	if h.diagnostics == nil {
		writeError(c, http.StatusServiceUnavailable, "诊断服务尚未启动")
	}
	return h.diagnostics
}

func (h *Handler) GetDiagnosticSettings(c *gin.Context) {
	if m := h.diagnosticManager(c); m != nil {
		c.JSON(http.StatusOK, m.Config().Public())
	}
}

func (h *Handler) UpdateDiagnosticSettings(c *gin.Context) {
	m := h.diagnosticManager(c)
	if m == nil {
		return
	}
	var request struct {
		diag.ServerConfig
		BaseBranch       string `json:"base_branch"`
		ClearAPIKey      bool   `json:"clear_api_key"`
		ClearGitHubToken bool   `json:"clear_github_token"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		writeError(c, http.StatusBadRequest, "诊断设置格式无效")
		return
	}
	if request.BaseBranch != "" && request.BaseBranch != diag.RepairBaseBranch {
		writeError(c, http.StatusBadRequest, "修复目标固定为 custom/main，main 仅跟随官方更新")
		return
	}
	if err := m.Update(c.Request.Context(), request.ServerConfig, request.ClearAPIKey, request.ClearGitHubToken); err != nil {
		writeError(c, http.StatusBadRequest, diag.SafeText(err.Error()))
		return
	}
	c.JSON(http.StatusOK, m.Config().Public())
}

func (h *Handler) GetDiagnosticStatus(c *gin.Context) {
	if m := h.diagnosticManager(c); m != nil {
		c.JSON(http.StatusOK, m.Status())
	}
}

func (h *Handler) RunDiagnosticScan(c *gin.Context) {
	m := h.diagnosticManager(c)
	if m == nil {
		return
	}
	if err := m.RunNow(); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, diag.ErrAlreadyRunning) {
			status = http.StatusConflict
		}
		writeError(c, status, diag.SafeText(err.Error()))
		return
	}
	c.JSON(http.StatusAccepted, m.Status())
}

func (h *Handler) ListDiagnosticHistory(c *gin.Context) {
	m := h.diagnosticManager(c)
	if m == nil {
		return
	}
	items, err := m.History()
	if err != nil {
		writeError(c, http.StatusInternalServerError, "读取诊断记录失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func (h *Handler) ListDiagnosticIncidents(c *gin.Context) {
	m := h.diagnosticManager(c)
	if m == nil {
		return
	}
	scan, err := m.Incidents()
	if err != nil {
		writeError(c, http.StatusInternalServerError, "读取诊断日志失败")
		return
	}
	c.JSON(http.StatusOK, scan)
}

func (h *Handler) GetDiagnosticReport(c *gin.Context) {
	m := h.diagnosticManager(c)
	if m == nil {
		return
	}
	report, patch, err := m.Report(c.Param("id"))
	if err != nil {
		status := http.StatusBadRequest
		if os.IsNotExist(err) {
			status = http.StatusNotFound
		}
		writeError(c, status, "该记录暂无诊断报告")
		return
	}
	// Keep credentials out of API errors even if a provider echoed one unexpectedly.
	cfg := m.Config()
	for _, secret := range []string{cfg.APIKey, cfg.GitHubToken} {
		if secret != "" {
			report.Diagnosis.RootCause = strings.ReplaceAll(report.Diagnosis.RootCause, secret, "[redacted]")
		}
	}
	c.JSON(http.StatusOK, gin.H{"report": report, "patch": patch})
}
