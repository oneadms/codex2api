package auth

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// 后台自动打票的全局配置。与手工注入（codex_turn_state.go）不同，这份配置是全局的，
// 逐账号/逐模型的差异只体现在门票本身与门控模型名单上。
//
// 打票出口与业务出口刻意分开：业务请求仍走账号绑定的住宅代理，打票走
// HarvestProxyURL（由代理服务商自己轮换出口 IP）。用同一条住宅 IP 反复打票会迅速
// 把该 IP 打脏，而门票本身与铸造时的出口绑定，脏 IP 铸出来的票很快会被上游拒绝。
type CodexTicketSettings struct {
	// Enabled 是总开关。关闭时既不探测也不注入，按原链路转发。
	Enabled bool `json:"enabled"`
	// HarvestProxyURL 是打票专用代理（http/https/socks5/socks5h）。空表示未配置，
	// 此时不探测——绝不用业务代理兜底，避免污染账号的住宅出口。
	HarvestProxyURL string `json:"harvest_proxy_url"`
	// TargetLength 是门票目标长度。等于 CodexTicketDefaultTargetLength 或未配置时
	// 按账号套餐推导（个人版 292 / 团队版 332）。
	TargetLength int `json:"target_length"`
	// TTLSeconds 是门票在网关侧的缓存时长上限（默认 240）。
	TTLSeconds int `json:"ttl_seconds"`
	// RefreshBeforeSeconds 是提前重打窗口（默认 60）：剩余寿命进入该窗口即视为待刷新。
	RefreshBeforeSeconds int `json:"refresh_before_seconds"`
	// ProbeIntervalSeconds 是探测周期（默认 30，下限 30）。
	ProbeIntervalSeconds int `json:"probe_interval_seconds"`
	// CooldownSeconds 是单次探测失败后的冷却时长（默认 60）。
	CooldownSeconds int `json:"cooldown_seconds"`
	// MaxProbesPerRound 是每轮探测的请求数上限（默认 6）。
	MaxProbesPerRound int `json:"max_probes_per_round"`
	// ProbeTimeoutSeconds 是单次探测的超时（默认 25）。
	ProbeTimeoutSeconds int `json:"probe_timeout_seconds"`
	// FailClosed 为真时，门控模型上没有可用门票的账号直接拒绝出站（默认 true）：
	// 宁可报错也不裸打上游，避免上游把「无票请求」当成异常行为。
	FailClosed bool `json:"fail_closed"`
	// Models 是门控模型名单。门票按 (账号, 模型) 分别持有，只有名单内的模型才
	// 需要打票与注入；空名单表示开关开着但不门控任何模型。
	Models []string `json:"models"`
}

// 归一化后的默认值。
const (
	CodexTicketDefaultTTLSeconds        = 240
	CodexTicketDefaultRefreshBeforeSecs = 60
	CodexTicketDefaultProbeIntervalSecs = 30
	CodexTicketMinProbeIntervalSecs     = 30
	CodexTicketDefaultCooldownSecs      = 60
	CodexTicketDefaultMaxProbesPerRound = 6
	CodexTicketDefaultProbeTimeoutSecs  = 25
	// CodexTicketMaxModels 限制门控模型数量，避免配置面被滥用。
	CodexTicketMaxModels = 64
	// codexTicketProxyMaskPlaceholder 是打票代理密码的回显占位符。原文替换而非
	// url.UserPassword 重建，才能保证占位符在界面里可读、也能被原样提交回来。
	codexTicketProxyMaskPlaceholder = "***"
)

var configuredCodexTicketSettings atomic.Value // CodexTicketSettings

// DefaultCodexTicketSettings 返回出厂默认：关闭、无代理、个人版长度、无门控模型。
func DefaultCodexTicketSettings() CodexTicketSettings {
	return CodexTicketSettings{
		Enabled:              false,
		TargetLength:         CodexTicketDefaultTargetLength,
		TTLSeconds:           CodexTicketDefaultTTLSeconds,
		RefreshBeforeSeconds: CodexTicketDefaultRefreshBeforeSecs,
		ProbeIntervalSeconds: CodexTicketDefaultProbeIntervalSecs,
		CooldownSeconds:      CodexTicketDefaultCooldownSecs,
		MaxProbesPerRound:    CodexTicketDefaultMaxProbesPerRound,
		ProbeTimeoutSeconds:  CodexTicketDefaultProbeTimeoutSecs,
		FailClosed:           true,
	}
}

