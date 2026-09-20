package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 后台自动打票：X-Codex-Turn-State 门票。
//
// 与 codex_turn_state.go 的手工注入互补：
//   - 手工注入：运维把上游铸造的值粘到账号上，一份值 + 模型名单；
//   - 自动打票：后台用合成请求向上游「铸造」门票，按 (账号, 模型) 分别持有，
//     临期前自动重打，业务请求上覆盖客户端回带值与手工配置值。
//
// 门票不是身份：它只影响上游的回合状态校验，不进调度、不影响请求归属。
//
// 门票的形状是一个可校验的信封：base64url 解码后是 57 字节固定头 + N 个 16 字节
// 加密块，首字节恒为 0x80，第 1..9 字节是大端 unix 签发时刻。块数由账号套餐决定，
// 因此「长度」本身就是一份可验证的契约——个人版 10 块编码后恰为 292。
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
	// CodexTicketDefaultTargetLength 是个人版门票编码后的长度。配置里写这个值表示
	// 「按账号套餐推导」，写其他值则强制该长度。
	CodexTicketDefaultTargetLength = 292

	// CodexTicketCredentialKeyPrefix 是门票在账号凭据里的键前缀，后缀为模型名。
	CodexTicketCredentialKeyPrefix = "codex_ticket:"
	// CodexTicketProbeCredentialKeySuffix 是探测结果摘要的键后缀。
	CodexTicketProbeCredentialKeySuffix = ":probe"

	// codexTicketValidity 是上游实测的门票有效期：签发后约 1 小时失效。
	codexTicketValidity = time.Hour
	// codexTicketIssuedSkew 允许签发时刻比本地时间超前这么多（时钟漂移容差）。
	codexTicketIssuedSkew = 30 * time.Second
	// codexTicketValidityMargin 是有效性判定预留的安全边界：门票在 59.5 分钟处即视为
	// 不可用，避免把「刚好过期」的票发出去换来一次必然失败的上游请求。
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
	case "team", "teamplus", "business", "enterprise":
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

// CodexTicketTargetLength 解析本次打票的目标长度：配置里等于默认值（或未配置）时按
// 账号套餐推导，否则以配置为准。
func CodexTicketTargetLength(planType string, configured int) int {
	if configured > 0 && configured != CodexTicketDefaultTargetLength {
		return configured
	}
	return CodexTicketExpectedLength(CodexTicketExpectedBlocks(planType))
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
	// Identity 是铸造该门票的账号指纹，防止账号换凭据后拿到别人的票。
	Identity string `json:"identity,omitempty"`
	// Standby 是热备门票：新票落库时旧票仍未过期，则降级为备用，
	// 当前票被上游拒绝时立刻顶上，不必等下一个探测周期。
	Standby *CodexTicket `json:"standby,omitempty"`
	// Revoked 表示该门票已被上游拒绝，不可再用。
	Revoked bool `json:"revoked,omitempty"`
}

// Valid 报告门票在 now 时刻是否可用：形状正确、未撤销、未过期、签发时刻合理。
func (t *CodexTicket) Valid(now time.Time, targetLen int) bool {
	if t == nil || t.Revoked {
		return false
	}
	state := strings.TrimSpace(t.State)
	if targetLen <= 0 {
		targetLen = CodexTicketDefaultTargetLength
	}
	if len(state) != targetLen || t.Length != targetLen || !strings.HasPrefix(state, CodexTicketStatePrefix) {
		return false
	}
	if t.ExpiresAt.IsZero() || !now.Before(t.ExpiresAt) {
		return false
	}
	if !t.IssuedAt.IsZero() && !codexTicketIssuedAtPlausible(t.IssuedAt, now) {
		return false
	}
	return true
}

// NeedsRefresh 报告门票是否已进入重打窗口（TTL 即将耗尽）。
func (t *CodexTicket) NeedsRefresh(now time.Time, refreshBefore time.Duration) bool {
	if t == nil || t.ExpiresAt.IsZero() {
		return true
	}
	return !t.ExpiresAt.After(now.Add(refreshBefore))
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
	expires := capturedAt.Add(ttl)
	if !issuedAt.IsZero() {
		if upstream := issuedAt.Add(codexTicketValidity - codexTicketValidityMargin); upstream.Before(expires) {
			expires = upstream
		}
	}
	return expires
}

// CodexTicketIdentity 返回账号指纹：工作区 ID + 邮箱的哈希。门票与铸造账号的出站
// 身份绑定，账号被重新授权成另一个工作区后旧票必须作废。
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
// 主票不可用但热备票可用时返回热备票。
func (a *Account) CodexTicketForModel(model string, now time.Time, targetLen int) *CodexTicket {
	if a == nil {
		return nil
	}
	key := NormalizeCodexTicketModel(model)
	if key == "" {
		return nil
	}
	a.mu.RLock()
	ticket := a.CodexTickets[key]
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

// CodexTicketInjection 返回本次请求该注入的门票。models 传入客户端模型与上游模型，
// 任一命中即可——映射改写之后两者常常不是同一个名字。
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
	Probe *CodexTicketProbeSummary `json:"probe,omitempty"`
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
		if active.Valid(now, targetLen) {
			status.Ready = true
			status.Length = active.Length
			if remaining := int64(active.ExpiresAt.Sub(now) / time.Second); remaining > 0 {
				status.RemainingSeconds = remaining
			}
			expires := active.ExpiresAt
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
	defer account.mu.Unlock()
	if account.CodexTickets == nil {
		account.CodexTickets = map[string]*CodexTicket{}
	}
	key := NormalizeCodexTicketModel(ticket.Model)
	if key == "" {
		return
	}
	next := *ticket
	next.Model = key
	if current := account.CodexTickets[key]; current != nil && !current.Revoked && current.State != next.State {
		previous := *current
		next.Standby = &previous
	}
	account.CodexTickets[key] = &next
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
