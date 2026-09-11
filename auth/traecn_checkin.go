package auth

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/codex2api/database"
)

// 自动签到状态随账号凭据一起持久化，避免进程重启后同一天重复签到。
const (
	TraeCNCheckinDateCredentialKey    = "traecn_checkin_date"
	TraeCNCheckinAtCredentialKey      = "traecn_checkin_at"
	TraeCNCheckinCreditsCredentialKey = "traecn_checkin_credits"
	TraeCNCheckinResultCredentialKey  = "traecn_checkin_result"
)

// TraeCNCheckinSnapshot 是账号最近一次签到结果。Date 用 yyyy-mm-dd（本地日期），
// 只用于"每天一次"的去重，不参与任何计费或调度决策。
type TraeCNCheckinSnapshot struct {
	Date    string
	At      time.Time
	Credits int64
	Result  string
}

// TraeCNCheckin 返回当前内存里的签到快照。
func (a *Account) TraeCNCheckin() TraeCNCheckinSnapshot {
	if a == nil {
		return TraeCNCheckinSnapshot{}
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.traeCNCheckin
}

// TraeCNCheckinDate 返回最近一次签到成功/尝试的本地日期。
func (a *Account) TraeCNCheckinDate() string {
	return a.TraeCNCheckin().Date
}

// PersistTraeCNCheckin 更新内存快照并落库；落库失败只记日志，不回滚内存
// （当天已签到的判断宁可保守，也不要因为一次写失败重复领取）。
func (s *Store) PersistTraeCNCheckin(dbID int64, snapshot TraeCNCheckinSnapshot) bool {
	if s == nil {
		return false
	}
	account := s.FindByID(dbID)
	if account == nil {
		return false
	}
	account.mu.Lock()
	previous := account.traeCNCheckin
	account.traeCNCheckin = snapshot
	account.mu.Unlock()
	if s.db == nil {
		return true
	}
	updates := map[string]interface{}{
		TraeCNCheckinDateCredentialKey:    snapshot.Date,
		TraeCNCheckinResultCredentialKey:  snapshot.Result,
		TraeCNCheckinCreditsCredentialKey: snapshot.Credits,
	}
	if !snapshot.At.IsZero() {
		updates[TraeCNCheckinAtCredentialKey] = snapshot.At.UTC().Format(time.RFC3339Nano)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.db.UpdateCredentials(ctx, dbID, updates); err != nil {
		log.Printf("[账号 %d] 持久化 Trae CN 签到状态失败: %v", dbID, err)
		account.mu.Lock()
		account.traeCNCheckin = previous
		account.mu.Unlock()
		return false
	}
	return true
}

// traeCNCheckinFromCredentials 从账号行的凭据里恢复签到快照。
func traeCNCheckinFromCredentials(row *database.AccountRow) TraeCNCheckinSnapshot {
	if row == nil {
		return TraeCNCheckinSnapshot{}
	}
	credits, _ := strconv.ParseInt(strings.TrimSpace(row.GetCredential(TraeCNCheckinCreditsCredentialKey)), 10, 64)
	snapshot := TraeCNCheckinSnapshot{
		Date:    strings.TrimSpace(row.GetCredential(TraeCNCheckinDateCredentialKey)),
		Result:  strings.TrimSpace(row.GetCredential(TraeCNCheckinResultCredentialKey)),
		Credits: credits,
	}
	if raw := strings.TrimSpace(row.GetCredential(TraeCNCheckinAtCredentialKey)); raw != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			snapshot.At = parsed.UTC()
		}
	}
	return snapshot
}

// ApplyTraeCNCheckinForTest 只给测试用：直接写入内存快照，不落库。
func (a *Account) ApplyTraeCNCheckinForTest(snapshot TraeCNCheckinSnapshot) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.traeCNCheckin = snapshot
	a.mu.Unlock()
}

// TraeCNCheckinHeaders 在标准桌面指纹之上补市场客户端标识：积分签到接口
// (api.trae.cn/trae/api/v2/ug/*) 要求 x-market-* 系列头，缺了就会被判定为非客户端。
func TraeCNCheckinHeaders(account *Account, accessToken, requestID string) http.Header {
	// 市场接口（api.trae.cn/trae/api/v2/ug/*）是另一套客户端身份，逐项对齐抓包：
	//   user-agent: VSCode 1.107.1 (Trae CN)、accept-language: zh-CN、
	//   authorization: Cloud-IDE-JWT、vscode-sessionid=machine id、
	//   x-market-client-id/x-market-user-id、package-type: stable_cn、x-request-id 为裸 uuid。
	// 这套头里没有 x-app-id / x-ide-version / x-machine-id，所以不复用 agent 的头集。
	if requestID == "" {
		requestID = uuid.NewString()
	}
	seed, _ := traeCNHeaderIdentity(account, requestID)
	profile := traeCNDeviceProfileForAccount(account, seed)
	marketUserAgent := firstTraeCNEnv("TRAECN_MARKET_USER_AGENT", "TRAE_MARKET_USER_AGENT")
	if marketUserAgent == "" {
		marketUserAgent = TraeCNMarketUserAgent
	}
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "*/*")
	headers.Set("Accept-Language", "zh-CN")
	headers.Set("User-Agent", marketUserAgent)
	headers.Set("Authorization", "Cloud-IDE-JWT "+strings.TrimSpace(accessToken))
	headers.Set("app-version", profile.IDEVersion)
	headers.Set("x-app-version", profile.IDEVersion)
	headers.Set("package-type", TraeCNPackageType)
	headers.Set("x-lgw-req-sdk-type", "3")
	headers.Set("vscode-sessionid", profile.MachineID)
	headers.Set("x-market-client-id", TraeCNMarketClientID)
	headers.Set("x-market-user-id", account.TraeCNMarketUserID())
	headers.Set("x-device-brand", profile.DeviceBrand)
	headers.Set("x-device-id", profile.DeviceID)
	headers.Set("x-device-type", profile.DeviceType)
	headers.Set("x-os-version", profile.OSVersion)
	headers.Set("x-tt-trace-id", TraeCNTTTraceID(seed))
	headers.Set("sec-fetch-dest", "empty")
	headers.Set("sec-fetch-mode", "no-cors")
	headers.Set("sec-fetch-site", "none")
	headers.Set("x-request-id", requestID)
	return headers
}
