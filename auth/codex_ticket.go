package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

// 后台自动打票：X-Codex-Turn-State 门票。
//
// 与 codex_turn_state.go 的手工注入互补：
//   - 手工注入：运维把上游铸造的值粘到账号上，一份值 + 模型名单；
//   - 自动打票：后台用合成请求向上游「铸造」门票，优先归属铸造账号；同套餐账号在本地票不可用时可按模型回退到共享池，临期前自动重打。
//
// 门票不是身份：它只影响上游的回合状态校验，不进调度、不影响请求归属。
//
// 门票的形状是一个可校验的信封：base64url 解码后是 57 字节固定头 + N 个 16 字节
// 加密块，首字节恒为 0x80，第 1..9 字节是大端 unix 签发时刻。合格判定不再依赖编码
// 长度（上游编码会变化），采票侧改用探测回答校验（Gemini 版本号须大于 2.5）。
const (
	// CodexTicketStatePrefix 是门票 blob 的固定前缀（解码后首字节 0x80）。
	CodexTicketStatePrefix = "gAAAAA"
	// CodexTicketEnvelopeHeaderBytes 是信封固定头长度（1 字节标记 + 8 字节签发时刻 + 48 字节其余）。
	CodexTicketEnvelopeHeaderBytes = 57
	// CodexTicketBlockBytes 是每个加密块的字节数。
	CodexTicketBlockBytes = 16
	// CodexTicketPersonalBlocks / CodexTicketTeamBlocks 是个人版与团队版的信封块数。
	CodexTicketPersonalBlocks = 10
	CodexTicketTeamBlocks     = 12

	// CodexTicketCredentialKeyPrefix 是门票在账号凭据里的键前缀，后缀为模型名。
	CodexTicketCredentialKeyPrefix = "codex_ticket:"
	// CodexTicketProbeCredentialKeySuffix 是探测结果摘要的键后缀。
	CodexTicketProbeCredentialKeySuffix = ":probe"

	// codexTicketValidity 是本地保守缓存上限，不代表上游保证的票据寿命。
	codexTicketValidity = 240 * time.Second
	// codexTicketIssuedSkew 允许签发时刻比本地时间超前这么多（时钟漂移容差）。
	codexTicketIssuedSkew = 30 * time.Second
	// codexTicketValidityMargin 预留传输与时钟误差，签发后 210 秒即停止使用。
	codexTicketValidityMargin = 30 * time.Second

	// maxCodexTicketBytes 是门票编码后的长度上限（团队版 332，留一个数量级余量）。
	maxCodexTicketBytes = 2048
)

// CodexTicketShape 是门票信封的可校验投影。
type CodexTicketShape struct {
	// Blocks 是加密块数，应与账号套餐推导出的期望值一致。
	Blocks int
	// IssuedAt 是上游签发该门票的时刻。
	IssuedAt time.Time
}

// ParseCodexTicketShape 解析门票信封结构。只做结构校验，不校验内容——门票是不透明
// 的上游 blob，网关能验证的只有形状与时效。
func ParseCodexTicketShape(value string) (CodexTicketShape, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return CodexTicketShape{}, fmt.Errorf("门票为空")
	}
	if len(value) > maxCodexTicketBytes || strings.ContainsAny(value, "\r\n\t ") {
		return CodexTicketShape{}, fmt.Errorf("门票编码非法")
	}
	core := strings.TrimRight(value, "=")
	if len(value)-len(core) > 2 {
		return CodexTicketShape{}, fmt.Errorf("门票填充非法")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(core)
	if err != nil ||
		len(raw) < CodexTicketEnvelopeHeaderBytes+CodexTicketBlockBytes ||
		raw[0] != 0x80 ||
		(len(raw)-CodexTicketEnvelopeHeaderBytes)%CodexTicketBlockBytes != 0 {
		return CodexTicketShape{}, fmt.Errorf("门票信封无法识别")
	}
	issuedUnix := binary.BigEndian.Uint64(raw[1:9])
	// 2020-01-01 .. 2100-01-01：超出这个范围说明解出来的不是签发时刻。
	if issuedUnix < 1577836800 || issuedUnix >= 4102444800 {
		return CodexTicketShape{}, fmt.Errorf("门票签发时刻超出合理范围")
	}
	return CodexTicketShape{
		Blocks:   (len(raw) - CodexTicketEnvelopeHeaderBytes) / CodexTicketBlockBytes,
		IssuedAt: time.Unix(int64(issuedUnix), 0),
	}, nil
}

