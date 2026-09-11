package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

const traeCNImportMaxAccounts = 500

type traeCNAccountsRequest struct {
	Name          string          `json:"name"`
	RefreshTokens json.RawMessage `json:"refresh_tokens"`
	RefreshToken  string          `json:"refresh_token"`
	ProxyURL      string          `json:"proxy_url"`
	Host          string          `json:"host"`
	Models        []string        `json:"models"`
	GroupIDs      json.RawMessage `json:"group_ids"`
	Enabled       *bool           `json:"enabled"`
}

type updateTraeCNAccountRequest struct {
	Name string `json:"name"`
	Host string `json:"host"`
	// Models is a pointer so an edit that only changes host/name/proxy does not
	// accidentally clear a legacy account allowlist. An explicit empty array
	// still means "follow the whole upstream catalog".
	Models   *[]string       `json:"models"`
	ProxyURL string          `json:"proxy_url"`
	GroupIDs json.RawMessage `json:"group_ids"`
}

type traeCNImportItem struct {
	Index   int64  `json:"index"`
	ID      int64  `json:"id,omitempty"`
	Name    string `json:"name,omitempty"`
	UserID  string `json:"user_id,omitempty"`
	OK      bool   `json:"ok"`
	Stored  bool   `json:"stored"`
	Warning string `json:"warning,omitempty"`
	Error   string `json:"error,omitempty"`
}

func parseTraeCNRefreshTokens(raw json.RawMessage, singular string) ([]string, error) {
	values := make([]string, 0)
	if trimmed := strings.TrimSpace(string(raw)); trimmed != "" && trimmed != "null" {
		if strings.HasPrefix(trimmed, "[") {
			if err := json.Unmarshal(raw, &values); err != nil {
				return nil, fmt.Errorf("refresh_tokens 必须是字符串或字符串数组")
			}
		} else {
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				return nil, fmt.Errorf("refresh_tokens 必须是字符串或字符串数组")
			}
			values = append(values, strings.FieldsFunc(value, func(r rune) bool { return r == '\n' || r == '\r' || r == ',' })...)
		}
	}
	if strings.TrimSpace(singular) != "" {
		values = append(values, strings.FieldsFunc(singular, func(r rune) bool { return r == '\n' || r == '\r' || r == ',' })...)
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}

// resolveTraeCNGroupIDs validates the channel before any account is created or
// edited. bindImportedAccountGroups performs the same check after insertion as
// a final database guard, but rejecting here keeps an invalid import free of
// partial side effects.
func (h *Handler) resolveTraeCNGroupIDs(ctx context.Context, raw json.RawMessage) ([]int64, error) {
	groupIDs, err := h.resolveImportGroupIDsJSON(ctx, raw)
	if err != nil || len(groupIDs) == 0 {
		return groupIDs, err
	}
	groups, err := h.db.ListAccountGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("校验分组渠道失败: %w", err)
	}
	byID := make(map[int64]database.AccountGroup, len(groups))
	for _, group := range groups {
		byID[group.ID] = group
	}
	for _, groupID := range groupIDs {
		group := byID[groupID]
		channel := database.NormalizeAccountGroupChannel(group.Channel)
		if channel != database.AccountGroupChannelTraeCN {
			return nil, fmt.Errorf("分组「%s」是 %s 渠道分组,不能加入 TRAECN 账号",
				group.Name, groupChannelDisplayName(channel))
		}
	}
	return groupIDs, nil
}

