package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/google/uuid"
)

// Trae 有风控：一个设备码下出现多个账号会被判定为"同机批量登录"。因此设备码
// （machine_id + 16 位十进制 device_id，形态与真实客户端抓包一致）必须与账号一一绑定：
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
	// TraeCNMarketUserIDCredentialKey 是市场接口（x-market-user-id）用的客户端标识。
	// 抓包实测是一个安装期 UUID（652d2c41-…），与 machine id 无关。
	TraeCNMarketUserIDCredentialKey = "traecn_market_user_id"
)

// TraeCNDeviceIdentity 是绑定到某个账号的设备码。
type TraeCNDeviceIdentity struct {
	MachineID string    `json:"machine_id"`
	DeviceID  string    `json:"device_id"`
	BoundAt   time.Time `json:"bound_at,omitempty"`
	// MarketUserID 是市场客户端标识（x-market-user-id），真实客户端每次安装一个 UUID。
	MarketUserID string `json:"market_user_id,omitempty"`
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
	if value := strings.TrimSpace(id.MarketUserID); value != "" {
		updates[TraeCNMarketUserIDCredentialKey] = value
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

// TraeCNDeviceIDDigits 是真实客户端 x-device-id 的位数：抓包实测 3996093699599548
// 是 16 位十进制、没有前导零，且与 x-machine-id 没有可推导关系（它是客户端自己
// 生成的标识）。所以新账号直接生成同样形状的 16 位随机数，而不是拿 machine_id
// 去"算"一个出来——用哈希派生会得到 0000000000249215914 这种 9 个前导零、19 位的
// 值，和真实客户端一眼就能区分开。
const TraeCNDeviceIDDigits = 16

// NewTraeCNDeviceID 生成一个与真实客户端同形的 x-device-id：16 位十进制、首位非 0，
// 落在 JavaScript 安全整数范围内（< 2^53）。
func NewTraeCNDeviceID() string {
	// 16 位十进制，且上界不超过 2^53（JavaScript 安全整数）：真实抓包值
	// 3996093699599548 就在这个区间里。
	const (
		low  = 1_000_000_000_000_000       // 16 位下界
		span = 9_007_199_254_740_992 - low // 到 2^53 为止
	)
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		digest := sha256.Sum256([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
		copy(random, digest[:8])
	}
	value := uint64(0)
	for _, b := range random {
		value = value<<8 | uint64(b)
	}
	return strconv.FormatUint(low+value%span, 10)
}

// NewTraeCNDeviceIdentity 生成一份全新的设备码（随机，账号之间不会重复）。
func NewTraeCNDeviceIdentity() TraeCNDeviceIdentity {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		// 随机源不可用时退化为时间派生，仍然与其它账号不同。
		digest := sha256.Sum256([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
		raw = digest[:]
	}
	machineID := hex.EncodeToString(raw)
	return TraeCNDeviceIdentity{
		MachineID:    machineID,
		DeviceID:     NewTraeCNDeviceID(),
		MarketUserID: uuid.NewString(),
		BoundAt:      time.Now().UTC(),
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

// TraeCNMarketUserID 返回市场接口用的客户端标识（x-market-user-id）。账号没绑定过
// 时按稳定身份确定性生成——同一账号每次请求必须是同一个值，否则市场接口会当成新客户端。
func (a *Account) TraeCNMarketUserID() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	bound := strings.TrimSpace(a.TraeCNDeviceMarketUserID)
	seed := TraeCNStableDeviceSeed(a.CredentialFamilyID, a.AccountID, a.DBID, a.RefreshToken)
	a.mu.RUnlock()
	if bound != "" {
		return bound
	}
	if seed == "" {
		return ""
	}
	digest := sha256.Sum256([]byte("traecn-market:" + seed))
	// UUID v4 形状（版本位与变体位按规范置位），与真实客户端一致。
	digest[6] = (digest[6] & 0x0f) | 0x40
	digest[8] = (digest[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", digest[0:4], digest[4:6], digest[6:8], digest[8:10], digest[10:16])
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
		MachineID:    strings.TrimSpace(a.TraeCNDeviceMachineID),
		DeviceID:     strings.TrimSpace(a.TraeCNDeviceID),
		MarketUserID: strings.TrimSpace(a.TraeCNDeviceMarketUserID),
		BoundAt:      a.TraeCNDeviceBoundAt,
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
	a.TraeCNDeviceMarketUserID = strings.TrimSpace(identity.MarketUserID)
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
