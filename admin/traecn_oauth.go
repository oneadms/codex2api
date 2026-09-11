package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

// Trae CN 账号的三种添加方式共用同一套凭据落库逻辑：
//
//	OAuth 授权（PKCE） -> refresh_token + access_token；
//	RT 粘贴/批量导入   -> refresh_token（由 ExchangeToken 换 AT）；
//	JSON 导入/导出     -> 上面两种的合集，方便在部署之间迁移账号。

type traeCNOAuthStartRequest struct {
	Name         string          `json:"name"`
	Host         string          `json:"host"`
	ProxyURL     string          `json:"proxy_url"`
	CallbackBase string          `json:"callback_base"`
	GroupIDs     json.RawMessage `json:"group_ids"`
	Enabled      *bool           `json:"enabled"`
}

type traeCNOAuthCompleteRequest struct {
	LoginID  string `json:"login_id"`
	Callback string `json:"callback"`
}

type traeCNOAuthClaimRequest struct {
	LoginID  string          `json:"login_id"`
	Name     string          `json:"name"`
	Host     string          `json:"host"`
	ProxyURL string          `json:"proxy_url"`
	GroupIDs json.RawMessage `json:"group_ids"`
	Enabled  *bool           `json:"enabled"`
}

// traeCNCallbackBase 推导 Trae 授权回调地址。Trae 授权页只接受
// http://127.0.0.1:<port>/authorize（实测其它写法一律"登录失败 网络错误"），
// 因此这里只决定端口：
//   - 调用方显式给了回调基地址 / 端口，就按它推导；
//   - 否则用管理台自身的来源（反向代理下按 X-Forwarded-* 还原）：管理台跑在
//     127.0.0.1 上时端口一致，本机网关能真正收到回调并自动完成；
//   - 其余情况落到默认端口，靠用户把地址栏链接粘回来提交。
func traeCNCallbackBase(c *gin.Context, explicit string) string {
	if base := strings.TrimSpace(explicit); base != "" {
		return base
	}
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	if forwarded := strings.TrimSpace(c.GetHeader("X-Forwarded-Proto")); forwarded != "" {
		scheme = strings.ToLower(strings.Split(forwarded, ",")[0])
	}
	host := strings.TrimSpace(c.GetHeader("X-Forwarded-Host"))
	if host == "" {
		host = c.Request.Host
	} else {
		host = strings.TrimSpace(strings.Split(host, ",")[0])
	}
	return fmt.Sprintf("%s://%s", scheme, host)
}

// StartTraeCNOAuth 创建一次授权会话，返回给前端打开的授权链接。
func (h *Handler) StartTraeCNOAuth(c *gin.Context) {
	var req traeCNOAuthStartRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	proxyURL := security.SanitizeInput(strings.TrimSpace(req.ProxyURL))
	if err := security.ValidateProxyURL(proxyURL); err != nil {
		writeError(c, http.StatusBadRequest, "代理URL无效")
		return
	}
	callbackBase := traeCNCallbackBase(c, req.CallbackBase)
	ctx, cancel := context.WithTimeout(c.Request.Context(), auth.TraeCNOAuthTimeout*3)
	defer cancel()

	start, err := auth.StartTraeCNOAuth(ctx, callbackBase, proxyURL)
	if err != nil {
		writeError(c, http.StatusBadGateway, "发起 Trae CN 授权失败: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"login_id":         start.LoginID,
		"login_trace_id":   start.LoginTraceID,
		"verification_uri": start.VerificationURI,
		"callback_url":     start.CallbackURL,
		"login_host":       start.LoginHost,
		"expires_in":       start.ExpiresIn,
		"interval_seconds": start.IntervalSeconds,
	})
}

// GetTraeCNOAuthStatus 供前端轮询授权结果。
func (h *Handler) GetTraeCNOAuthStatus(c *gin.Context) {
	loginID := strings.TrimSpace(c.Query("login_id"))
	status, ok := auth.TraeCNOAuthStatusFor(loginID)
	if !ok {
		writeError(c, http.StatusNotFound, "授权会话不存在或已过期，请重新发起")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"login_id":         status.LoginID,
		"state":            status.State,
		"verification_uri": status.VerificationURI,
		"callback_url":     status.CallbackURL,
		"expires_in":       status.ExpiresIn,
		"interval_seconds": status.IntervalSeconds,
		"message":          status.Message,
		"account":          traeCNOAuthPublicAccount(status.Account),
	})
}

