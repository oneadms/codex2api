package auth

import (
	"testing"
	"time"
)

func TestNormalizeCodexTicketSettings(t *testing.T) {
	settings, err := NormalizeCodexTicketSettings(CodexTicketSettings{})
	if err != nil {
		t.Fatalf("normalize defaults: %v", err)
	}
	if settings.Enabled {
		t.Fatal("harvesting must default to off")
	}
	if settings.TargetLength != CodexTicketDefaultTargetLength {
		t.Fatalf("target length = %d", settings.TargetLength)
	}
	if settings.TTLSeconds != CodexTicketDefaultTTLSeconds ||
		settings.RefreshBeforeSeconds != CodexTicketDefaultRefreshBeforeSecs ||
		settings.ProbeIntervalSeconds != CodexTicketDefaultProbeIntervalSecs ||
		settings.CooldownSeconds != CodexTicketDefaultCooldownSecs ||
		settings.MaxProbesPerRound != CodexTicketDefaultMaxProbesPerRound ||
		settings.ProbeTimeoutSeconds != CodexTicketDefaultProbeTimeoutSecs {
		t.Fatalf("defaults not filled: %+v", settings)
	}

	// 旧版长 TTL 必须收敛到当前 240 秒上限。
	clamped, err := NormalizeCodexTicketSettings(CodexTicketSettings{TTLSeconds: 99999, RefreshBeforeSeconds: 99999})
	if err != nil {
		t.Fatal(err)
	}
	if clamped.TTLSeconds != 240 {
		t.Fatalf("TTL not clamped: %d", clamped.TTLSeconds)
	}
	if clamped.RefreshBeforeSeconds >= clamped.TTLSeconds {
		t.Fatalf("refresh window must stay inside TTL: %d >= %d", clamped.RefreshBeforeSeconds, clamped.TTLSeconds)
	}

	// 探测周期低于下限时回到默认值，而不是照单全收。
	fast, err := NormalizeCodexTicketSettings(CodexTicketSettings{ProbeIntervalSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	if fast.ProbeIntervalSeconds < CodexTicketMinProbeIntervalSecs {
		t.Fatalf("probe interval = %d, want >= %d", fast.ProbeIntervalSeconds, CodexTicketMinProbeIntervalSecs)
	}

	// 非法输入必须被拒绝。
	for _, bad := range []CodexTicketSettings{
		{HarvestProxyURL: "ftp://127.0.0.1:1080"},
		{HarvestProxyURL: "socks5h://127.0.0.1:1080/path"},
		{HarvestProxyURL: "http://127.0.0.1:1080?x=1"},
		{TargetLength: maxCodexTicketBytes + 1},
		{Models: make([]string, CodexTicketMaxModels+1)},
	} {
		if _, err := NormalizeCodexTicketSettings(bad); err == nil {
			t.Errorf("expected error for %+v", bad)
		}
	}

	// 负的探测周期回到默认值（下限保护），而不是被当成"未配置"后照原样留下。
	negative, err := NormalizeCodexTicketSettings(CodexTicketSettings{ProbeIntervalSeconds: -5})
	if err != nil {
		t.Fatal(err)
	}
	if negative.ProbeIntervalSeconds != CodexTicketDefaultProbeIntervalSecs {
		t.Fatalf("negative probe interval = %d", negative.ProbeIntervalSeconds)
	}
}

func TestNormalizeCodexTicketModels(t *testing.T) {
	models, err := NormalizeCodexTicketModels([]string{" GPT-5.5 ", "gpt-5.5", "", "gpt-5*"})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0] != "gpt-5.5" || models[1] != "gpt-5*" {
		t.Fatalf("models = %v", models)
	}
	empty, err := NormalizeCodexTicketModels(nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty models = %v err=%v", empty, err)
	}
}

// 门控生效需要开关、代理、名单三者齐备；缺一都不注入。
func TestCodexTicketGateEnabled(t *testing.T) {
	previous := ConfiguredCodexTicketSettings()
	t.Cleanup(func() { SetConfiguredCodexTicketSettings(previous) })

	for _, tc := range []struct {
		name     string
		settings CodexTicketSettings
		want     bool
	}{
		{name: "all present", settings: CodexTicketSettings{Enabled: true, HarvestProxyURL: "socks5h://127.0.0.1:1080", Models: []string{"gpt-5.5"}}, want: true},
		{name: "disabled", settings: CodexTicketSettings{Enabled: false, HarvestProxyURL: "socks5h://127.0.0.1:1080", Models: []string{"gpt-5.5"}}},
		{name: "no proxy", settings: CodexTicketSettings{Enabled: true, Models: []string{"gpt-5.5"}}},
		{name: "no models", settings: CodexTicketSettings{Enabled: true, HarvestProxyURL: "socks5h://127.0.0.1:1080"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			SetConfiguredCodexTicketSettings(tc.settings)
			if got := CodexTicketGateEnabled(); got != tc.want {
				t.Fatalf("gate enabled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCodexTicketModelGated(t *testing.T) {
	previous := ConfiguredCodexTicketSettings()
	t.Cleanup(func() { SetConfiguredCodexTicketSettings(previous) })
	SetConfiguredCodexTicketSettings(CodexTicketSettings{
		Enabled: true, HarvestProxyURL: "socks5h://127.0.0.1:1080", Models: []string{"gpt-5.5"},
	})
	if !CodexTicketModelGated("GPT-5.5") {
		t.Fatal("model match must be case insensitive")
	}
	if CodexTicketModelGated("gpt-5.1-codex") || CodexTicketModelGated("") {
		t.Fatal("ungated models must not match")
	}
}

func TestCodexTicketCredentialKeys(t *testing.T) {
	if got := CodexTicketCredentialKey(" GPT-5.5 "); got != "codex_ticket:gpt-5.5" {
		t.Fatalf("credential key = %q", got)
	}
	if got := CodexTicketProbeCredentialKey("gpt-5.5"); got != "codex_ticket:gpt-5.5:probe" {
		t.Fatalf("probe key = %q", got)
	}
	if !IsCodexTicketCredentialKey("codex_ticket:gpt-5.5") || IsCodexTicketCredentialKey("codex_turn_state") {
		t.Fatal("credential key prefix detection is wrong")
	}
}

// 门票与铸造账号的出站身份绑定：换了工作区或邮箱就是另一张票。
func TestCodexTicketIdentity(t *testing.T) {
	base := CodexTicketIdentity("acct-1", "a@example.com")
	if base == "" || base != CodexTicketIdentity(" acct-1 ", "a@example.com") {
		t.Fatal("identity must be stable across whitespace")
	}
	if base == CodexTicketIdentity("acct-2", "a@example.com") ||
		base == CodexTicketIdentity("acct-1", "b@example.com") {
		t.Fatal("identity must change with account id or email")
	}
}

// 失效时刻取 TTL 与信封自带签发时刻+有效期中的较早者：本地 TTL 调大不能让过期票复活。
func TestCodexTicketExpiry(t *testing.T) {
	captured := time.Now()
	ttl := time.Hour
	// 信封里是一张早就签发的票：信封说了算。
	stale := CodexTicketExpiry(captured, captured.Add(-55*time.Minute), ttl)
	if !stale.Before(captured.Add(ttl)) {
		t.Fatalf("stale ticket expiry must be bounded by the envelope: %v", stale)
	}
	// 信封时刻缺失时也不能突破当前有效期。
	if got := CodexTicketExpiry(captured, time.Time{}, ttl); !got.Equal(captured.Add(210 * time.Second)) {
		t.Fatalf("expiry without issued time = %v", got)
	}
	// 刚签发的票：有效期为 240 秒，预留 30 秒安全边界。
	fresh := CodexTicketExpiry(captured, captured, ttl)
	if !fresh.Equal(captured.Add(codexTicketValidity - codexTicketValidityMargin)) {
		t.Fatalf("fresh ticket expiry = %v", fresh)
	}
	if !fresh.Before(captured.Add(ttl)) {
		t.Fatal("envelope validity must win over a longer local TTL")
	}
}

func TestParseCodexTicketSettings(t *testing.T) {
	// 未配过的配置（列默认 '{}'）必须落到文档承诺的默认值：FailClosed 默认开启。
	empty, err := ParseCodexTicketSettings("{}")
	if err != nil {
		t.Fatalf("parse empty object: %v", err)
	}
	if !empty.FailClosed {
		t.Fatal("fail-closed must default to on for a never-configured install")
	}
	if empty.Enabled {
		t.Fatal("harvesting must default to off")
	}

	settings, err := ParseCodexTicketSettings(`{"enabled":true,"harvest_proxy_url":"socks5h://127.0.0.1:1080","models":["gpt-5.5"],"fail_closed":false}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !settings.Enabled || settings.FailClosed || len(settings.Models) != 1 {
		t.Fatalf("parsed = %+v", settings)
	}
	// 空串与坏 JSON 都要落到默认值而不是 panic。
	if _, err := ParseCodexTicketSettings(""); err != nil {
		t.Fatalf("empty parse: %v", err)
	}
	if _, err := ParseCodexTicketSettings("{not json"); err == nil {
		t.Fatal("malformed JSON must error")
	}
}

func TestMaskCodexTicketProxyURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "password masked", raw: "socks5h://user:secret@127.0.0.1:1080", want: "socks5h://user:***@127.0.0.1:1080"},
		{name: "no password untouched", raw: "socks5h://user@127.0.0.1:1080", want: "socks5h://user@127.0.0.1:1080"},
		{name: "no userinfo untouched", raw: "http://127.0.0.1:8080", want: "http://127.0.0.1:8080"},
		{name: "invalid returns empty", raw: "ftp://127.0.0.1:1080", want: ""},
		{name: "empty returns empty", raw: "  ", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MaskCodexTicketProxyURL(tc.raw); got != tc.want {
				t.Fatalf("mask(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
	// 掩码必须能被识别，否则原样提交会变成把 *** 当新密码。
	if !IsMaskedCodexTicketProxyURL("socks5h://user:***@127.0.0.1:1080") {
		t.Fatal("masked proxy must be recognized")
	}
	if IsMaskedCodexTicketProxyURL("socks5h://user:secret@127.0.0.1:1080") {
		t.Fatal("real password must not look masked")
	}
	if !IsMaskedCodexTicketProxyURL("") {
		t.Fatal("empty means no proxy, treated as masked/no-op")
	}
}

func TestValidateCodexTicketProxyURL(t *testing.T) {
	for _, good := range []string{
		"", "http://127.0.0.1:8080", "https://proxy.example.com:443",
		"socks5://user:pass@127.0.0.1:1080", "socks5h://127.0.0.1:1080",
	} {
		if err := ValidateCodexTicketProxyURL(good); err != nil {
			t.Errorf("valid proxy %q rejected: %v", good, err)
		}
	}
	for _, bad := range []string{
		"127.0.0.1:1080", "ftp://127.0.0.1:1080", "socks5h://127.0.0.1:0",
		"socks5h://127.0.0.1:99999", "socks5h://127.0.0.1:1080/path", "socks5h://127.0.0.1:1080?a=1",
	} {
		if err := ValidateCodexTicketProxyURL(bad); err == nil {
			t.Errorf("invalid proxy %q accepted", bad)
		}
	}
}