// NormalizeCodexTicketSettings 补齐缺省值并校验，返回可安全使用的配置。
func NormalizeCodexTicketSettings(settings CodexTicketSettings) (CodexTicketSettings, error) {
	out := settings
	out.HarvestProxyURL = strings.TrimSpace(out.HarvestProxyURL)
	if out.HarvestProxyURL != "" {
		if err := ValidateCodexTicketProxyURL(out.HarvestProxyURL); err != nil {
			return DefaultCodexTicketSettings(), err
		}
	}
	if out.TargetLength < 0 {
		out.TargetLength = 0
	}
	if out.TargetLength == 0 {
		out.TargetLength = CodexTicketDefaultTargetLength
	}
	if out.TargetLength > maxCodexTicketBytes {
		return DefaultCodexTicketSettings(), fmt.Errorf("门票目标长度不能超过 %d", maxCodexTicketBytes)
	}
	if out.TTLSeconds <= 0 {
		out.TTLSeconds = CodexTicketDefaultTTLSeconds
	}
	// TTL 超过上游有效期没有意义：信封自带的签发时刻才是最终裁决者。
	if maxTTL := int(codexTicketValidity / time.Second); out.TTLSeconds > maxTTL {
		out.TTLSeconds = maxTTL
	}
	if out.RefreshBeforeSeconds <= 0 {
		out.RefreshBeforeSeconds = CodexTicketDefaultRefreshBeforeSecs
	}
	if out.RefreshBeforeSeconds >= out.TTLSeconds {
		out.RefreshBeforeSeconds = min(CodexTicketDefaultRefreshBeforeSecs, out.TTLSeconds/2)
	}
	if out.ProbeIntervalSeconds < CodexTicketMinProbeIntervalSecs {
		out.ProbeIntervalSeconds = CodexTicketDefaultProbeIntervalSecs
	}
	// 旧版探测间隔可能长达数分钟；刷新窗口内至少保留两次检查机会。
	maxInterval := max(CodexTicketMinProbeIntervalSecs, out.RefreshBeforeSeconds/2)
	out.ProbeIntervalSeconds = min(out.ProbeIntervalSeconds, maxInterval)
	if out.CooldownSeconds <= 0 {
		out.CooldownSeconds = CodexTicketDefaultCooldownSecs
	}
	if out.MaxProbesPerRound <= 0 {
		out.MaxProbesPerRound = CodexTicketDefaultMaxProbesPerRound
	}
	if out.ProbeTimeoutSeconds <= 0 {
		out.ProbeTimeoutSeconds = CodexTicketDefaultProbeTimeoutSecs
	}
	models, err := NormalizeCodexTicketModels(out.Models)
	if err != nil {
		return DefaultCodexTicketSettings(), err
	}
	out.Models = models
	return out, nil
}

// NormalizeCodexTicketModels 去空白、去重、统一小写并保序。空名单是有意义的：
// 表示开关开着但不门控任何模型。
func NormalizeCodexTicketModels(models []string) ([]string, error) {
	if len(models) > CodexTicketMaxModels {
		return nil, fmt.Errorf("门控模型最多允许 %d 个", CodexTicketMaxModels)
	}
	seen := make(map[string]struct{}, len(models))
	out := make([]string, 0, len(models))
	for _, model := range models {
		model = NormalizeCodexTicketModel(model)
		if model == "" {
			continue
		}
		if _, dup := seen[model]; dup {
			continue
		}
		seen[model] = struct{}{}
		out = append(out, model)
	}
	return out, nil
}

// ValidateCodexTicketProxyURL 只校验语法，不发网络请求，也不在错误里回显凭据。
func ValidateCodexTicketProxyURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return fmt.Errorf("打票代理必须是带主机名的 HTTP(S) 或 SOCKS5(h) 地址，且不能带查询参数或片段")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return fmt.Errorf("打票代理地址不能带路径")
	}
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return fmt.Errorf("打票代理协议必须是 http、https、socks5 或 socks5h")
	}
	if port := parsed.Port(); port != "" {
		n, convErr := strconv.Atoi(port)
		if convErr != nil || n < 1 || n > 65535 {
			return fmt.Errorf("打票代理端口必须在 1-65535 之间")
		}
	}
	return nil
}

// MaskCodexTicketProxyURL 把代理地址里的密码替换成 ***，供界面回显。
// 非法或历史遗留数据一律返回空串，绝不把凭据透出去。
//
// 直接在原文字面上替换，而不是 url.Parse + 重建：重建时 url.UserPassword 会把
// * 转义成 %2A，界面输入框里会显示成一串百分号转义，运维根本认不出那是掩码，
// 而掩码必须能被原样提交回来（见 IsMaskedCodexTicketProxyURL）。
func MaskCodexTicketProxyURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || ValidateCodexTicketProxyURL(raw) != nil {
		return ""
	}
	start, end, ok := codexTicketProxyPasswordSpan(raw)
	if !ok {
		return raw
	}
	return raw[:start] + codexTicketProxyMaskPlaceholder + raw[end:]
}