func (h *Handler) AddTraeCNAccounts(c *gin.Context) {
	var req traeCNAccountsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	name := security.SanitizeInput(strings.TrimSpace(req.Name))
	proxyURL := security.SanitizeInput(strings.TrimSpace(req.ProxyURL))
	if security.ContainsXSS(name) || security.ContainsSQLInjection(name) || utf8.RuneCountInString(name) > 100 {
		writeError(c, http.StatusBadRequest, "名称包含非法字符或长度超过100字符")
		return
	}
	if err := security.ValidateProxyURL(proxyURL); err != nil {
		writeError(c, http.StatusBadRequest, "代理URL无效")
		return
	}
	host, err := auth.NormalizeTraeCNHost(req.Host)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	refreshTokens, err := parseTraeCNRefreshTokens(req.RefreshTokens, req.RefreshToken)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	if len(refreshTokens) == 0 {
		writeError(c, http.StatusBadRequest, "refresh_tokens 是必填字段")
		return
	}
	if len(refreshTokens) > traeCNImportMaxAccounts {
		writeError(c, http.StatusBadRequest, fmt.Sprintf("单次最多添加 %d 个 Trae CN 账号", traeCNImportMaxAccounts))
		return
	}
	models := auth.NormalizeAccountModels(req.Models)
	if len(models) > 200 {
		writeError(c, http.StatusBadRequest, "模型数量不能超过 200")
		return
	}
	for _, model := range models {
		if err := security.ValidateModelName(model); err != nil {
			writeError(c, http.StatusBadRequest, fmt.Sprintf("模型名称无效: %s", model))
			return
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 180*time.Second)
	defer cancel()
	groupIDs, err := h.resolveTraeCNGroupIDs(ctx, req.GroupIDs)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	existing, err := h.db.GetAllRefreshTokens(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	items := make([]traeCNImportItem, len(refreshTokens))
	createdIDs := make([]int64, 0, len(refreshTokens))
	var mu sync.Mutex
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for index, refreshToken := range refreshTokens {
		index, refreshToken := index, refreshToken
		wg.Add(1)
		go func() {
			defer wg.Done()
			item := traeCNImportItem{Index: int64(index + 1)}
			if existing[refreshToken] {
				item.Error = "refresh_token 已存在"
				items[index] = item
				return
			}
			accountName := name
			if accountName == "" {
				accountName = "traecn"
			}
			if len(refreshTokens) > 1 {
				accountName = fmt.Sprintf("%s-%d", accountName, index+1)
			}
			item.Name = accountName
			// Reserve the RT before contacting ExchangeToken. The reservation is
			// atomic at the database level, so concurrent import requests cannot
			// both consume and store the same rotating credential.
			// 一账号一份设备码：Trae 有风控，同设备码下的多个账号会被判定为同机
			// 批量登录。导入时就在这里绑定并落库，之后推理/签到一直用它。
			device := auth.NewTraeCNDeviceIdentity()
			credentials := map[string]interface{}{
				"upstream_type": auth.UpstreamTraeCN,
				"refresh_token": refreshToken,
				"traecn_host":   host,
				"plan_type":     "traecn",
			}
			for key, value := range device.CredentialUpdates() {
				credentials[key] = value
			}
			if len(models) > 0 {
				credentials["models"] = models
			}
			// Record the setting even when the administrator leaves the list
			// empty. This is distinct from an old row whose generic `models`
			// field still needs to be treated as a legacy allowlist.
			credentials[auth.TraeCNModelAllowlistCredentialKey] = models
			credentials[auth.TraeCNModelAllowlistSetCredentialKey] = true
			id, inserted, insertErr := h.db.InsertAccountWithUpstreamIfRefreshTokenAbsent(ctx, accountName, "trae", auth.UpstreamTraeCN, refreshToken, credentials, proxyURL)
			if insertErr != nil {
				item.Error = insertErr.Error()
				items[index] = item
				return
			}
			if !inserted {
				item.Error = "refresh_token 已存在"
				items[index] = item
				return
			}
			item.ID, item.Stored = id, true
			mu.Lock()
			createdIDs = append(createdIDs, id)
			mu.Unlock()
			if !enabled {
				_ = h.db.SetAccountEnabled(ctx, id, false)
			}

			sem <- struct{}{}
			tokenCtx, tokenCancel := context.WithTimeout(ctx, auth.TraeCNExchangeTimeout)
			token, exchangeErr := auth.ExchangeTraeCNRefreshToken(tokenCtx, refreshToken, host, proxyURL, strconv.FormatInt(id, 10))
			tokenCancel()
			<-sem
			if exchangeErr == nil {
				updates := map[string]any{
					"upstream_type": auth.UpstreamTraeCN,
					"access_token":  token.AccessToken,
					"refresh_token": refreshToken,
					"expires_at":    token.ExpiresAt.UTC().Format(time.RFC3339),
				}
				if token.RefreshToken != "" {
					updates["refresh_token"] = token.RefreshToken
				}
				if token.UserID != "" {
					updates["traecn_user_id"] = token.UserID
					updates["account_id"] = token.UserID
					updates["email"] = token.UserID
					item.UserID = token.UserID
				}
				newGeneration, applied, persistErr := h.db.UpdateAccountCredentialsCAS(ctx, id, 1, updates)
				if persistErr != nil {
					item.OK = false
					item.Warning = "AT 已兑换，但凭据持久化失败: " + persistErr.Error()
				} else if !applied {
					item.OK = false
					item.Warning = fmt.Sprintf("AT 已兑换，但账号凭据已被修改，未覆盖新凭据（当前 generation=%d）", newGeneration)
				} else {
					item.OK = true
				}
			} else {
				item.OK = false
				item.Warning = "RT 已保存，但兑换 AT 失败: " + exchangeErr.Error()
				_ = h.db.UpdateCredentials(ctx, id, map[string]interface{}{"traecn_sync_error": exchangeErr.Error()})
				_ = h.db.SetError(ctx, id, exchangeErr.Error())
			}
			items[index] = item
		}()
	}
	wg.Wait()
	if len(createdIDs) > 0 {
		h.db.BatchInsertAccountEventsAsync(createdIDs, "added", "manual_traecn")
		for _, id := range createdIDs {
			if err := h.store.LoadAccountByID(ctx, id); err != nil {
				continue
			}
			if !enabled {
				h.store.ApplyAccountEnabled(id, false)
			}
		}
		if err := h.bindImportedAccountGroups(ctx, createdIDs, groupIDs); err != nil {
			for index := range items {
				if items[index].ID > 0 && items[index].Stored {
					items[index].Warning = strings.TrimSpace(strings.TrimSuffix(items[index].Warning, ".")) + "；分组绑定失败: " + err.Error()
				}
			}
		}
	}
	success, failed := 0, 0
	for _, item := range items {
		if item.Stored {
			success++
		} else {
			failed++
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("已保存 %d 个 Trae CN RT，失败 %d 个", success, failed),
		"total":   len(items), "success": success, "failed": failed, "items": items,
		"group_ids": groupIDs, "host": host,
	})
}

func (h *Handler) RefreshTraeCNAccount(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), auth.TraeCNExchangeTimeout)
	defer cancel()
	if err := h.store.RefreshTraeCNAccountByID(ctx, id); err != nil {
		writeError(c, http.StatusBadGateway, "Trae CN 刷新失败: "+err.Error())
		return
	}
	writeMessage(c, http.StatusOK, "Trae CN 账号刷新成功")
}