// CompleteTraeCNOAuth 处理手动粘贴的回调链接（自动回调不可用时使用）。
func (h *Handler) CompleteTraeCNOAuth(c *gin.Context) {
	var req traeCNOAuthCompleteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	loginID := strings.TrimSpace(req.LoginID)
	callback := strings.TrimSpace(req.Callback)
	if loginID == "" || callback == "" {
		writeError(c, http.StatusBadRequest, "login_id 与 callback 都是必填字段")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), auth.TraeCNOAuthTimeout*2)
	defer cancel()
	account, err := auth.CompleteTraeCNOAuth(ctx, loginID, callback)
	if err != nil {
		writeError(c, http.StatusBadGateway, "Trae CN 授权失败: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Trae CN 授权成功", "account": account})
}

// TraeCNOAuthCallback 是浏览器授权后跳回的公开端点：把授权码交给对应会话。
// 它会自动完成兑换，前端轮询到 state=ready 后调用 claim 建号。
func (h *Handler) TraeCNOAuthCallback(c *gin.Context) {
	values := c.Request.URL.Query()
	traceID := strings.TrimSpace(values.Get("login_trace_id"))
	if traceID == "" {
		traceID = strings.TrimSpace(values.Get("loginTraceId"))
	}
	loginID := strings.TrimSpace(values.Get("state"))
	if loginID == "" {
		loginID = strings.TrimSpace(values.Get("login_id"))
	}
	key := traceID
	if key == "" {
		key = loginID
	}
	c.Header("Cache-Control", "no-store")
	if key == "" {
		c.Data(http.StatusBadRequest, "text/html; charset=utf-8", traeCNOAuthCallbackPage("参数缺失", "回调链接里没有 login_trace_id，请回到管理台重新发起授权。", false))
		return
	}
	session := auth.FindTraeCNOAuthSession(key)
	if session == nil {
		c.Data(http.StatusNotFound, "text/html; charset=utf-8", traeCNOAuthCallbackPage("会话已过期", "这次授权会话已经失效，请在管理台重新发起一次 Trae CN 授权。", false))
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), auth.TraeCNOAuthTimeout*2)
	defer cancel()
	if _, err := auth.CompleteTraeCNOAuth(ctx, key, c.Request.URL.RequestURI()); err != nil {
		c.Data(http.StatusOK, "text/html; charset=utf-8", traeCNOAuthCallbackPage("授权失败", err.Error(), false))
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", traeCNOAuthCallbackPage("授权成功", "可以关闭本页面，回到管理台完成添加。", true))
}

// traeCNOAuthPublicAccount 去掉返回给浏览器的凭据明文：建号用的是服务端会话里的
// 账号对象（claim 直接从会话读），前端只需要展示账号身份。
func traeCNOAuthPublicAccount(account *auth.TraeCNOAuthAccount) gin.H {
	if account == nil {
		return nil
	}
	return gin.H{
		"email":        account.Email,
		"user_id":      account.UserID,
		"plan_type":    account.PlanType,
		"login_host":   account.LoginHost,
		"login_region": account.LoginRegion,
		"user_tag":     account.UserTag,
		"expires_at":   account.ExpiresAt,
		"warning":      account.Warning,
		"device":       account.Device,
	}
}

