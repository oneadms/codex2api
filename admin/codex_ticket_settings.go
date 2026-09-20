package admin

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// codexTicketSettingsResponse 是打票配置的管理端投影。代理地址回显时把密码掩成
// ***（绝不把凭据透出去），并额外给一个 has_password 让界面知道"回显的是掩码"。
type codexTicketSettingsResponse struct {
	Enabled              bool     `json:"enabled"`
	HarvestProxyURL      string   `json:"harvest_proxy_url"`
	HarvestProxyMasked   bool     `json:"harvest_proxy_masked"`
	TargetLength         int      `json:"target_length"`
	TTLSeconds           int      `json:"ttl_seconds"`
	RefreshBeforeSeconds int      `json:"refresh_before_seconds"`
	ProbeIntervalSeconds int      `json:"probe_interval_seconds"`
	CooldownSeconds      int      `json:"cooldown_seconds"`
	MaxProbesPerRound    int      `json:"max_probes_per_round"`
	ProbeTimeoutSeconds  int      `json:"probe_timeout_seconds"`
	FailClosed           bool     `json:"fail_closed"`
	Models               []string `json:"models"`
	// GateActive 报告门控当前是否真的生效（开关+代理+名单三者齐备）。
	GateActive bool `json:"gate_active"`
	// Ready 是各门控模型上已有可用门票的账号数 / 账号总数，供界面判断打票是否正常。
	ReadyAccounts int `json:"ready_accounts"`
	TotalAccounts int `json:"total_accounts"`
}

func (h *Handler) codexTicketSettingsResponse(settings auth.CodexTicketSettings) codexTicketSettingsResponse {
	response := codexTicketSettingsResponse{
		Enabled:              settings.Enabled,
		HarvestProxyURL:      auth.MaskCodexTicketProxyURL(settings.HarvestProxyURL),
		TargetLength:         settings.TargetLength,
		TTLSeconds:           settings.TTLSeconds,
		RefreshBeforeSeconds: settings.RefreshBeforeSeconds,
		ProbeIntervalSeconds: settings.ProbeIntervalSeconds,
		CooldownSeconds:      settings.CooldownSeconds,
		MaxProbesPerRound:    settings.MaxProbesPerRound,
		ProbeTimeoutSeconds:  settings.ProbeTimeoutSeconds,
		FailClosed:           settings.FailClosed,
		Models:               settings.Models,
		GateActive:           auth.CodexTicketGateEnabled(),
	}
	response.HarvestProxyMasked = settings.HarvestProxyURL != "" && response.HarvestProxyURL != settings.HarvestProxyURL
	response.ReadyAccounts, response.TotalAccounts = h.codexTicketReadyAccountCounts(settings.Models)
	return response
}

// codexTicketReadyAccountCounts 统计各门控模型上已有可用门票的账号数。任一门控模型
// 有票即算就绪——账号通常只跑一两个模型，按"全部命中"统计会让界面长期显示 0。
func (h *Handler) codexTicketReadyAccountCounts(models []string) (ready, total int) {
	if h.store == nil || len(models) == 0 {
		return 0, 0
	}
	accounts := h.store.Accounts()
	total = len(accounts)
	now := time.Now()
	for _, account := range accounts {
		if account == nil {
			continue
		}
		targetLen := auth.CodexTicketTargetLengthFor(account.GetPlanType())
		if _, ok := account.CodexTicketInjection(now, targetLen, models...); ok {
			ready++
		}
	}
	return ready, total
}

func (h *Handler) GetCodexTicketSettings(c *gin.Context) {
	settings := auth.ConfiguredCodexTicketSettings()
	c.JSON(http.StatusOK, h.codexTicketSettingsResponse(settings))
}

