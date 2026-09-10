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
	ModelMapping map[string]string `json:"model_mapping"`
	Models       []string          `json:"models"`
}

func (h *Handler) GetTraeCNSettings(c *gin.Context) {
	c.JSON(http.StatusOK, traeCNSettingsResponse{
		ModelMapping: auth.ConfiguredTraeCNSettings().ModelMapping,
		Models:       h.traeCNChannelModels(),
	})
}

// UpdateTraeCNSettings 整体替换映射；空对象表示清空，未传字段不修改。
func (h *Handler) UpdateTraeCNSettings(c *gin.Context) {
	var req struct {
		ModelMapping *map[string]string `json:"model_mapping"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.ModelMapping == nil {
		writeError(c, http.StatusBadRequest, "model_mapping 必须是模型名称到目标模型的对象")
		return
	}
	settings, err := auth.NormalizeTraeCNSettings(auth.TraeCNSettings{ModelMapping: *req.ModelMapping})
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
	log.Printf("设置已更新: traecn_config mappings=%d", len(settings.ModelMapping))
	c.JSON(http.StatusOK, traeCNSettingsResponse{ModelMapping: settings.ModelMapping, Models: models})
}