// ClaimTraeCNOAuthAccount 把已完成的授权会话落成账号。
func (h *Handler) ClaimTraeCNOAuthAccount(c *gin.Context) {
	var req traeCNOAuthClaimRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	status, ok := auth.TraeCNOAuthStatusFor(strings.TrimSpace(req.LoginID))
	if !ok {
		writeError(c, http.StatusNotFound, "授权会话不存在或已过期，请重新发起")
		return
	}
	if status.State != "ready" || status.Account == nil {
		writeError(c, http.StatusBadRequest, fmt.Sprintf("授权尚未完成（当前状态: %s）", status.State))
		return
	}
	name := security.SanitizeInput(strings.TrimSpace(req.Name))
	if security.ContainsXSS(name) || security.ContainsSQLInjection(name) {
		writeError(c, http.StatusBadRequest, "名称包含非法字符")
		return
	}
	proxyURL := security.SanitizeInput(strings.TrimSpace(req.ProxyURL))
	if err := security.ValidateProxyURL(proxyURL); err != nil {
		writeError(c, http.StatusBadRequest, "代理URL无效")
		return
	}
	host, err := auth.NormalizeTraeCNHost(req.Host)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	account := status.Account
	if strings.TrimSpace(account.RefreshToken) == "" {
		writeError(c, http.StatusBadGateway, "授权结果里没有 refresh_token，无法建号")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	groupIDs, err := h.resolveTraeCNGroupIDs(ctx, req.GroupIDs)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	if name == "" {
		name = "traecn-oauth"
	}
	id, inserted, _, err := h.insertTraeCNAccountFromCredentials(ctx, name, host, proxyURL, account, h.loadTraeCNBindingIndex(ctx))
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if !inserted {
		writeError(c, http.StatusConflict, "该 refresh_token 已存在，无需重复添加")
		return
	}
	if !enabled {
		_ = h.db.SetAccountEnabled(ctx, id, false)
	}
	h.finalizeTraeCNImportedAccounts(ctx, []int64{id}, groupIDs, "oauth_traecn", enabled)
	c.JSON(http.StatusOK, gin.H{
		"message": "Trae CN 账号已通过 OAuth 添加",
		"id":      id,
		"email":   account.Email,
		"user_id": account.UserID,
	})
}

// traeCNBindingIndex 记录号池里已占用的设备码与 Trae 账号 ID。导入必须经过它，
// 否则把别处用着的设备码原样搬进来就会造出"一个设备码挂多个账号"——Trae 的风控
// 正是按这个判定同机批量登录。
//
// 注意判定维度是 **Trae 账号**（user id）而不是"设备码有没有被用过"：同一个账号
// 在同一个设备码下本来就应该这样（重新导入、迁移都是同一个账号同一台设备）；
// 只有不同 Trae 账号撞到同一个设备码才是要拆开的。
type traeCNBindingIndex struct {
	machine map[string]traeCNBindingHolder
	device  map[string]traeCNBindingHolder
	user    map[string]int64
}

type traeCNBindingHolder struct {
	AccountID int64
	UserID    string
}

func newTraeCNBindingIndex(bindings []database.TraeCNAccountBinding) *traeCNBindingIndex {
	index := &traeCNBindingIndex{
		machine: map[string]traeCNBindingHolder{},
		device:  map[string]traeCNBindingHolder{},
		user:    map[string]int64{},
	}
	for _, binding := range bindings {
		index.record(binding.AccountID, binding.UserID, auth.TraeCNDeviceIdentity{MachineID: binding.MachineID, DeviceID: binding.DeviceID})
	}
	return index
}

func (idx *traeCNBindingIndex) record(accountID int64, userID string, identity auth.TraeCNDeviceIdentity) {
	if idx == nil || accountID <= 0 {
		return
	}
	holder := traeCNBindingHolder{AccountID: accountID, UserID: strings.TrimSpace(userID)}
	if machineID := strings.TrimSpace(identity.MachineID); machineID != "" {
		if _, taken := idx.machine[machineID]; !taken {
			idx.machine[machineID] = holder
		}
	}
	if deviceID := strings.TrimSpace(identity.DeviceID); deviceID != "" {
		if _, taken := idx.device[deviceID]; !taken {
			idx.device[deviceID] = holder
		}
	}
	if value := strings.TrimSpace(userID); value != "" {
		if _, taken := idx.user[value]; !taken {
			idx.user[value] = accountID
		}
	}
}

// holderOf 返回设备码当前的占用者；没有占用返回零值。
func (idx *traeCNBindingIndex) holderOf(identity auth.TraeCNDeviceIdentity) traeCNBindingHolder {
	if idx == nil {
		return traeCNBindingHolder{}
	}
	if machineID := strings.TrimSpace(identity.MachineID); machineID != "" {
		if holder, taken := idx.machine[machineID]; taken {
			return holder
		}
	}
	if deviceID := strings.TrimSpace(identity.DeviceID); deviceID != "" {
		return idx.device[deviceID]
	}
	return traeCNBindingHolder{}
}

// reserve 返回可以安全落库的设备码。只有当这份设备码被 **另一个 Trae 账号** 占用时
// 才重新分配：占用者与来者是同一个 user id 时沿用原码（同一账号同一设备，正常）；
// 来者没有 user id（无从判断）时按冲突处理，宁可分错也不共用。
func (idx *traeCNBindingIndex) reserve(identity auth.TraeCNDeviceIdentity, incomingUserID string) (auth.TraeCNDeviceIdentity, int64, bool) {
	if idx == nil || identity.Empty() {
		return identity, 0, false
	}
	holder := idx.holderOf(identity)
	if holder.AccountID <= 0 {
		return identity, 0, false
	}
	incoming := strings.TrimSpace(incomingUserID)
	if incoming != "" && holder.UserID != "" && strings.EqualFold(holder.UserID, incoming) {
		// 同一个 Trae 账号：沿用同一设备码是正确行为，不做改动。
		return identity, 0, false
	}
	return auth.NewTraeCNDeviceIdentity(), holder.AccountID, true
}

// userHolder 返回已经存在的同一个 Trae 账号（user id）所属账号 ID；没有则 0。
func (idx *traeCNBindingIndex) userHolder(userID string) int64 {
	if idx == nil {
		return 0
	}
	return idx.user[strings.TrimSpace(userID)]
}

// loadTraeCNBindingIndex 读出当前号池绑定；读取失败时返回空索引（不阻断导入）。
func (h *Handler) loadTraeCNBindingIndex(ctx context.Context) *traeCNBindingIndex {
	if h == nil || h.db == nil {
		return newTraeCNBindingIndex(nil)
	}
	bindings, err := h.db.ListTraeCNAccountBindings(ctx)
	if err != nil {
		log.Printf("[TRAECN] 读取设备码绑定失败，导入将不做占用检查: %v", err)
		return newTraeCNBindingIndex(nil)
	}
	return newTraeCNBindingIndex(bindings)
}

// insertTraeCNAccountFromCredentials 落库一个凭据已经齐备的 Trae CN 账号。
// 返回的 reassignedFrom 非 0 表示数据里带的设备码已被该账号占用、本次重新分配了。
func (h *Handler) insertTraeCNAccountFromCredentials(ctx context.Context, name, host, proxyURL string, account *auth.TraeCNOAuthAccount, codes *traeCNBindingIndex) (id int64, inserted bool, reassignedFrom int64, err error) {
	// 导入数据里带的设备码先去重：已在号池里的（迁移前那份账号还在用）就重新分配，
	// 避免出现两个账号共用一个设备码——那正是风控判定"同机批量登录"的依据。
	device := account.Device
	if device.Empty() {
		device = auth.NewTraeCNDeviceIdentity()
	}
	device, reassignedFrom, _ = codes.reserve(device, account.UserID)
	if reassignedFrom > 0 {
		log.Printf("[TRAECN] 设备码已被账号 #%d（不同 Trae 账号）占用，为 %q 重新分配一份", reassignedFrom, name)
	}
	credentials := map[string]interface{}{
		"upstream_type":                           auth.UpstreamTraeCN,
		"refresh_token":                           account.RefreshToken,
		"traecn_host":                             host,
		"plan_type":                               "traecn",
		auth.TraeCNModelAllowlistCredentialKey:    []string{},
		auth.TraeCNModelAllowlistSetCredentialKey: true,
	}
	for key, value := range device.CredentialUpdates() {
		credentials[key] = value
	}
	if account.AccessToken != "" {
		credentials["access_token"] = account.AccessToken
	}
	if !account.ExpiresAt.IsZero() {
		credentials["expires_at"] = account.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if account.UserID != "" {
		credentials["traecn_user_id"] = account.UserID
		credentials["account_id"] = account.UserID
		if account.Email == "" {
			credentials["email"] = account.UserID
		}
	}
	if account.Email != "" {
		credentials["email"] = account.Email
	}
	if account.UserTag != "" {
		credentials["traecn_user_tag"] = account.UserTag
	}
	if account.LoginRegion != "" {
		credentials["traecn_login_region"] = account.LoginRegion
	}
	id, inserted, err = h.db.InsertAccountWithUpstreamIfRefreshTokenAbsent(ctx, name, "trae", auth.UpstreamTraeCN, account.RefreshToken, credentials, proxyURL)
	if inserted {
		// 登记进索引：同一次导入里的后续条目不会再用到这份设备码。
		codes.record(id, account.UserID, device)
	}
	return id, inserted, reassignedFrom, err
}

// finalizeTraeCNImportedAccounts 统一处理导入后的收尾动作（事件、内存态、分组）。
func (h *Handler) finalizeTraeCNImportedAccounts(ctx context.Context, ids []int64, groupIDs []int64, source string, enabled bool) {
	if len(ids) == 0 {
		return
	}
	h.db.BatchInsertAccountEventsAsync(ids, "added", source)
	for _, id := range ids {
		if err := h.store.LoadAccountByID(ctx, id); err != nil {
			continue
		}
		if !enabled {
			h.store.ApplyAccountEnabled(id, false)
		}
	}
	_ = h.bindImportedAccountGroups(ctx, ids, groupIDs)
}

func traeCNOAuthHTMLEscape(raw string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return replacer.Replace(raw)
}

func traeCNOAuthCallbackPage(title, message string, ok bool) []byte {
	tone := "#ef4444"
	if ok {
		tone = "#22c55e"
	}
	page := fmt.Sprintf(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1"/>
<title>Trae CN 授权 - codex2api</title>
<style>
:root{color-scheme:dark}
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
background:#0b1120;color:#e5edf8;font-family:system-ui,-apple-system,"Segoe UI",sans-serif}
.card{max-width:420px;padding:32px;border-radius:18px;background:#111827;border:1px solid rgba(148,163,184,.24);text-align:center}
.dot{width:44px;height:44px;margin:0 auto 16px;border-radius:999px;background:%s;opacity:.18}
h1{margin:0 0 8px;font-size:18px}
p{margin:0;color:#9aa7bc;font-size:14px;line-height:1.6}
</style></head>
<body><div class="card"><div class="dot"></div><h1>%s</h1><p>%s</p></div></body></html>`,
		tone, traeCNOAuthHTMLEscape(title), traeCNOAuthHTMLEscape(message))
	return []byte(page)
}

// TraeCNExportAccount 是导出/导入 JSON 里的单个账号。
type TraeCNExportAccount struct {
	Name         string   `json:"name,omitempty"`
	Email        string   `json:"email,omitempty"`
	UserID       string   `json:"user_id,omitempty"`
	Host         string   `json:"host,omitempty"`
	ProxyURL     string   `json:"proxy_url,omitempty"`
	AccessToken  string   `json:"access_token,omitempty"`
	RefreshToken string   `json:"refresh_token,omitempty"`
	ExpiresAt    string   `json:"expires_at,omitempty"`
	PlanType     string   `json:"plan_type,omitempty"`
	UserTag      string   `json:"user_tag,omitempty"`
	LoginRegion  string   `json:"login_region,omitempty"`
	Models       []string `json:"models,omitempty"`
	GroupIDs     []int64  `json:"group_ids,omitempty"`
	Enabled      bool     `json:"enabled"`
	Status       string   `json:"status,omitempty"`
	// 设备码随账号一起迁移，避免导入后重新分配导致换设备。
	MachineID string `json:"machine_id,omitempty"`
	DeviceID  string `json:"device_id,omitempty"`
}

// TraeCNExportPayload 是导出的文档结构；导入时同时接受裸数组与 accounts 包裹。
type TraeCNExportPayload struct {
	Version    int                   `json:"version"`
	ExportedAt string                `json:"exported_at"`
	Channel    string                `json:"channel"`
	Accounts   []TraeCNExportAccount `json:"accounts"`
}

// ExportTraeCNAccounts 导出 Trae CN 账号为 JSON（含明文 refresh_token）。
// 与 /accounts/export、Grok 导出同一道门禁：远程迁移必须先配好管理密钥并开启开关。
func (h *Handler) ExportTraeCNAccounts(c *gin.Context) {
	if c.Query("remote") == "true" {
		if !h.hasConfiguredAdminSecret(c.Request.Context()) {
			writeError(c, http.StatusForbidden, "请先设置管理密钥，再启用远程迁移")
			return
		}
		if !h.store.GetAllowRemoteMigration() {
			writeError(c, http.StatusForbidden, "远程迁移未启用，请在系统设置中开启")
			return
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	rows, err := h.db.ListActiveByChannel(ctx, auth.UpstreamTraeCN)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	requested := parseExportIDSet(c.Query("ids"))
	payload := TraeCNExportPayload{Version: 1, ExportedAt: time.Now().UTC().Format(time.RFC3339), Channel: auth.UpstreamTraeCN}
	for _, row := range rows {
		if !strings.EqualFold(row.GetCredential("upstream_type"), auth.UpstreamTraeCN) {
			continue
		}
		if requested != nil && !requested[row.ID] {
			continue
		}
		payload.Accounts = append(payload.Accounts, traeCNExportAccountFromRow(row))
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		writeInternalError(c, err)
		return
	}
	filename := fmt.Sprintf("traecn-accounts-%s.json", time.Now().UTC().Format("20060102-150405"))
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

func traeCNExportAccountFromRow(row *database.AccountRow) TraeCNExportAccount {
	enabled := true
	if row != nil {
		enabled = row.Enabled
	}
	account := TraeCNExportAccount{Enabled: enabled}
	if row == nil {
		return account
	}
	account.Name = row.Name
	account.Status = row.Status
	account.Email = row.GetCredential("email")
	account.UserID = row.GetCredential("traecn_user_id")
	account.Host = row.GetCredential("traecn_host")
	account.ProxyURL = row.ProxyURL
	account.AccessToken = row.GetCredential("access_token")
	account.RefreshToken = row.GetCredential("refresh_token")
	account.ExpiresAt = row.GetCredential("expires_at")
	account.PlanType = row.GetCredential("plan_type")
	account.UserTag = row.GetCredential("traecn_user_tag")
	account.LoginRegion = row.GetCredential("traecn_login_region")
	account.Models = row.GetCredentialStringSlice(auth.TraeCNModelAllowlistCredentialKey)
	// 导出实际生效的设备码（老账号可能还没落库绑定值），迁移后才能不换设备。
	deviceIdentity := auth.TraeCNEffectiveDeviceIdentity(row)
	account.MachineID = deviceIdentity.MachineID
	account.DeviceID = deviceIdentity.DeviceID
	return account
}

// traeCNImportJSONAccount 是导入时接受的宽松结构：字段名兼容多种写法。
type traeCNImportJSONAccount struct {
	Name         string          `json:"name"`
	Email        string          `json:"email"`
	UserID       string          `json:"user_id"`
	Host         string          `json:"host"`
	ProxyURL     string          `json:"proxy_url"`
	AccessToken  string          `json:"access_token"`
	RefreshToken string          `json:"refresh_token"`
	ExpiresAt    string          `json:"expires_at"`
	PlanType     string          `json:"plan_type"`
	UserTag      string          `json:"user_tag"`
	LoginRegion  string          `json:"login_region"`
	Models       []string        `json:"models"`
	GroupIDs     json.RawMessage `json:"group_ids"`
	Enabled      *bool           `json:"enabled"`
	MachineID    string          `json:"machine_id"`
	DeviceID     string          `json:"device_id"`
}

// TraeCNImportJSON 导入 Trae CN 账号 JSON。支持三种输入：
//
//	{"version":1,"accounts":[...]}  导出文件原样回灌；
//	[{...},{...}]                   裸数组；
//	{"refresh_token":"..."}         单个账号对象。
func (h *Handler) TraeCNImportJSON(c *gin.Context) {
	var req struct {
		Accounts       json.RawMessage `json:"accounts"`
		JSON           string          `json:"json"`
		Name           string          `json:"name"`
		Host           string          `json:"host"`
		ProxyURL       string          `json:"proxy_url"`
		GroupIDs       json.RawMessage `json:"group_ids"`
		Enabled        *bool           `json:"enabled"`
		AllowDuplicate bool            `json:"allow_duplicate"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	raw := strings.TrimSpace(req.JSON)
	if raw == "" && len(req.Accounts) > 0 {
		raw = strings.TrimSpace(string(req.Accounts))
	}
	if raw == "" {
		writeError(c, http.StatusBadRequest, "json 或 accounts 字段必填")
		return
	}
	items, err := parseTraeCNImportJSON(raw)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	if len(items) == 0 {
		writeError(c, http.StatusBadRequest, "JSON 里没有可用账号")
		return
	}
	if len(items) > traeCNImportMaxAccounts {
		writeError(c, http.StatusBadRequest, fmt.Sprintf("单次最多导入 %d 个 Trae CN 账号", traeCNImportMaxAccounts))
		return
	}
	defaultHost, err := auth.NormalizeTraeCNHost(traeCNFirstNonEmpty(req.Host, items[0].Host))
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	defaultProxy := security.SanitizeInput(strings.TrimSpace(req.ProxyURL))
	if err := security.ValidateProxyURL(defaultProxy); err != nil {
		writeError(c, http.StatusBadRequest, "代理URL无效")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 300*time.Second)
	defer cancel()
	groupIDs, err := h.resolveTraeCNGroupIDs(ctx, firstRawMessage(req.GroupIDs, nil))
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	existing, err := h.db.GetAllRefreshTokens(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	codes := h.loadTraeCNBindingIndex(ctx)

	results := make([]traeCNImportItem, 0, len(items))
	createdIDs := make([]int64, 0, len(items))
	for index, item := range items {
		result := traeCNImportItem{Index: int64(index + 1)}
		result.UserID = item.UserID
		result.Name = item.Name
		refreshToken := strings.TrimSpace(item.RefreshToken)
		if refreshToken == "" {
			result.Error = "缺少 refresh_token"
			results = append(results, result)
			continue
		}
		if existing[refreshToken] && !req.AllowDuplicate {
			result.Error = "refresh_token 已存在"
			results = append(results, result)
			continue
		}
		host := defaultHost
		if strings.TrimSpace(item.Host) != "" {
			normalized, hostErr := auth.NormalizeTraeCNHost(item.Host)
			if hostErr != nil {
				result.Error = hostErr.Error()
				results = append(results, result)
				continue
			}
			host = normalized
		}
		proxyURL := defaultProxy
		if strings.TrimSpace(item.ProxyURL) != "" {
			candidate := security.SanitizeInput(strings.TrimSpace(item.ProxyURL))
			if err := security.ValidateProxyURL(candidate); err != nil {
				result.Error = "代理URL无效"
				results = append(results, result)
				continue
			}
			proxyURL = candidate
		}
		name := security.SanitizeInput(strings.TrimSpace(item.Name))
		if name == "" {
			name = security.SanitizeInput(strings.TrimSpace(req.Name))
		}
		if name == "" {
			name = "traecn"
		}
		account := &auth.TraeCNOAuthAccount{
			RefreshToken: refreshToken,
			AccessToken:  strings.TrimSpace(item.AccessToken),
			Email:        strings.TrimSpace(item.Email),
			UserID:       strings.TrimSpace(item.UserID),
			UserTag:      strings.TrimSpace(item.UserTag),
			LoginRegion:  strings.TrimSpace(item.LoginRegion),
			Device: auth.TraeCNDeviceIdentity{
				MachineID: strings.TrimSpace(item.MachineID),
				DeviceID:  strings.TrimSpace(item.DeviceID),
			},
		}
		if parsed, parseErr := time.Parse(time.RFC3339, strings.TrimSpace(item.ExpiresAt)); parseErr == nil {
			account.ExpiresAt = parsed
		}
		holder := codes.userHolder(item.UserID)
		id, inserted, reassignedFrom, insertErr := h.insertTraeCNAccountFromCredentials(ctx, name, host, proxyURL, account, codes)
		if insertErr != nil {
			result.Error = insertErr.Error()
			results = append(results, result)
			continue
		}
		if !inserted {
			result.Error = "refresh_token 已存在"
			results = append(results, result)
			continue
		}
		warnings := make([]string, 0, 2)
		if reassignedFrom > 0 {
			warnings = append(warnings, fmt.Sprintf("设备码已被另一个 Trae 账号（账号 #%d）占用，已为该账号重新分配一份", reassignedFrom))
		}
		if holder > 0 {
			warnings = append(warnings, fmt.Sprintf("该 Trae 账号已在号池中（账号 #%d），确认是否重复添加", holder))
		}
		if len(warnings) > 0 {
			result.Warning = strings.Join(warnings, "；")
		}
		result.ID, result.Stored, result.OK = id, true, true
		createdIDs = append(createdIDs, id)
		if len(item.Models) > 0 {
			if err := h.db.UpdateCredentials(ctx, id, map[string]interface{}{
				auth.TraeCNModelAllowlistCredentialKey:    auth.NormalizeAccountModels(item.Models),
				auth.TraeCNModelAllowlistSetCredentialKey: true,
			}); err != nil {
				result.Warning = "模型允许清单写入失败: " + err.Error()
			}
		}
		if item.Enabled != nil && !*item.Enabled {
			_ = h.db.SetAccountEnabled(ctx, id, false)
		}
		results = append(results, result)
	}
	h.finalizeTraeCNImportedAccounts(ctx, createdIDs, groupIDs, "json_import_traecn", enabled)
	success := 0
	for _, item := range results {
		if item.Stored {
			success++
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"message": fmt.Sprintf("已导入 %d 个 Trae CN 账号，失败 %d 个", success, len(results)-success),
		"total":   len(results),
		"success": success,
		"failed":  len(results) - success,
		"items":   results,
		"host":    defaultHost,
	})
}

// traeCNFirstNonEmpty 取第一个非空字符串（导入时用来兜底 host 等字段）。
func traeCNFirstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func firstRawMessage(values ...json.RawMessage) json.RawMessage {
	for _, value := range values {
		if len(value) > 0 {
			return value
		}
	}
	return nil
}

// parseTraeCNImportJSON 解析导入 JSON，兼容导出文件、裸数组、单个对象，以及
// cockpit-tools 一类的 refreshToken/RefreshToken 驼峰写法。
func parseTraeCNImportJSON(raw string) ([]traeCNImportJSONAccount, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("JSON 不能为空")
	}
	if strings.HasPrefix(trimmed, "{") {
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &envelope); err != nil {
			return nil, fmt.Errorf("JSON 解析失败: %w", err)
		}
		if accounts, ok := envelope["accounts"]; ok {
			return decodeTraeCNImportAccounts(accounts)
		}
		if nested, ok := envelope["data"]; ok && strings.HasPrefix(strings.TrimSpace(string(nested)), "[") {
			return decodeTraeCNImportAccounts(nested)
		}
		// 单个账号对象，或 cockpit-tools 的 {trae_accounts:[...]}
		if list, ok := envelope["trae_accounts"]; ok {
			return decodeTraeCNImportAccounts(list)
		}
		item, err := decodeTraeCNImportAccount([]byte(trimmed))
		if err != nil {
			return nil, err
		}
		return []traeCNImportJSONAccount{item}, nil
	}
	if strings.HasPrefix(trimmed, "[") {
		return decodeTraeCNImportAccounts([]byte(trimmed))
	}
	return nil, fmt.Errorf("JSON 必须是对象或数组")
}

func decodeTraeCNImportAccounts(raw []byte) ([]traeCNImportJSONAccount, error) {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("accounts 必须是数组: %w", err)
	}
	items := make([]traeCNImportJSONAccount, 0, len(entries))
	for _, entry := range entries {
		item, err := decodeTraeCNImportAccount(entry)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func decodeTraeCNImportAccount(raw []byte) (traeCNImportJSONAccount, error) {
	var item traeCNImportJSONAccount
	if err := json.Unmarshal(raw, &item); err != nil {
		return item, fmt.Errorf("账号对象解析失败: %w", err)
	}
	// 宽松兜底：refreshToken / RefreshToken / rt / token 等写法。
	if strings.TrimSpace(item.RefreshToken) == "" {
		var generic map[string]any
		if err := json.Unmarshal(raw, &generic); err == nil {
			for _, key := range []string{"refreshToken", "RefreshToken", "refresh_token", "Refresh_Token", "rt", "token", "auth_token"} {
				if value, ok := generic[key]; ok {
					if text := strings.TrimSpace(strings.TrimSpace(fmt.Sprintf("%v", value))); text != "" && text != "<nil>" {
						item.RefreshToken = text
						break
					}
				}
			}
			if strings.TrimSpace(item.AccessToken) == "" {
				for _, key := range []string{"accessToken", "AccessToken", "at"} {
					if value, ok := generic[key]; ok {
						if text := strings.TrimSpace(fmt.Sprintf("%v", value)); text != "" && text != "<nil>" {
							item.AccessToken = text
							break
						}
					}
				}
			}
			if strings.TrimSpace(item.MachineID) == "" {
				for _, key := range []string{"machineId", "machine_id", "x_machine_id"} {
					if value, ok := generic[key]; ok {
						if text := strings.TrimSpace(fmt.Sprintf("%v", value)); text != "" && text != "<nil>" {
							item.MachineID = text
							break
						}
					}
				}
			}
			if strings.TrimSpace(item.DeviceID) == "" {
				for _, key := range []string{"deviceId", "device_id", "x_device_id"} {
					if value, ok := generic[key]; ok {
						if text := strings.TrimSpace(fmt.Sprintf("%v", value)); text != "" && text != "<nil>" {
							item.DeviceID = text
							break
						}
					}
				}
			}
			if strings.TrimSpace(item.UserID) == "" {
				for _, key := range []string{"userId", "UserID", "uid", "user_id"} {
					if value, ok := generic[key]; ok {
						if text := strings.TrimSpace(fmt.Sprintf("%v", value)); text != "" && text != "<nil>" {
							item.UserID = text
							break
						}
					}
				}
			}
		}
	}
	if strings.TrimSpace(item.RefreshToken) == "" {
		return item, fmt.Errorf("JSON 里有账号缺少 refresh_token")
	}
	return item, nil
}
