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
	Models                  []string          `json:"models"`
}

func (h *Handler) GetTraeCNSettings(c *gin.Context) {
	settings := auth.ConfiguredTraeCNSettings()
	c.JSON(http.StatusOK, traeCNSettingsResponse{
		ModelMapping:            settings.ModelMapping,
		PreflightSSEPassthrough: settings.PreflightSSEPassthrough,
		Models:                  h.traeCNChannelModels(),
	})
}

// UpdateTraeCNSettings 局部更新：只替换请求里出现的字段，未传字段保持原值；
// 传空对象表示清空映射。模型映射与「前置元数据立即下发」开关共用该接口，
// 因此两者都可单独提交，互不覆盖。
func (h *Handler) UpdateTraeCNSettings(c *gin.Context) {
	var req struct {
		ModelMapping            *map[string]string `json:"model_mapping"`
		PreflightSSEPassthrough *bool              `json:"preflight_sse_passthrough"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体必须是包含 model_mapping 或 preflight_sse_passthrough 的对象")
		return
	}
	if req.ModelMapping == nil && req.PreflightSSEPassthrough == nil {
		writeError(c, http.StatusBadRequest, "至少需要提供 model_mapping 或 preflight_sse_passthrough")
		return
	}
	current := auth.ConfiguredTraeCNSettings()
	next := auth.TraeCNSettings{
		ModelMapping:            current.ModelMapping,
		PreflightSSEPassthrough: current.PreflightSSEPassthrough,
	}
	if req.PreflightSSEPassthrough != nil {
		next.PreflightSSEPassthrough = *req.PreflightSSEPassthrough
	}
	if req.ModelMapping != nil {
		next.ModelMapping = *req.ModelMapping
	}
	settings, err := auth.NormalizeTraeCNSettings(next)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
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
	log.Printf("设置已更新: traecn_config mappings=%d preflight_sse_passthrough=%t", len(settings.ModelMapping), settings.PreflightSSEPassthrough)
	c.JSON(http.StatusOK, traeCNSettingsResponse{
		ModelMapping:            settings.ModelMapping,
		PreflightSSEPassthrough: settings.PreflightSSEPassthrough,
		Models:                  models,
	})
}
