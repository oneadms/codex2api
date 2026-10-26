package admin

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

type traeCNSettingsResponse struct {
	ModelMapping            map[string]string `json:"model_mapping"`
	PreflightSSEPassthrough bool              `json:"preflight_sse_passthrough"`
	ModelSyncEnabled        bool              `json:"model_sync_enabled"`
	ModelSyncTime           string            `json:"model_sync_time"`
	ModelSyncTimezone       string            `json:"model_sync_timezone"`
	Models                  []string          `json:"models"`
}

func (h *Handler) traeCNSettingsResponse(settings auth.TraeCNSettings) traeCNSettingsResponse {
	return traeCNSettingsResponse{
		ModelMapping:            settings.ModelMapping,
		PreflightSSEPassthrough: settings.PreflightSSEPassthrough,
		ModelSyncEnabled:        settings.ModelSyncEnabled,
		ModelSyncTime:           settings.ModelSyncTime,
		ModelSyncTimezone:       settings.ModelSyncTimezone,
		Models:                  h.traeCNChannelModels(),
	}
}

func (h *Handler) GetTraeCNSettings(c *gin.Context) {
	c.JSON(http.StatusOK, h.traeCNSettingsResponse(auth.ConfiguredTraeCNSettings()))
}

// UpdateTraeCNSettings only replaces supplied fields. Each settings card can
// save independently without resetting mappings, passthrough or the schedule.
func (h *Handler) UpdateTraeCNSettings(c *gin.Context) {
	var req struct {
		ModelMapping            *map[string]string `json:"model_mapping"`
		PreflightSSEPassthrough *bool              `json:"preflight_sse_passthrough"`
		ModelSyncEnabled        *bool              `json:"model_sync_enabled"`
		ModelSyncTime           *string            `json:"model_sync_time"`
		ModelSyncTimezone       *string            `json:"model_sync_timezone"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体必须是有效的 TRAECN 设置对象")
		return
	}
	if req.ModelMapping == nil && req.PreflightSSEPassthrough == nil && req.ModelSyncEnabled == nil && req.ModelSyncTime == nil && req.ModelSyncTimezone == nil {
		writeError(c, http.StatusBadRequest, "至少需要提供一个 TRAECN 设置字段")
		return
	}
	if req.ModelSyncTime != nil && strings.TrimSpace(*req.ModelSyncTime) == "" || req.ModelSyncTimezone != nil && strings.TrimSpace(*req.ModelSyncTimezone) == "" {
		writeError(c, http.StatusBadRequest, "模型目录同步时间和时区不能为空")
		return
	}
	h.settingsUpdateMu.Lock()
	defer h.settingsUpdateMu.Unlock()
	next := auth.ConfiguredTraeCNSettings()
	if req.PreflightSSEPassthrough != nil {
		next.PreflightSSEPassthrough = *req.PreflightSSEPassthrough
	}
	if req.ModelMapping != nil {
		next.ModelMapping = *req.ModelMapping
	}
	if req.ModelSyncEnabled != nil {
		next.ModelSyncEnabled = *req.ModelSyncEnabled
	}
	if req.ModelSyncTime != nil {
		next.ModelSyncTime = *req.ModelSyncTime
	}
	if req.ModelSyncTimezone != nil {
		next.ModelSyncTimezone = *req.ModelSyncTimezone
	}
	settings, err := auth.NormalizeTraeCNSettings(next)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	// An upstream catalog may have changed since a mapping was saved. Such a
	// stale mapping must not prevent an administrator from changing the schedule.
	if req.ModelMapping != nil {
		models := h.traeCNChannelModels()
		for _, target := range settings.ModelMapping {
			found := false
			for _, model := range models {
				if strings.EqualFold(model, target) {
					found = true
					break
				}
			}
			if !found {
				writeError(c, http.StatusBadRequest, "目标模型不在 TRAECN 模型目录中: "+target)
				return
			}
		}
	}
	if h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "数据库不可用")
		return
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.db.SaveTraeCNConfig(c.Request.Context(), string(encoded)); err != nil {
		writeError(c, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	auth.SetConfiguredTraeCNSettings(settings)
	log.Printf("设置已更新: traecn_config mappings=%d preflight_sse_passthrough=%t model_sync_enabled=%t model_sync_time=%s model_sync_timezone=%s", len(settings.ModelMapping), settings.PreflightSSEPassthrough, settings.ModelSyncEnabled, settings.ModelSyncTime, settings.ModelSyncTimezone)
	c.JSON(http.StatusOK, h.traeCNSettingsResponse(settings))
}