// CodexTicketExpectedBlocks 返回该账号套餐应有的信封块数。
func CodexTicketExpectedBlocks(planType string) int {
	switch strings.ToLower(strings.TrimSpace(planType)) {
	case "team", "teamplus", "business", "enterprise", "self_serve_business_prolite",
		"team5x", "team-5x", "team_5x", "team 5x":
		// self_serve_business_prolite is a Team 5x workspace plan, not personal Prolite.
		// Keep these aliases local to tickets; preserve the account's plan metadata.
		return CodexTicketTeamBlocks
	}
	return CodexTicketPersonalBlocks
}

// CodexTicketExpectedLength 返回给定块数对应的编码后长度。
func CodexTicketExpectedLength(blocks int) int {
	if blocks <= 0 {
		blocks = CodexTicketPersonalBlocks
	}
	return base64.URLEncoding.EncodedLen(CodexTicketEnvelopeHeaderBytes + CodexTicketBlockBytes*blocks)
}

// NormalizeCodexTicketModel 归一化门票的模型键：大小写不敏感、去空白。
// 门票按 (账号, 模型) 持有，键必须在写入与读取两侧同源。
func NormalizeCodexTicketModel(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

// CodexTicketCredentialKey 返回模型对应的凭据键。
func CodexTicketCredentialKey(model string) string {
	return CodexTicketCredentialKeyPrefix + NormalizeCodexTicketModel(model)
}

// CodexTicketProbeCredentialKey 返回模型对应的探测摘要凭据键。
func CodexTicketProbeCredentialKey(model string) string {
	return CodexTicketCredentialKey(model) + CodexTicketProbeCredentialKeySuffix
}

// IsCodexTicketCredentialKey 报告某个凭据键是否为网关自管的门票材料。
// 账号编辑、导出与备份都要据此剔除，避免把门票当成运维配置回写或泄露。
func IsCodexTicketCredentialKey(key string) bool {
	return strings.HasPrefix(strings.TrimSpace(key), CodexTicketCredentialKeyPrefix)
}

// CodexTicketProbeSummary 是给管理端看的最近一次探测结果，不含门票内容。
type CodexTicketProbeSummary struct {
	// Result 是探测结论：success / token_error / error / invalid_state /
	// response_incomplete_or_error。
	Result string `json:"result"`
	// HTTPStatus 是探测请求的上游状态码（0 表示未取到响应）。
	HTTPStatus int `json:"http_status,omitempty"`
	// CheckedAt 是本次探测时刻。
	CheckedAt time.Time `json:"checked_at"`
	// NextProbeAt 是冷却结束、允许再次探测的时刻。
	NextProbeAt *time.Time `json:"next_probe_at,omitempty"`
}

// CodexTicket 是一张已捕获的门票。
type CodexTicket struct {
	// Model 是该门票适用的上游模型名。
	Model string `json:"model"`
	// State 是门票 blob 本身。
	State string `json:"state"`
	// Cookie 是签发门票时配套的请求 Cookie，必须随门票一起保存、切换和注入。
	Cookie string `json:"cookie,omitempty"`
	// CookieExpiresAt 单独保留 Cookie 的期限，业务响应换票时不能误用旧门票的期限。
	CookieExpiresAt time.Time `json:"cookie_expires_at,omitempty"`
	// CookieCapturedAt 独立计时，业务换票不能延长未更新的 Cookie。
	CookieCapturedAt time.Time `json:"cookie_captured_at,omitempty"`
	// 打票出口与会话随票保存，热备切换和业务续票必须沿用同一份快照。
	HarvestProxyURL  string `json:"harvest_proxy_url,omitempty"`
	HarvestSessionID string `json:"harvest_session_id,omitempty"`
	HarvestNodeID    string `json:"harvest_node_id,omitempty"`
	HarvestNodeName  string `json:"harvest_node_name,omitempty"`
	HarvestPoolID    string `json:"harvest_pool_id,omitempty"`
	// Length 是 State 的长度，落库后用于快速校验（不信任序列化后的长度）。
	Length int `json:"length"`
	// Blocks 是信封块数。
	Blocks int `json:"blocks,omitempty"`
	// IssuedAt 是上游签发时刻。
	IssuedAt time.Time `json:"issued_at,omitempty"`
	// CapturedAt 是网关捕获时刻。
	CapturedAt time.Time `json:"captured_at"`
	// ExpiresAt 是网关侧的失效时刻（TTL 与签发时刻+有效期取较早者）。
	ExpiresAt time.Time `json:"expires_at"`
	// Identity 记录铸造该门票的来源账号指纹，便于诊断与后续身份策略使用。共享回退
	// 不会把门票复制到其他账号的持久化凭据中。
	Identity string `json:"identity,omitempty"`
	// Standby 是热备门票：新票落库时旧票仍未过期，则降级为备用，
	// 当前票被上游拒绝时立刻顶上，不必等下一个探测周期。
	Standby *CodexTicket `json:"standby,omitempty"`
	// Revoked 表示该门票已被上游拒绝，不可再用。
	Revoked bool `json:"revoked,omitempty"`
}

// Valid 报告门票在 now 时刻是否可用：形状正确、未撤销、未过期、签发时刻合理。
// targetLen 传 0（或负值）表示不按编码长度校验——采票合格判定已改为探测回答。
func (t *CodexTicket) Valid(now time.Time, targetLen int) bool {
	if t == nil || t.Revoked || NormalizeCodexTicketCookie(t.Cookie) == "" {
		return false
	}
	if (t.HarvestProxyURL != "" && ValidateCodexTicketProxyURL(t.HarvestProxyURL) != nil) || strings.ContainsAny(t.HarvestSessionID, "\r\n") {
		return false
	}
	state := strings.TrimSpace(t.State)
	if len(state) == 0 || t.Length != len(state) || !strings.HasPrefix(state, CodexTicketStatePrefix) {
		return false
	}
	if targetLen > 0 && len(state) != targetLen {
		return false
	}
	shape, err := ParseCodexTicketShape(state)
	if err != nil || !codexTicketIssuedAtPlausible(shape.IssuedAt, now) {
		return false
	}
	if t.ExpiresAt.IsZero() || !now.Before(t.ExpiresAt) {
		return false
	}
	if !t.CookieFresh(now) {
		return false
	}
	return true
}

// NeedsRefresh 报告门票是否已进入重打窗口（TTL 即将耗尽）。
func (t *CodexTicket) NeedsRefresh(now time.Time, refreshBefore time.Duration) bool {
	if t == nil || !t.Valid(now, t.Length) {
		return true
	}
	return !t.EffectiveExpiry().After(now.Add(refreshBefore))
}

// EffectiveExpiry 统一缓存票、刷新调度和管理端的时效口径，旧版一小时缓存也受新上限约束。
func (t *CodexTicket) EffectiveExpiry() time.Time {
	if t == nil || t.ExpiresAt.IsZero() {
		return time.Time{}
	}
	shape, err := ParseCodexTicketShape(t.State)
	if err != nil {
		return time.Time{}
	}
	expires := minCodexTicketExpiry(t.ExpiresAt, shape.IssuedAt.Add(codexTicketValidity-codexTicketValidityMargin))
	if cookieExpiry := t.CookieEffectiveExpiry(); !cookieExpiry.IsZero() {
		expires = minCodexTicketExpiry(expires, cookieExpiry)
	}
	return expires
}

func minCodexTicketExpiry(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// codexTicketIssuedAtPlausible 校验签发时刻：不能来自未来（允许时钟漂移），
// 也不能老到超出上游有效期。
func codexTicketIssuedAtPlausible(issuedAt, now time.Time) bool {
	if issuedAt.After(now.Add(codexTicketIssuedSkew)) {
		return false
	}
	return now.Before(issuedAt.Add(codexTicketValidity - codexTicketValidityMargin))
}

// CodexTicketExpiry 把配置 TTL 与信封自带的签发时刻+有效期取较早者：
// 上游才是有效期的最终裁决者，本地 TTL 调大不能让过期票复活。
func CodexTicketExpiry(capturedAt, issuedAt time.Time, ttl time.Duration) time.Time {
	if ttl <= 0 || ttl > codexTicketValidity-codexTicketValidityMargin {
		ttl = codexTicketValidity - codexTicketValidityMargin
	}
	expires := capturedAt.Add(ttl)
	if !issuedAt.IsZero() {
		if upstream := issuedAt.Add(codexTicketValidity - codexTicketValidityMargin); upstream.Before(expires) {
			expires = upstream
		}
	}
	return expires
}

// CodexTicketIdentity 返回账号指纹：工作区 ID + 邮箱的哈希。它记录门票来源，
// 共享池是否允许回退由套餐类型与模型键决定。
func CodexTicketIdentity(accountID, email string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.TrimSpace(accountID)+"\x00"+strings.TrimSpace(email))))
}

