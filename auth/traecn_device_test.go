package auth

import (
	"strconv"
	"strings"
	"testing"

	"github.com/codex2api/database"
)

func TestNewTraeCNDeviceIdentityIsUniquePerAccount(t *testing.T) {
	t.Parallel()
	seen := make(map[string]bool, 1024)
	for i := 0; i < 512; i++ {
		identity := NewTraeCNDeviceIdentity()
		if identity.Empty() {
			t.Fatal("generated identity is empty")
		}
		if len(identity.MachineID) != 64 {
			t.Fatalf("machine id = %q, want 64 hex chars like the desktop client", identity.MachineID)
		}
		// 真实抓包里的 x-device-id 是 16 位十进制、无前导零（3996093699599548），
		// 所以新账号也必须是这个形状，不能被补零成 19 位。
		if len(identity.DeviceID) != TraeCNDeviceIDDigits || strings.HasPrefix(identity.DeviceID, "0") {
			t.Fatalf("device id = %q, want %d digits without leading zeros", identity.DeviceID, TraeCNDeviceIDDigits)
		}
		if value, err := strconv.ParseUint(identity.DeviceID, 10, 64); err != nil || value >= 1<<53 {
			t.Fatalf("device id = %q, want a JS-safe integer: %v", identity.DeviceID, err)
		}
		if seen[identity.MachineID] {
			t.Fatalf("duplicated machine id %q", identity.MachineID)
		}
		seen[identity.MachineID] = true
		if seen[identity.DeviceID] {
			t.Fatalf("duplicated device id %q", identity.DeviceID)
		}
		seen[identity.DeviceID] = true
		if len(identity.MarketUserID) != 36 || seen[identity.MarketUserID] {
			t.Fatalf("market user id = %q, want a unique UUID", identity.MarketUserID)
		}
		seen[identity.MarketUserID] = true
	}
}

func TestDeriveTraeCNDeviceIdentityIsStablePerSeed(t *testing.T) {
	t.Parallel()
	first := DeriveTraeCNDeviceIdentity("traecn:family-a")
	second := DeriveTraeCNDeviceIdentity("traecn:family-a")
	other := DeriveTraeCNDeviceIdentity("traecn:family-b")
	if first.MachineID != second.MachineID || first.DeviceID != second.DeviceID {
		t.Fatalf("same seed must derive the same identity: %+v vs %+v", first, second)
	}
	if first.MachineID == other.MachineID {
		t.Fatal("different seeds must derive different identities")
	}
	if DeriveTraeCNDeviceIdentity("") != (TraeCNDeviceIdentity{}) {
		t.Fatal("empty seed must not produce an identity")
	}
}

// 关键风控点：同一台网关上跑多个账号时，每个账号必须用自己的设备码，主机探测到的
// 设备码不能泄漏到请求头里。
func TestTraeCNRequestHeadersUseAccountBoundDeviceCode(t *testing.T) {
	t.Setenv("TRAECN_MACHINE_ID", "")
	t.Setenv("TRAECN_DEVICE_ID", "")
	sharedHostMachineID := discoverTraeCNDeviceProfile().MachineID

	firstIdentity := NewTraeCNDeviceIdentity()
	secondIdentity := NewTraeCNDeviceIdentity()
	first := &Account{
		DBID: 1, UpstreamType: UpstreamTraeCN, AccessToken: "AT", RefreshToken: "RT",
		CredentialFamilyID:    "family-1",
		TraeCNDeviceMachineID: firstIdentity.MachineID,
		TraeCNDeviceID:        firstIdentity.DeviceID,
	}
	second := &Account{
		DBID: 2, UpstreamType: UpstreamTraeCN, AccessToken: "AT", RefreshToken: "RT",
		CredentialFamilyID:    "family-2",
		TraeCNDeviceMachineID: secondIdentity.MachineID,
		TraeCNDeviceID:        secondIdentity.DeviceID,
	}

	firstHeaders := TraeCNRequestHeaders(first, "AT", "req-1")
	secondHeaders := TraeCNRequestHeaders(second, "AT", "req-1")
	if got := firstHeaders.Get("x-machine-id"); got != firstIdentity.MachineID {
		t.Fatalf("first x-machine-id = %q, want %q", got, firstIdentity.MachineID)
	}
	if got := firstHeaders.Get("x-device-id"); got != firstIdentity.DeviceID {
		t.Fatalf("first x-device-id = %q, want %q", got, firstIdentity.DeviceID)
	}
	if firstHeaders.Get("x-machine-id") == secondHeaders.Get("x-machine-id") {
		t.Fatalf("two accounts share one device code: %q", firstHeaders.Get("x-machine-id"))
	}
	if sharedHostMachineID != "" && firstHeaders.Get("x-machine-id") == sharedHostMachineID {
		t.Fatalf("host machine id %q leaked into an account request", sharedHostMachineID)
	}

	// 未绑定的老账号按账号身份确定性派生，两次请求必须一致。
	legacy := &Account{DBID: 9, UpstreamType: UpstreamTraeCN, AccessToken: "AT", RefreshToken: "RT", CredentialFamilyID: "family-legacy"}
	firstLegacy := TraeCNRequestHeaders(legacy, "AT", "req-1").Get("x-machine-id")
	secondLegacy := TraeCNRequestHeaders(legacy, "AT", "req-2").Get("x-machine-id")
	if firstLegacy == "" || firstLegacy != secondLegacy {
		t.Fatalf("legacy account device code is not stable: %q vs %q", firstLegacy, secondLegacy)
	}
	if firstLegacy == firstHeaders.Get("x-machine-id") {
		t.Fatal("legacy account reused another account's device code")
	}
}