// UpdateTraeCNAccount updates non-secret Trae CN account settings. RT/AT are
// intentionally not accepted here; credential rotation uses the add/import
// flow so every change is explicit and auditable.
func (h *Handler) UpdateTraeCNAccount(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}
	var req updateTraeCNAccountRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	name := security.SanitizeInput(strings.TrimSpace(req.Name))
	proxyURL := security.SanitizeInput(strings.TrimSpace(req.ProxyURL))
	if security.ContainsXSS(name) || security.ContainsSQLInjection(name) || utf8.RuneCountInString(name) > 100 {
		writeError(c, http.StatusBadRequest, "名称包含非法字符或长度超过100字符")
		return
	}
	if err := security.ValidateProxyURL(proxyURL); err != nil {
		writeError(c, http.StatusBadRequest, "代理URL无效")
		return
	}
	host, err := auth.NormalizeTraeCNHost(req.Host)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	var models []string
	if req.Models != nil {
		models = auth.NormalizeAccountModels(*req.Models)
	}
	if len(models) > 200 {
		writeError(c, http.StatusBadRequest, "模型数量不能超过 200")
		return
	}
	for _, model := range models {
		if err := security.ValidateModelName(model); err != nil {
			writeError(c, http.StatusBadRequest, fmt.Sprintf("模型名称无效: %s", model))
			return
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	row, err := h.db.GetAccountByID(ctx, id)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no rows") {
			writeError(c, http.StatusNotFound, "账号不存在")
			return
		}
		writeInternalError(c, err)
		return
	}
	if !strings.EqualFold(strings.TrimSpace(row.GetCredential("upstream_type")), auth.UpstreamTraeCN) {
		writeError(c, http.StatusBadRequest, "仅 Trae CN 账号支持该设置")
		return
	}
	if req.Models == nil {
		// The current UI no longer exposes a hand-maintained model field. Keep
		// any existing allowlist when editing unrelated account settings; the
		// upstream catalog is refreshed separately by the sync action.
		if h.store != nil {
			if account := h.store.FindByID(id); account != nil {
				models = account.TraeCNConfiguredModelAllowlist()
			}
		}
		if models == nil {
			models = auth.NormalizeAccountModels(row.GetCredentialStringSlice(auth.TraeCNModelAllowlistCredentialKey))
			if len(models) == 0 && !row.GetCredentialBool(auth.TraeCNModelAllowlistSetCredentialKey) &&
				len(row.GetCredentialStringSlice(auth.TraeCNUpstreamModelsCredentialKey)) == 0 {
				// Pre-catalog rows stored the optional narrowing list in the
				// generic models field.
				models = auth.NormalizeAccountModels(row.GetCredentialStringSlice("models"))
			}
		}
	}
	groupIDs, err := h.resolveTraeCNGroupIDs(ctx, req.GroupIDs)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	effectiveModels := models
	if account := h.store.FindByID(id); account != nil {
		effectiveModels = account.TraeCNModelsForAllowlist(models)
	}
	credentials := map[string]interface{}{
		"upstream_type": auth.UpstreamTraeCN,
		"traecn_host":   host,
		"plan_type":     "traecn",
		// Keep the generic projection for older clients, while the dedicated
		// field records that this list is an optional account-level narrowing.
		"models":                                  effectiveModels,
		auth.TraeCNModelAllowlistCredentialKey:    models,
		auth.TraeCNModelAllowlistSetCredentialKey: true,
	}
	if err := h.db.UpdateCredentials(ctx, id, credentials); err != nil {
		writeInternalError(c, err)
		return
	}
	if err := h.db.UpdateAccountProxyURL(ctx, id, proxyURL); err != nil {
		writeInternalError(c, err)
		return
	}
	if err := h.db.UpdateAccountName(ctx, id, name); err != nil {
		writeInternalError(c, err)
		return
	}
	if h.store != nil {
		h.store.ApplyTraeCNConfig(id, host, models, proxyURL)
	}
	if err := h.bindImportedAccountGroups(ctx, []int64{id}, groupIDs); err != nil {
		writeError(c, http.StatusInternalServerError, "分组绑定失败: "+err.Error())
		return
	}
	h.db.InsertAccountEventAsync(id, "updated", "manual_traecn")
	writeMessage(c, http.StatusOK, "Trae CN 账号设置已更新")
}