// codexTicketProxyPasswordSpan 定位密码在原始地址里的字节区间。没有密码时 ok=false。
func codexTicketProxyPasswordSpan(raw string) (start, end int, ok bool) {
	userinfoStart := strings.Index(raw, "://")
	if userinfoStart < 0 {
		return 0, 0, false
	}
	userinfoStart += 3
	userinfoEnd := strings.Index(raw[userinfoStart:], "@")
	if userinfoEnd < 0 {
		return 0, 0, false
	}
	userinfo := raw[userinfoStart : userinfoStart+userinfoEnd]
	colon := strings.Index(userinfo, ":")
	if colon < 0 {
		return 0, 0, false
	}
	start = userinfoStart + colon + 1
	end = userinfoStart + len(userinfo)
	if start >= end {
		return 0, 0, false
	}
	return start, end, true
}

// IsMaskedCodexTicketProxyURL 识别界面回显用的密码占位符，避免把掩码当成新密码存回去。
func IsMaskedCodexTicketProxyURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return false
	}
	password, hasPassword := parsed.User.Password()
	return hasPassword && password == codexTicketProxyMaskPlaceholder
}

// ParseCodexTicketSettings 解析持久化的 JSON 配置。
//
// 在默认值之上做解码：缺失的键保留默认值，出现的键覆盖它。这样老配置（列里是 '{}'
// 或从未写过 fail_closed）也能拿到文档承诺的默认语义——FailClosed 默认开启。直接
// 解码进零值结构体会把「没配过」读成 false，等于把安全门控默认关掉。
func ParseCodexTicketSettings(raw string) (CodexTicketSettings, error) {
	settings := DefaultCodexTicketSettings()
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &settings); err != nil {
			return DefaultCodexTicketSettings(), err
		}
	}
	return NormalizeCodexTicketSettings(settings)
}

// SetConfiguredCodexTicketSettings 在持久化成功后发布不可变快照。
func SetConfiguredCodexTicketSettings(settings CodexTicketSettings) {
	normalized, err := NormalizeCodexTicketSettings(settings)
	if err != nil {
		// 调用方应先校验；这里兜底成默认值而不是把非法配置发布出去。
		normalized = DefaultCodexTicketSettings()
	}
	configuredCodexTicketSettings.Store(normalized)
}

// ConfiguredCodexTicketSettings 返回当前生效的打票配置。
func ConfiguredCodexTicketSettings() CodexTicketSettings {
	settings, _ := configuredCodexTicketSettings.Load().(CodexTicketSettings)
	if settings.TargetLength == 0 && len(settings.Models) == 0 && !settings.Enabled {
		// 尚未装载过任何配置（进程刚起）时给出默认值。
		return DefaultCodexTicketSettings()
	}
	return settings
}

// CodexTicketGateEnabled 报告打票门控是否生效：开关打开、配置了打票代理、且门控名单非空。
// 三者缺一都不注入——没有代理打不出票，没有名单就没有门控对象。
func CodexTicketGateEnabled() bool {
	settings := ConfiguredCodexTicketSettings()
	return settings.Enabled && settings.HarvestProxyURL != "" && len(settings.Models) > 0
}

// CodexTicketModelGated 报告某模型是否在门控名单内。
func CodexTicketModelGated(model string) bool {
	model = NormalizeCodexTicketModel(model)
	if model == "" {
		return false
	}
	for _, candidate := range ConfiguredCodexTicketSettings().Models {
		if candidate == model {
			return true
		}
	}
	return false
}

// CodexTicketTargetLengthFor 返回该账号应打的门票长度。
func CodexTicketTargetLengthFor(planType string) int {
	return CodexTicketTargetLength(planType, ConfiguredCodexTicketSettings().TargetLength)
}

// CodexTicketRefreshBefore 返回重打窗口时长。
func CodexTicketRefreshBefore() time.Duration {
	return time.Duration(ConfiguredCodexTicketSettings().RefreshBeforeSeconds) * time.Second
}

// CodexTicketProbeInterval 返回探测周期（下限见 CodexTicketMinProbeIntervalSecs）。
func CodexTicketProbeInterval() time.Duration {
	seconds := ConfiguredCodexTicketSettings().ProbeIntervalSeconds
	if seconds < CodexTicketMinProbeIntervalSecs {
		seconds = CodexTicketDefaultProbeIntervalSecs
	}
	return time.Duration(seconds) * time.Second
}

// CodexTicketCooldown 返回单次打票失败后的冷却时长。
func CodexTicketCooldown() time.Duration {
	seconds := ConfiguredCodexTicketSettings().CooldownSeconds
	if seconds <= 0 {
		seconds = CodexTicketDefaultCooldownSecs
	}
	return time.Duration(seconds) * time.Second
}