func TestTraeCNCheckinHeadersUseAccountDeviceCode(t *testing.T) {
	t.Setenv("TRAECN_MACHINE_ID", "")
	t.Setenv("TRAECN_DEVICE_ID", "")
	identity := NewTraeCNDeviceIdentity()
	account := &Account{
		DBID: 3, UpstreamType: UpstreamTraeCN, AccessToken: "AT", RefreshToken: "RT",
		CredentialFamilyID: "family-checkin",
	}
	account.ApplyTraeCNDeviceIdentity(identity)
	headers := TraeCNCheckinHeaders(account, "AT", "req-checkin")
	// 签到走市场客户端身份：UA/package-type 与抓包一致，x-market-user-id 是安装期
	// UUID（不是 machine id），vscode-sessionid 才是 machine id。
	if got := headers.Get("User-Agent"); got != TraeCNMarketUserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, TraeCNMarketUserAgent)
	}
	if got := headers.Get("package-type"); got != TraeCNPackageType {
		t.Fatalf("package-type = %q, want %q", got, TraeCNPackageType)
	}
	if got := headers.Get("vscode-sessionid"); got != identity.MachineID {
		t.Fatalf("vscode-sessionid = %q, want the account machine id %q", got, identity.MachineID)
	}
	if got := headers.Get("x-market-user-id"); got != identity.MarketUserID {
		t.Fatalf("x-market-user-id = %q, want the bound market user id %q", got, identity.MarketUserID)
	}
	if got := headers.Get("x-market-user-id"); len(got) != 36 {
		t.Fatalf("x-market-user-id = %q, want a UUID like the real client", got)
	}
	// 推理请求走 TTNet 客户端身份。
	if got := TraeCNRequestHeaders(account, "AT", "req").Get("User-Agent"); got != TraeCNDefaultUserAgent {
		t.Fatalf("agent User-Agent = %q, want %q", got, TraeCNDefaultUserAgent)
	}
}

func TestTraeCNDeviceIdentityRoundTripsThroughCredentials(t *testing.T) {
	t.Parallel()
	identity := NewTraeCNDeviceIdentity()
	updates := identity.CredentialUpdates()
	if updates[TraeCNMachineIDCredentialKey] != identity.MachineID || updates[TraeCNDeviceIDCredentialKey] != identity.DeviceID {
		t.Fatalf("credential updates = %+v", updates)
	}
	if updates[TraeCNDeviceBoundAtCredentialKey] == nil {
		t.Fatal("bound_at must be recorded")
	}

	account := &Account{UpstreamType: UpstreamTraeCN}
	account.ApplyTraeCNDeviceIdentity(identity)
	if got := account.TraeCNDeviceIdentity(); got.MachineID != identity.MachineID || got.DeviceID != identity.DeviceID {
		t.Fatalf("applied identity = %+v, want %+v", got, identity)
	}
	// 只给 machine_id 时 device_id 自动派生。
	partial := &Account{UpstreamType: UpstreamTraeCN}
	partial.ApplyTraeCNDeviceIdentity(TraeCNDeviceIdentity{MachineID: strings.Repeat("a", 64)})
	if got := partial.TraeCNDeviceIdentity(); got.DeviceID != traeCNDeviceIDForMachineID(strings.Repeat("a", 64)) {
		t.Fatalf("derived device id = %q", got.DeviceID)
	}
}

