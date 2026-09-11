package auth

import (
	"strconv"
	"strings"
	"testing"

	"github.com/codex2api/database"
)

func TestNewTraeCNDeviceIdentityIsUniquePerAccount(t *testing.T) {
	t.Parallel()
	seen := make(map[string]bool, 512)
	for i := 0; i < 512; i++ {
		identity := NewTraeCNDeviceIdentity()
		if identity.Empty() {
			t.Fatal("generated identity is empty")
		}
		if len(identity.MachineID) != 64 {
			t.Fatalf("machine id = %q, want 64 hex chars like the desktop client", identity.MachineID)
		}
		if _, err := strconv.ParseUint(identity.DeviceID, 10, 64); err != nil {
			t.Fatalf("device id = %q, want a numeric id: %v", identity.DeviceID, err)
		}
		if seen[identity.MachineID] {
			t.Fatalf("duplicated machine id %q", identity.MachineID)
		}
		seen[identity.MachineID] = true
		if identity.DeviceID != traeCNDeviceIDForMachineID(identity.MachineID) {
			t.Fatalf("device id %q is not derived from machine id %q", identity.DeviceID, identity.MachineID)
		}
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
		CredentialFamilyID:    "family-checkin",
		TraeCNDeviceMachineID: identity.MachineID,
		TraeCNDeviceID:        identity.DeviceID,
	}
	headers := TraeCNCheckinHeaders(account, "AT", "req-checkin")
	if got := headers.Get("x-market-user-id"); got != identity.MachineID {
		t.Fatalf("x-market-user-id = %q, want the account device code %q", got, identity.MachineID)
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
