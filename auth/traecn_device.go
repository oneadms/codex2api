package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"github.com/codex2api/database"
	"strings"
	"time"
)

// Trae 有风控：一个设备码下出现多个账号会被判定为"同机批量登录"。因此设备码
// （machine_id + 由它派生的 19 位 device_id）必须与账号一一绑定：
//
//   - 新建账号（RT 导入 / JSON 导入 / OAuth 授权）时生成一份随机设备码并落库；
//   - 老账号没有绑定时，按账号自身的稳定身份（credential family / DBID / RT 摘要）
//     确定性派生——这与升级前的行为一致，避免无谓地改动已有指纹；
//   - 所有出站请求（推理、模型同步、签到）都用账号自己那一份，绝不共用主机探测到的
//     真实设备码。
//
// 显式设置 TRAECN_MACHINE_ID / TRAECN_DEVICE_ID 时仍以环境变量为准（管理员主动
// 指定单一设备画像的场景）。
const (
	// TraeCNMachineIDCredentialKey 是账号绑定的设备 machine_id（64 位 hex，与桌面端
	// 的 telemetry.machineId 同格式）。
	TraeCNMachineIDCredentialKey = "traecn_machine_id"
	// TraeCNDeviceIDCredentialKey 是客户端由 machine_id 派生的 19 位数字设备码。
	TraeCNDeviceIDCredentialKey = "traecn_device_id"
	// TraeCNDeviceBoundAtCredentialKey 记录设备码的绑定时间。
	TraeCNDeviceBoundAtCredentialKey = "traecn_device_bound_at"
)

// TraeCNDeviceIdentity 是绑定到某个账号的设备码。
type TraeCNDeviceIdentity struct {
	MachineID string    `json:"machine_id"`
	DeviceID  string    `json:"device_id"`
	BoundAt   time.Time `json:"bound_at,omitempty"`
}

// Empty 表示没有可用的设备码。
func (id TraeCNDeviceIdentity) Empty() bool {
	return strings.TrimSpace(id.MachineID) == "" && strings.TrimSpace(id.DeviceID) == ""
}

// CredentialUpdates 返回落库用的凭据字段。
func (id TraeCNDeviceIdentity) CredentialUpdates() map[string]any {
	updates := map[string]any{}
	if machineID := strings.TrimSpace(id.MachineID); machineID != "" {
		updates[TraeCNMachineIDCredentialKey] = machineID
	}
	if deviceID := strings.TrimSpace(id.DeviceID); deviceID != "" {
		updates[TraeCNDeviceIDCredentialKey] = deviceID
	}
	if updates[TraeCNMachineIDCredentialKey] != nil {
		boundAt := id.BoundAt
		if boundAt.IsZero() {
			boundAt = time.Now().UTC()
		}
		updates[TraeCNDeviceBoundAtCredentialKey] = boundAt.UTC().Format(time.RFC3339Nano)
	}
	return updates
}

// NewTraeCNDeviceIdentity 生成一份全新的设备码（密码学随机，账号之间不会重复）。
func NewTraeCNDeviceIdentity() TraeCNDeviceIdentity {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		// 随机源不可用时退化为时间派生，仍然与其它账号不同。
		digest := sha256.Sum256([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
		raw = digest[:]
	}
	machineID := hex.EncodeToString(raw)
	return TraeCNDeviceIdentity{
		MachineID: machineID,
		DeviceID:  traeCNDeviceIDForMachineID(machineID),
		BoundAt:   time.Now().UTC(),
	}
}

// DeriveTraeCNDeviceIdentity 从账号的稳定身份确定性派生设备码。老账号没有绑定值
// 时用它兜底：同一账号每次派生出同一个设备码，升级不会改变既有指纹。
func DeriveTraeCNDeviceIdentity(seed string) TraeCNDeviceIdentity {
	seed = strings.TrimSpace(seed)
	if seed == "" {
		return TraeCNDeviceIdentity{}
	}
	digest := sha256.Sum256([]byte(seed))
	machineID := hex.EncodeToString(digest[:])
	return TraeCNDeviceIdentity{MachineID: machineID, DeviceID: traeCNDeviceIDForMachineID(machineID)}
}

// TraeCNDeviceIdentity 返回账号绑定的设备码；未绑定时返回零值。
func (a *Account) TraeCNDeviceIdentity() TraeCNDeviceIdentity {
	if a == nil {
		return TraeCNDeviceIdentity{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.traeCNDeviceIdentityLocked()
}

func (a *Account) traeCNDeviceIdentityLocked() TraeCNDeviceIdentity {
	identity := TraeCNDeviceIdentity{
		MachineID: strings.TrimSpace(a.TraeCNDeviceMachineID),
		DeviceID:  strings.TrimSpace(a.TraeCNDeviceID),
		BoundAt:   a.TraeCNDeviceBoundAt,
	}
	if identity.MachineID == "" && identity.DeviceID == "" {
		return TraeCNDeviceIdentity{}
	}
	if identity.DeviceID == "" {
		identity.DeviceID = traeCNDeviceIDForMachineID(identity.MachineID)
	}
	return identity
}

// ApplyTraeCNDeviceIdentity 把设备码写进内存投影（落库由调用方负责）。
func (a *Account) ApplyTraeCNDeviceIdentity(identity TraeCNDeviceIdentity) {
	if a == nil || identity.Empty() {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.TraeCNDeviceMachineID = strings.TrimSpace(identity.MachineID)
	a.TraeCNDeviceID = strings.TrimSpace(identity.DeviceID)
	a.TraeCNDeviceBoundAt = identity.BoundAt
	if a.TraeCNDeviceID == "" {
		a.TraeCNDeviceID = traeCNDeviceIDForMachineID(a.TraeCNDeviceMachineID)
	}
}

// TraeCNEffectiveDeviceIdentity 返回账号行实际生效的设备码：已绑定就返回绑定值，
// 否则按行身份确定性派生（与出站请求完全同一算法）。账号列表与导出用它能确保
// 展示/迁移的设备码和真正发出去的一致。
func TraeCNEffectiveDeviceIdentity(row *database.AccountRow) TraeCNDeviceIdentity {
	if row == nil {
		return TraeCNDeviceIdentity{}
	}
	identity := TraeCNDeviceIdentity{
		MachineID: strings.TrimSpace(row.GetCredential(TraeCNMachineIDCredentialKey)),
		DeviceID:  strings.TrimSpace(row.GetCredential(TraeCNDeviceIDCredentialKey)),
	}
	if !identity.Empty() {
		if identity.DeviceID == "" {
			identity.DeviceID = traeCNDeviceIDForMachineID(identity.MachineID)
		}
		return identity
	}
	seed := TraeCNStableDeviceSeed(row.CredentialFamilyID, row.GetCredential("traecn_user_id"), row.ID, row.GetCredential("refresh_token"))
	return DeriveTraeCNDeviceIdentity(seed)
}

// traeCNBoundDeviceIdentity 返回该账号实际要用的设备码：绑定值优先，没有就按账号
// 稳定身份派生。bound 表示这个值是否来自账号绑定的凭据。
func (a *Account) traeCNBoundDeviceIdentity(stableSeed string) (identity TraeCNDeviceIdentity, bound bool) {
	if a != nil {
		a.mu.RLock()
		identity = a.traeCNDeviceIdentityLocked()
		a.mu.RUnlock()
	}
	if !identity.Empty() {
		return identity, true
	}
	return DeriveTraeCNDeviceIdentity(stableSeed), false
}