// TRAECN_MACHINE_ID 是管理员显式指定单一设备画像的逃生口：设置后所有账号都按它出站。
func TestTraeCNDeviceEnvOverrideWins(t *testing.T) {
	t.Setenv("TRAECN_MACHINE_ID", "pinned-machine")
	account := &Account{
		DBID: 4, UpstreamType: UpstreamTraeCN, AccessToken: "AT", RefreshToken: "RT",
		CredentialFamilyID:    "family-pinned",
		TraeCNDeviceMachineID: NewTraeCNDeviceIdentity().MachineID,
	}
	if got := TraeCNRequestHeaders(account, "AT", "req").Get("x-machine-id"); got != "pinned-machine" {
		t.Fatalf("x-machine-id = %q, want the env override", got)
	}
}

// 账号列表/导出展示的必须是"实际发出去的那个设备码"：老账号没绑定值时按行身份派生，
// 且同一行每次派生结果一致、不同行互不相同。
func TestTraeCNEffectiveDeviceIdentityCoversLegacyRows(t *testing.T) {
	t.Parallel()
	bound := NewTraeCNDeviceIdentity()
	boundRow := &database.AccountRow{
		ID: 11, CredentialFamilyID: "family-bound",
		Credentials: map[string]any{
			TraeCNMachineIDCredentialKey: bound.MachineID,
			TraeCNDeviceIDCredentialKey:  bound.DeviceID,
		},
	}
	if got := TraeCNEffectiveDeviceIdentity(boundRow); got.MachineID != bound.MachineID || got.DeviceID != bound.DeviceID {
		t.Fatalf("bound row identity = %+v, want %+v", got, bound)
	}

	legacyA := &database.AccountRow{ID: 12, CredentialFamilyID: "family-a", Credentials: map[string]any{"traecn_user_id": "user-a"}}
	legacyB := &database.AccountRow{ID: 13, CredentialFamilyID: "family-b", Credentials: map[string]any{"traecn_user_id": "user-b"}}
	first := TraeCNEffectiveDeviceIdentity(legacyA)
	again := TraeCNEffectiveDeviceIdentity(legacyA)
	if first.MachineID == "" || first.MachineID != again.MachineID {
		t.Fatalf("legacy row identity is not stable: %+v vs %+v", first, again)
	}
	if first.MachineID == TraeCNEffectiveDeviceIdentity(legacyB).MachineID {
		t.Fatal("two legacy rows share one device code")
	}
	if first.MachineID == bound.MachineID {
		t.Fatal("legacy row reused a bound account's device code")
	}
	// 与出站请求的派生必须一致：有 DBID 用 DBID，只有 RT 时才用 RT 摘要。
	byID := &database.AccountRow{ID: 14, Credentials: map[string]any{"refresh_token": "rt-only"}}
	if got, want := TraeCNEffectiveDeviceIdentity(byID), DeriveTraeCNDeviceIdentity(TraeCNStableDeviceSeed("", "", 14, "rt-only")); got.MachineID != want.MachineID {
		t.Fatalf("db-derived identity = %+v, want %+v", got, want)
	}
	rtOnly := &database.AccountRow{Credentials: map[string]any{"refresh_token": "rt-only"}}
	if got, want := TraeCNEffectiveDeviceIdentity(rtOnly), DeriveTraeCNDeviceIdentity(TraeCNStableDeviceSeed("", "", 0, "rt-only")); got.MachineID != want.MachineID {
		t.Fatalf("rt-derived identity = %+v, want %+v", got, want)
	}
	if TraeCNEffectiveDeviceIdentity(nil) != (TraeCNDeviceIdentity{}) {
		t.Fatal("nil row must not produce an identity")
	}
}