// ParseCodexTicket 从凭据里的任意形态解析门票；字段缺失时补齐 Length 并做基本清理。
func ParseCodexTicket(model string, raw any) *CodexTicket {
	if raw == nil {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var ticket CodexTicket
	if err := json.Unmarshal(encoded, &ticket); err != nil {
		return nil
	}
	ticket.State = strings.TrimSpace(ticket.State)
	ticket.Cookie = NormalizeCodexTicketCookie(ticket.Cookie)
	if ticket.State == "" {
		return nil
	}
	ticket.Model = NormalizeCodexTicketModel(firstNonEmptyString(ticket.Model, model))
	if ticket.Length == 0 {
		ticket.Length = len(ticket.State)
	}
	if ticket.Standby != nil {
		standby := *ticket.Standby
		standby.State = strings.TrimSpace(standby.State)
		standby.Cookie = NormalizeCodexTicketCookie(standby.Cookie)
		if standby.Length == 0 {
			standby.Length = len(standby.State)
		}
		standby.Model = NormalizeCodexTicketModel(firstNonEmptyString(standby.Model, ticket.Model))
		ticket.Standby = &standby
	}
	return &ticket
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// ==================== 运行时账号上的门票 ====================

// CodexTicketForModel 返回该账号在指定模型上的门票（含有效性判定）。
// 主票不可用但热备票可用时返回热备票。targetLen 传 0 表示不按长度校验。
func (a *Account) CodexTicketForModel(model string, now time.Time, targetLen int) *CodexTicket {
	if a == nil {
		return nil
	}
	key := NormalizeCodexTicketModel(model)
	if key == "" {
		return nil
	}
	a.mu.RLock()
	ticket := cloneCodexTicket(a.CodexTickets[key])
	a.mu.RUnlock()
	if ticket == nil {
		return nil
	}
	if ticket.Valid(now, targetLen) {
		return ticket
	}
	if ticket.Standby != nil && ticket.Standby.Valid(now, targetLen) {
		return ticket.Standby
	}
	return nil
}

// CodexTicketInjection 返回该账号本次请求该注入的门票。
// 仅查询账号自己的门票；需要共享池回退时使用 CodexTicketInjectionWithShared。
func (a *Account) CodexTicketInjection(now time.Time, targetLen int, models ...string) (string, bool) {
	if a == nil {
		return "", false
	}
	for _, model := range models {
		if ticket := a.CodexTicketForModel(model, now, targetLen); ticket != nil {
			return ticket.State, true
		}
	}
	return "", false
}

const codexTicketSharedPoolKeySeparator = "\x00"

var codexTicketSharedPool = struct {
	sync.RWMutex
	tickets map[string]*CodexTicket
}{tickets: make(map[string]*CodexTicket)}

func codexTicketSharedPoolKey(planType, model string) string {
	return strings.ToLower(strings.TrimSpace(planType)) + codexTicketSharedPoolKeySeparator + NormalizeCodexTicketModel(model)
}

func cloneCodexTicket(ticket *CodexTicket) *CodexTicket {
	if ticket == nil {
		return nil
	}
	copy := *ticket
	copy.Standby = cloneCodexTicket(ticket.Standby)
	return &copy
}

// CodexTicketSnapshot 返回包括失效状态的完整副本，供持久化撤销结果使用。
func (a *Account) CodexTicketSnapshot(model string) *CodexTicket {
	if a == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return cloneCodexTicket(a.CodexTickets[NormalizeCodexTicketModel(model)])
}

func publishCodexTicketToSharedPool(planType string, ticket *CodexTicket) {
	if ticket == nil || NormalizeCodexTicketModel(ticket.Model) == "" {
		return
	}
	key := codexTicketSharedPoolKey(planType, ticket.Model)
	copy := cloneCodexTicket(ticket)
	copy.Model = NormalizeCodexTicketModel(copy.Model)
	codexTicketSharedPool.Lock()
	defer codexTicketSharedPool.Unlock()
	current := codexTicketSharedPool.tickets[key]
	if current == nil || copy.CapturedAt.After(current.CapturedAt) || (copy.CapturedAt.Equal(current.CapturedAt) && copy.State != current.State) {
		if current != nil && current.Valid(time.Now(), copy.Length) && current.State != copy.State && copy.Standby == nil {
			copy.Standby = cloneCodexTicket(current)
			copy.Standby.Standby = nil
		}
		codexTicketSharedPool.tickets[key] = copy
	}
}

// PublishCodexTicketToSharedPool registers an account's valid ticket for same-plan
// fallback without copying it into another account's credentials.
func PublishCodexTicketToSharedPool(account *Account, ticket *CodexTicket) {
	if account == nil || ticket == nil {
		return
	}
	publishCodexTicketToSharedPool(account.GetPlanType(), ticket)
}

func sharedCodexTicketForModel(planType, model string, now time.Time, targetLen int) *CodexTicket {
	key := codexTicketSharedPoolKey(planType, model)
	codexTicketSharedPool.RLock()
	ticket := cloneCodexTicket(codexTicketSharedPool.tickets[key])
	codexTicketSharedPool.RUnlock()
	if ticket == nil {
		return nil
	}
	if ticket.Valid(now, targetLen) {
		return ticket
	}
	if ticket.Standby != nil && ticket.Standby.Valid(now, targetLen) {
		return ticket.Standby
	}
	codexTicketSharedPool.Lock()
	if current := codexTicketSharedPool.tickets[key]; current != nil && !current.Valid(now, targetLen) && (current.Standby == nil || !current.Standby.Valid(now, targetLen)) {
		delete(codexTicketSharedPool.tickets, key)
	}
	codexTicketSharedPool.Unlock()
	return nil
}

// CodexTicketInjectionWithShared prefers the account's own ticket and falls back
// to a ticket captured by another account with the exact same plan type and model.
func (a *Account) CodexTicketInjectionWithShared(now time.Time, targetLen int, models ...string) (string, bool) {
	if ticket := a.CodexTicketWithShared(now, targetLen, models...); ticket != nil {
		return ticket.State, true
	}
	return "", false
}

// CodexTicketWithShared 返回完整快照，保证共享池与热备切换时门票和 Cookie 始终来自同一张票。
func (a *Account) CodexTicketWithShared(now time.Time, targetLen int, models ...string) *CodexTicket {
	if a == nil {
		return nil
	}
	for _, model := range models {
		if ticket := a.CodexTicketForModel(model, now, targetLen); ticket != nil {
			return ticket
		}
	}
	for _, model := range models {
		if ticket := sharedCodexTicketForModel(a.GetPlanType(), model, now, targetLen); ticket != nil {
			return ticket
		}
	}
	return nil
}

// RevokeSharedCodexTicket removes the matching shared ticket and its standby.
// Matching the state prevents a stale 401 from revoking a newer replacement.
func RevokeSharedCodexTicket(planType, model, state string) bool {
	key := codexTicketSharedPoolKey(planType, model)
	codexTicketSharedPool.Lock()
	defer codexTicketSharedPool.Unlock()
	current := codexTicketSharedPool.tickets[key]
	if current == nil {
		return false
	}
	if current.State == state {
		if current.Standby != nil && current.Standby.Valid(time.Now(), current.Length) {
			promoted := cloneCodexTicket(current.Standby)
			promoted.Standby = nil
			codexTicketSharedPool.tickets[key] = promoted
		} else {
			delete(codexTicketSharedPool.tickets, key)
		}
		return true
	}
	if current.Standby != nil && current.Standby.State == state {
		current.Standby = nil
		return true
	}
	return false
}

// ResetCodexTicketSharedPoolForTest clears process-global ticket state between tests.
func ResetCodexTicketSharedPoolForTest() {
	codexTicketSharedPool.Lock()
	defer codexTicketSharedPool.Unlock()
	codexTicketSharedPool.tickets = make(map[string]*CodexTicket)
}

// CodexTicketStatus 是单个模型的门票状态投影，供管理端展示。
type CodexTicketStatus struct {
	Model string `json:"model"`
	// Ready 表示存在可用门票（主票或热备票）。
	Ready bool `json:"ready"`
	// Length 是当前可用门票的长度。
	Length int `json:"length,omitempty"`
	// RemainingSeconds 是距失效的剩余秒数。
	RemainingSeconds int64 `json:"remaining_seconds"`
	// Revoked 表示当前票已被上游拒绝，等待重打。
	Revoked bool `json:"revoked"`
	// ExpiresAt 是当前票的失效时刻。
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// IssuedAt 是上游签发时刻。
	IssuedAt *time.Time `json:"issued_at,omitempty"`
	// Probe 是最近一次探测结果。
	Probe                  *CodexTicketProbeSummary `json:"probe,omitempty"`
	CookieCount            int                      `json:"cookie_count"`
	CookieRemainingSeconds int64                    `json:"cookie_remaining_seconds"`
	CookieExpiresAt        *time.Time               `json:"cookie_expires_at,omitempty"`
	CookieExpired          bool                     `json:"cookie_expired"`
	EgressBound            bool                     `json:"egress_bound"`
	SessionBound           bool                     `json:"session_bound"`
	StandbyReady           bool                     `json:"standby_ready"`
}

// CodexTicketStatuses 汇总账号在各门控模型上的门票状态。models 为门控名单。
func (a *Account) CodexTicketStatuses(models []string, now time.Time, targetLen int) []CodexTicketStatus {
	if a == nil || len(models) == 0 {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]CodexTicketStatus, 0, len(models))
	for _, model := range models {
		key := NormalizeCodexTicketModel(model)
		if key == "" {
			continue
		}
		status := CodexTicketStatus{Model: key}
		if probe, ok := a.CodexTicketProbes[key]; ok && probe != nil {
			copy := *probe
			status.Probe = &copy
		}
		ticket := a.CodexTickets[key]
		if ticket == nil {
			out = append(out, status)
			continue
		}
		status.Revoked = ticket.Revoked
		active := ticket
		if !ticket.Valid(now, targetLen) && ticket.Standby != nil && ticket.Standby.Valid(now, targetLen) {
			active = ticket.Standby
		}
		status.CookieCount = active.CookieCount()
		status.CookieExpired = !active.CookieFresh(now)
		if expiry := active.CookieEffectiveExpiry(); !expiry.IsZero() {
			status.CookieExpiresAt = &expiry
			status.CookieRemainingSeconds = max(0, int64(expiry.Sub(now)/time.Second))
		}
		status.EgressBound = active.HarvestProxyURL != ""
		status.SessionBound = active.HarvestSessionID != ""
		status.StandbyReady = active == ticket && ticket.Standby.Valid(now, targetLen)
		if active.Valid(now, targetLen) {
			status.Ready = true
			status.Length = active.Length
			expires := active.EffectiveExpiry()
			if remaining := int64(expires.Sub(now) / time.Second); remaining > 0 {
				status.RemainingSeconds = remaining
			}
			status.ExpiresAt = &expires
			if !active.IssuedAt.IsZero() {
				issued := active.IssuedAt
				status.IssuedAt = &issued
			}
		}
		out = append(out, status)
	}
	return out
}

// codexTicketCredentialSource 是装载门票所需的最小凭据行视图。
type codexTicketCredentialSource interface {
	GetCredential(string) string
	// CredentialEntries 返回凭据的原始键值对。门票按模型名动态生成键，无法逐个列举，
	// 只能遍历原始 map 找出前缀命中的项。
	CredentialEntries() map[string]any
}

// setCodexTicketsFromRowLocked 从凭据行装载门票与探测摘要。调用方必须持有 a.mu 写锁。
// 每次都整体重建：账号凭据被外部改写（重新授权、导入备份）后，内存里的旧票必须消失。
func (a *Account) setCodexTicketsFromRowLocked(row codexTicketCredentialSource) {
	a.CodexTickets = codexTicketsFromRow(row)
	a.CodexTicketProbes = codexTicketProbesFromRow(row)
}

// codexTicketsFromRow / codexTicketProbesFromRow 从凭据行提取门票与探测摘要，
// 供账号装载（struct 初始化）与热更新两侧使用。
func codexTicketsFromRow(row codexTicketCredentialSource) map[string]*CodexTicket {
	if row == nil {
		return map[string]*CodexTicket{}
	}
	tickets := map[string]*CodexTicket{}
	for key, raw := range row.CredentialEntries() {
		if strings.HasSuffix(key, CodexTicketProbeCredentialKeySuffix) {
			continue
		}
		if !IsCodexTicketCredentialKey(key) {
			continue
		}
		model := strings.TrimPrefix(key, CodexTicketCredentialKeyPrefix)
		if NormalizeCodexTicketModel(model) == "" {
			continue
		}
		if ticket := ParseCodexTicket(model, raw); ticket != nil {
			tickets[NormalizeCodexTicketModel(model)] = ticket
		}
	}
	return tickets
}

func codexTicketProbesFromRow(row codexTicketCredentialSource) map[string]*CodexTicketProbeSummary {
	if row == nil {
		return map[string]*CodexTicketProbeSummary{}
	}
	probes := map[string]*CodexTicketProbeSummary{}
	for key, raw := range row.CredentialEntries() {
		if !strings.HasPrefix(key, CodexTicketCredentialKeyPrefix) || !strings.HasSuffix(key, CodexTicketProbeCredentialKeySuffix) {
			continue
		}
		model := strings.TrimPrefix(key, CodexTicketCredentialKeyPrefix)
		model = strings.TrimSuffix(model, CodexTicketProbeCredentialKeySuffix)
		if summary := parseCodexTicketProbe(raw); summary != nil {
			probes[NormalizeCodexTicketModel(model)] = summary
		}
	}
	return probes
}

// parseCodexTicketProbe 解析落库的探测摘要。
func parseCodexTicketProbe(raw any) *CodexTicketProbeSummary {
	if raw == nil {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var summary CodexTicketProbeSummary
	if err := json.Unmarshal(encoded, &summary); err != nil {
		return nil
	}
	if strings.TrimSpace(summary.Result) == "" {
		return nil
	}
	return &summary
}

// CodexTicketProbeCoolingDown 报告该模型最近一次打票失败是否仍在冷却窗口内。
// 冷却按 (账号, 模型) 记录，避免一个坏号被反复打、把打票代理的出口 IP 打脏。
func (a *Account) CodexTicketProbeCoolingDown(model string, now time.Time) bool {
	if a == nil {
		return false
	}
	key := NormalizeCodexTicketModel(model)
	if key == "" {
		return false
	}
	a.mu.RLock()
	probe := a.CodexTicketProbes[key]
	a.mu.RUnlock()
	if probe == nil || probe.NextProbeAt == nil {
		return false
	}
	return now.Before(*probe.NextProbeAt)
}

// PublishCodexTicket 把新捕获的门票发布到运行时账号：旧票仍有效时降级为热备票，
// 这样主票被上游拒绝的瞬间就能顶上，不必等下一个探测周期。
func (s *Store) PublishCodexTicket(id int64, ticket *CodexTicket) {
	if s == nil || ticket == nil {
		return
	}
	account := s.FindByID(id)
	if account == nil {
		return
	}
	account.mu.Lock()
	if account.CodexTickets == nil {
		account.CodexTickets = map[string]*CodexTicket{}
	}
	key := NormalizeCodexTicketModel(ticket.Model)
	if key == "" {
		account.mu.Unlock()
		return
	}
	next := *ticket
	next.Model = key
	if current := account.CodexTickets[key]; next.Standby == nil && current != nil && current.Valid(time.Now(), next.Length) && current.State != next.State {
		previous := *current
		previous.Standby = nil
		next.Standby = &previous
	}
	account.CodexTickets[key] = &next
	account.mu.Unlock()
	PublishCodexTicketToSharedPool(account, &next)
}

// RevokeCodexTicket 把该模型上当前门票标记为已撤销，并在有热备票时立即启用它。
// 返回是否发生了变更。
func (s *Store) RevokeCodexTicket(id int64, model string) bool {
	if s == nil {
		return false
	}
	account := s.FindByID(id)
	if account == nil {
		return false
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	key := NormalizeCodexTicketModel(model)
	current := account.CodexTickets[key]
	if current == nil || current.Revoked {
		return false
	}
	next := *current
	if current.Standby != nil {
		next = *current.Standby
		next.Standby = nil
	} else {
		next.Revoked = true
	}
	account.CodexTickets[key] = &next
	return true
}

// RevokeCodexTicketForState revokes the account-local ticket only when it still
// matches state. This prevents a stale response from removing a newer ticket.
func (s *Store) RevokeCodexTicketForState(id int64, model, state string) bool {
	if s == nil {
		return false
	}
	account := s.FindByID(id)
	if account == nil {
		return false
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	key := NormalizeCodexTicketModel(model)
	current := account.CodexTickets[key]
	if current == nil {
		return false
	}
	if current.State != state {
		if current.Standby == nil || current.Standby.State != state {
			return false
		}
		next := *current
		next.Standby = nil
		account.CodexTickets[key] = &next
		return true
	}
	if current.Standby != nil {
		next := *current.Standby
		next.Standby = nil
		account.CodexTickets[key] = &next
	} else {
		current.Revoked = true
	}
	return true
}

// PublishCodexTicketProbe 记录一次探测结果摘要。
func (s *Store) PublishCodexTicketProbe(id int64, model string, summary *CodexTicketProbeSummary) {
	if s == nil || summary == nil {
		return
	}
	account := s.FindByID(id)
	if account == nil {
		return
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	if account.CodexTicketProbes == nil {
		account.CodexTicketProbes = map[string]*CodexTicketProbeSummary{}
	}
	key := NormalizeCodexTicketModel(model)
	if key == "" {
		return
	}
	copy := *summary
	account.CodexTicketProbes[key] = &copy
}