// UpdateCodexTicketSettings 局部更新：只替换请求里出现的字段，未传字段保持原值。
// 代理地址收到掩码占位符时沿用原密码，避免把 *** 当成新密码存回去。
func (h *Handler) UpdateCodexTicketSettings(c *gin.Context) {
	var req struct {
		Enabled              *bool     `json:"enabled"`
		HarvestProxyURL      *string   `json:"harvest_proxy_url"`
		TargetLength         *int      `json:"target_length"`
		TTLSeconds           *int      `json:"ttl_seconds"`
		RefreshBeforeSeconds *int      `json:"refresh_before_seconds"`
		ProbeIntervalSeconds *int      `json:"probe_interval_seconds"`
		CooldownSeconds      *int      `json:"cooldown_seconds"`
		MaxProbesPerRound    *int      `json:"max_probes_per_round"`
		ProbeTimeoutSeconds  *int      `json:"probe_timeout_seconds"`
		FailClosed           *bool     `json:"fail_closed"`
		Models               *[]string `json:"models"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体格式错误")
		return
	}
	if req.Enabled == nil && req.HarvestProxyURL == nil && req.TargetLength == nil &&
		req.TTLSeconds == nil && req.RefreshBeforeSeconds == nil && req.ProbeIntervalSeconds == nil &&
		req.CooldownSeconds == nil && req.MaxProbesPerRound == nil && req.ProbeTimeoutSeconds == nil &&
		req.FailClosed == nil && req.Models == nil {
		writeError(c, http.StatusBadRequest, "至少需要提供一个要更新的字段")
		return
	}

	current := auth.ConfiguredCodexTicketSettings()
	next := current
	if req.Enabled != nil {
		next.Enabled = *req.Enabled
	}
	if req.HarvestProxyURL != nil {
		next.HarvestProxyURL = resolveCodexTicketProxyUpdate(*req.HarvestProxyURL, current.HarvestProxyURL)
	}
	if req.TargetLength != nil {
		next.TargetLength = *req.TargetLength
	}
	if req.TTLSeconds != nil {
		next.TTLSeconds = *req.TTLSeconds
	}
	if req.RefreshBeforeSeconds != nil {
		next.RefreshBeforeSeconds = *req.RefreshBeforeSeconds
	}
	if req.ProbeIntervalSeconds != nil {
		next.ProbeIntervalSeconds = *req.ProbeIntervalSeconds
	}
	if req.CooldownSeconds != nil {
		next.CooldownSeconds = *req.CooldownSeconds
	}
	if req.MaxProbesPerRound != nil {
		next.MaxProbesPerRound = *req.MaxProbesPerRound
	}
	if req.ProbeTimeoutSeconds != nil {
		next.ProbeTimeoutSeconds = *req.ProbeTimeoutSeconds
	}
	if req.FailClosed != nil {
		next.FailClosed = *req.FailClosed
	}
	if req.Models != nil {
		next.Models = *req.Models
	}

	settings, err := auth.NormalizeCodexTicketSettings(next)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	// 开着门控但没有打票代理等于门控空转：直接拒绝，避免运维以为开了实际没票。
	if settings.Enabled && strings.TrimSpace(settings.HarvestProxyURL) == "" {
		writeError(c, http.StatusBadRequest, "启用打票必须配置打票代理（业务代理不会被复用）")
		return
	}
	if settings.Enabled && len(settings.Models) == 0 {
		writeError(c, http.StatusBadRequest, "启用打票必须至少配置一个门控模型")
		return
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
	if err := h.db.SaveCodexTicketConfig(c.Request.Context(), string(encoded)); err != nil {
		writeError(c, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	auth.SetConfiguredCodexTicketSettings(settings)
	// 配置生效后立刻打一轮：门控刚打开时账号上还没有票，FailClosed 会先把请求拒掉。
	proxy.KickCodexTicketHarvest()
	log.Printf("设置已更新: codex_ticket_config enabled=%t models=%v fail_closed=%t", settings.Enabled, settings.Models, settings.FailClosed)
	c.JSON(http.StatusOK, h.codexTicketSettingsResponse(settings))
}

// resolveCodexTicketProxyUpdate 处理代理地址的掩码回显：界面把带 *** 的地址原样提交
// 回来时沿用原密码；显式提交空串表示清除代理。
func resolveCodexTicketProxyUpdate(incoming, current string) string {
	incoming = strings.TrimSpace(incoming)
	if incoming == "" {
		return ""
	}
	if !auth.IsMaskedCodexTicketProxyURL(incoming) {
		return incoming
	}
	// 掩码形态：把当前地址的密码替换进占位符。若原地址本身不可用则维持原值。
	masked := auth.MaskCodexTicketProxyURL(current)
	if masked != incoming {
		// 掩码与当前值不一致（并发改动等），维持原值比存下 *** 更安全。
		return current
	}
	return current
}