// x-tt-trace-id 是客户端 TTNet 生成的，格式必须与抓包同构：
// 00-<trace(32hex)>-<span(16hex)>-01，trace 前半等于 span，span 后 8 位是安装级基线，
// trace 末 4 位固定 ffff。
func TestTraeCNTTTraceIDMatchesClientShape(t *testing.T) {
	t.Parallel()
	first := TraeCNTTTraceID("traecn:family-a")
	second := TraeCNTTTraceID("traecn:family-a")
	other := TraeCNTTTraceID("traecn:family-b")

	for _, value := range []string{first, second, other} {
		parts := strings.Split(value, "-")
		if len(parts) != 4 || parts[0] != "00" || parts[3] != "01" {
			t.Fatalf("trace id = %q, want 00-<32hex>-<16hex>-01", value)
		}
		if len(parts[1]) != 32 || len(parts[2]) != 16 {
			t.Fatalf("trace id = %q, want 32/16 hex segments", value)
		}
		if parts[1][:16] != parts[2] {
			t.Fatalf("trace id = %q, want trace[0:16] == span", value)
		}
		if parts[1][28:] != "ffff" {
			t.Fatalf("trace id = %q, want the ffff sampling marker", value)
		}
		for _, segment := range parts[1:3] {
			if _, err := strconv.ParseUint(segment, 16, 64); err != nil {
				// trace 段是 32 位 hex，超过 uint64 时按 16 位一半分别校验。
				if _, err := strconv.ParseUint(segment[:16], 16, 64); err != nil {
					t.Fatalf("trace id %q segment %q is not hex: %v", value, segment, err)
				}
				if _, err := strconv.ParseUint(segment[16:], 16, 64); err != nil {
					t.Fatalf("trace id %q segment %q is not hex: %v", value, segment, err)
				}
			}
		}
	}
	// 安装基线（span 后 8 位）同账号恒定、不同账号不同；每请求前半不同。
	// 字符串布局: "00-" + trace(32) + "-" + span(16) + "-01"，所以 span = [36:52]。
	spanOf := func(value string) string { return value[36:52] }
	if spanOf(first)[8:] != spanOf(second)[8:] {
		t.Fatalf("install base changed between requests: %q vs %q", first, second)
	}
	if spanOf(first)[8:] == spanOf(other)[8:] {
		t.Fatalf("two accounts share the install base: %q vs %q", first, other)
	}
	if spanOf(first)[:8] == spanOf(second)[:8] {
		t.Fatalf("per-request span segment repeated: %q vs %q", first, second)
	}
}

func TestTraeCNHeadersCarryClientTraceHeaders(t *testing.T) {
	// 会改环境变量，不能并行。
	account := &Account{DBID: 7, UpstreamType: UpstreamTraeCN, AccessToken: "AT", RefreshToken: "RT", CredentialFamilyID: "family-trace"}
	headers := TraeCNRequestHeaders(account, "AT", "req-1")
	if got := headers.Get("x-tt-trace-id"); !strings.HasPrefix(got, "00-") || !strings.HasSuffix(got, "-01") {
		t.Fatalf("x-tt-trace-id = %q", got)
	}
	// x-request-pin / x-requested-at 是服务端签发的带时效令牌，客户端造不出来；
	// llm_utils_chat 收到无效值会 400，所以默认不发。
	if got := headers.Get("x-request-pin"); got != "" {
		t.Fatalf("x-request-pin must not be sent by default, got %q", got)
	}
	if got := headers.Get("x-requested-at"); got != "" {
		t.Fatalf("x-requested-at must not be sent by default, got %q", got)
	}
	// 手工注入通道仍然可用（成对下发）。
	t.Setenv("TRAECN_REQUEST_PIN", "4d9ab754f68a11f3")
	t.Setenv("TRAECN_REQUESTED_AT", "1789061186")
	injected := TraeCNRequestHeaders(account, "AT", "req-1")
	if injected.Get("x-request-pin") != "4d9ab754f68a11f3" || injected.Get("x-requested-at") != "1789061186" {
		t.Fatalf("pin injection failed: %q / %q", injected.Get("x-request-pin"), injected.Get("x-requested-at"))
	}
	if got := TraeCNRequestID(); !strings.HasPrefix(got, "req_") {
		t.Fatalf("request id = %q, want req_<uuid>", got)
	}
}
