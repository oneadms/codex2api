package admin

import (
	"net/http"
	"path/filepath"
	"strconv"

	"github.com/codex2api/internal/harvest"
	"github.com/codex2api/internal/mihomo"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// ConfigureCodexHarvest 由启动流程调用；普通 Handler 单元测试不启动外部进程。
func (h *Handler) ConfigureCodexHarvest(dataDir string) {
	h.mihomo = mihomo.New(filepath.Join(dataDir, "mihomo-managed"))
	h.codexHarvest = proxy.ConfigureCodexHarvest(h.db, h.store, dataDir)
}

func (h *Handler) StopCodexHarvest() {
	if h.codexHarvest != nil {
		h.codexHarvest.Close()
	}
}

func (h *Handler) GetMihomo(c *gin.Context) {
	if h.mihomo == nil {
		writeError(c, http.StatusServiceUnavailable, "Mihomo 未初始化")
		return
	}
	c.JSON(http.StatusOK, h.mihomo.Status())
}

func (h *Handler) UpdateMihomo(c *gin.Context) {
	if h.mihomo == nil {
		writeError(c, http.StatusServiceUnavailable, "Mihomo 未初始化")
		return
	}
	var request struct {
		Action        string                `json:"action"`
		Subscriptions []string              `json:"subscriptions"`
		Append        bool                  `json:"append"`
		CountryFilter *mihomo.CountryFilter `json:"country_filter"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 256<<10)
	if err := c.ShouldBindJSON(&request); err != nil {
		writeError(c, http.StatusBadRequest, "Mihomo 请求格式错误")
		return
	}
	if err := h.mihomo.Submit(request.Action, request.Subscriptions, request.Append, request.CountryFilter); err != nil {
		status := http.StatusBadRequest
		if h.mihomo.Status().Busy {
			status = http.StatusConflict
		}
		writeError(c, status, err.Error())
		return
	}
	c.JSON(http.StatusAccepted, h.mihomo.Status())
}

func (h *Handler) requireCodexHarvest(c *gin.Context) bool {
	if h.codexHarvest != nil {
		return true
	}
	writeError(c, http.StatusServiceUnavailable, "采票管理未初始化")
	return false
}

func (h *Handler) GetCodexHarvest(c *gin.Context) {
	if !h.requireCodexHarvest(c) {
		return
	}
	ctx := c.Request.Context()
	accounts, err := h.codexHarvest.Accounts(ctx)
	if err != nil {
		writeError(c, 500, "读取采票账号失败")
		return
	}
	scope, err := h.codexHarvest.Scope(ctx)
	if err != nil {
		writeError(c, 500, "读取采票范围失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"controls": h.codexHarvest.ControlSnapshot(ctx), "scope": scope, "accounts": accounts, "jobs": h.codexHarvest.Jobs()})
}

func (h *Handler) UpdateCodexHarvestControls(c *gin.Context) {
	if !h.requireCodexHarvest(c) {
		return
	}
	var request harvest.CodexHarvestControls
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32<<10)
	if err := c.ShouldBindJSON(&request); err != nil {
		writeError(c, 400, "控制配置格式错误")
		return
	}
	if err := harvest.ValidateCodexHarvestControls(request); err != nil {
		writeError(c, 400, err.Error())
		return
	}
	if err := h.codexHarvest.Controls.SaveControls(c.Request.Context(), request); err != nil {
		writeError(c, 500, "保存采票控制失败")
		return
	}
	c.JSON(http.StatusOK, h.codexHarvest.ControlSnapshot(c.Request.Context()))
}

func (h *Handler) UpdateCodexHarvestScope(c *gin.Context) {
	if !h.requireCodexHarvest(c) {
		return
	}
	var request harvest.Scope
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	if err := c.ShouldBindJSON(&request); err != nil {
		writeError(c, 400, "采票范围格式错误")
		return
	}
	if err := request.Validate(); err != nil {
		writeError(c, 400, err.Error())
		return
	}
	if err := h.codexHarvest.SaveScope(c.Request.Context(), request); err != nil {
		writeError(c, 500, "保存采票范围失败")
		return
	}
	c.JSON(http.StatusOK, request)
}

func harvestPagination(c *gin.Context) (int, int) {
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	return max(0, offset), min(max(1, limit), 200)
}

func (h *Handler) ListCodexHarvestNodes(c *gin.Context) {
	if !h.requireCodexHarvest(c) {
		return
	}
	offset, limit := harvestPagination(c)
	page, err := h.codexHarvest.Controls.ListNodes(c.Request.Context(), offset, limit)
	if err != nil {
		writeError(c, 500, "读取节点学习记录失败")
		return
	}
	c.JSON(http.StatusOK, page)
}

func (h *Handler) ResetCodexHarvestNodes(c *gin.Context) {
	if !h.requireCodexHarvest(c) {
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id < 0 {
		writeError(c, 400, "无效节点记录 ID")
		return
	}
	if err = h.codexHarvest.Controls.ResetNodes(c.Request.Context(), id); err != nil {
		writeError(c, 500, "重置节点记录失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) ListCodexHarvestEvents(c *gin.Context) {
	if !h.requireCodexHarvest(c) {
		return
	}
	offset, limit := harvestPagination(c)
	accountID, err := strconv.ParseInt(c.DefaultQuery("account_id", "0"), 10, 64)
	if err != nil || accountID < 0 {
		writeError(c, 400, "无效账号 ID")
		return
	}
	page, err := h.codexHarvest.Repository.Events(c.Request.Context(), accountID, c.Query("job_id"), offset, limit)
	if err != nil {
		writeError(c, 500, "读取采票事件失败")
		return
	}
	c.JSON(http.StatusOK, page)
}

func (h *Handler) StartCodexManualHarvest(c *gin.Context) {
	if !h.requireCodexHarvest(c) {
		return
	}
	var request harvest.ManualRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32<<10)
	if err := c.ShouldBindJSON(&request); err != nil {
		writeError(c, 400, "手动采票请求格式错误")
		return
	}
	job, err := h.codexHarvest.StartManual(c.Request.Context(), request)
	if err != nil {
		writeError(c, http.StatusConflict, err.Error())
		return
	}
	c.JSON(http.StatusAccepted, job)
}

func (h *Handler) StopCodexManualHarvest(c *gin.Context) {
	if !h.requireCodexHarvest(c) {
		return
	}
	if !h.codexHarvest.CancelJob(c.Param("id")) {
		writeError(c, 404, "任务不存在或已结束")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) KickCodexHarvest(c *gin.Context) {
	proxy.KickCodexTicketHarvest()
	c.JSON(http.StatusAccepted, gin.H{"ok": true})
}
