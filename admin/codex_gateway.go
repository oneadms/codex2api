package admin

import (
	"errors"
	"github.com/codex2api/internal/harvest"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"net/http"
)

func (h *Handler) GetCodexGateway(c *gin.Context) {
	if !h.requireCodexHarvest(c) {
		return
	}
	v, err := h.codexHarvest.GatewaySettings(c.Request.Context())
	if err != nil {
		writeError(c, 500, "读取借票配置失败")
		return
	}
	c.JSON(200, v)
}
func (h *Handler) UpdateCodexGateway(c *gin.Context) {
	if !h.requireCodexHarvest(c) {
		return
	}
	var v harvest.GatewaySettings
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32<<10)
	if c.ShouldBindJSON(&v) != nil {
		writeError(c, 400, "借票配置格式错误")
		return
	}
	v.Normalize()
	if err := v.Validate(); err != nil {
		writeError(c, 400, err.Error())
		return
	}
	saved, err := h.codexHarvest.SaveGatewaySettings(c.Request.Context(), v)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, proxy.ErrGatewayRevisionConflict) {
			status = http.StatusConflict
		}
		writeError(c, status, err.Error())
		return
	}
	c.JSON(200, saved)
}
func (h *Handler) GetCodexGatewayStatus(c *gin.Context) {
	if !h.requireCodexHarvest(c) {
		return
	}
	c.JSON(200, h.codexHarvest.GatewaySnapshot(c.Request.Context()))
}
func (h *Handler) TestCodexGateway(c *gin.Context) {
	if !h.requireCodexHarvest(c) {
		return
	}
	var v struct {
		Action    string `json:"action"`
		AccountID int64  `json:"account_id"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4<<10)
	if c.ShouldBindJSON(&v) != nil || v.AccountID < 0 {
		writeError(c, 400, "诊断请求格式错误")
		return
	}
	out, err := h.codexHarvest.TestGateway(c.Request.Context(), v.Action, v.AccountID)
	if err != nil {
		writeError(c, 400, err.Error())
		return
	}
	c.JSON(200, out)
}
func (h *Handler) GetCodexGatewayHistory(c *gin.Context) {
	if !h.requireCodexHarvest(c) {
		return
	}
	offset, limit := harvestPagination(c)
	out, err := h.codexHarvest.Repository.GatewayHistory(c.Request.Context(), offset, limit)
	if err != nil {
		writeError(c, 500, "读取借票历史失败")
		return
	}
	c.JSON(200, out)
}