// TriggerTraeCNCheckin 手动触发一次积分签到（供管理台按钮/排障使用）。
// 自动签到每天一次、时刻随机；手动触发同样把当天标记为已签到，避免与调度器重复领取。
func (h *Handler) TriggerTraeCNCheckin(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}
	if h == nil || h.store == nil {
		writeError(c, http.StatusServiceUnavailable, "账号服务不可用")
		return
	}
	account := h.store.FindByID(id)
	if account == nil || !account.IsTraeCNAPI() {
		writeError(c, http.StatusNotFound, "Trae CN 账号不存在")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	outcome, checkinErr := proxy.RunTraeCNCheckin(ctx, h.store, account, "")
	message := outcome.Message()
	snapshot := auth.TraeCNCheckinSnapshot{Date: time.Now().Format("2006-01-02"), At: time.Now(), Credits: outcome.Status.Credits, Result: message}
	if checkinErr != nil {
		snapshot.Result = "签到失败: " + checkinErr.Error()
		snapshot.Credits = 0
	}
	h.store.PersistTraeCNCheckin(id, snapshot)
	if h.db != nil {
		h.db.InsertAccountEventAsync(id, "traecn_checkin", snapshot.Result)
	}
	if checkinErr != nil {
		writeError(c, http.StatusBadGateway, snapshot.Result)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"message":    message,
		"checked_in": outcome.CheckedIn,
		"claimed":    outcome.Claimed,
		"skipped":    outcome.Skipped,
		"credits":    outcome.Status.Credits,
		"extra":      outcome.Status.Extra,
		"date":       snapshot.Date,
	})
}
