package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// 自动签到：每个 Trae CN 账号每天一次，时刻在当天 0 点后随机——不固定时间段，
// 也不同账号同一时刻。随机值用 (日期, 账号ID, salt) 派生，因此进程重启后当天
// 的目标时刻不变（不会因为重启而漏签或重复签）。
const (
	traeCNCheckinSaltEnv    = "TRAECN_CHECKIN_SALT"
	traeCNCheckinDisableEnv = "TRAECN_CHECKIN_DISABLED"
	traeCNCheckinDayFormat  = "2006-01-02"
	// 轮询间隔：只是"到点没到"的检查，代价极低。
	traeCNCheckinPollInterval = time.Minute
	// 失败重试上限：网络抖动时当天还能补签，但绝不打爆上游。
	traeCNCheckinMaxAttemptsPerDay = 3
)

// traeCNCheckinAttempts 记录每个账号当天已尝试次数（内存即可：重启后重新计数，
// 最坏情况是多试几次，不会重复领取——成功当天就会写日期去重）。
var traeCNCheckinAttempts = struct {
	sync.Mutex
	entries map[string]int
}{entries: map[string]int{}}

func traeCNCheckinAttemptKey(accountID int64, day string) string {
	return fmt.Sprintf("%d|%s", accountID, day)
}

func traeCNCheckinAttemptsToday(accountID int64, day string) int {
	traeCNCheckinAttempts.Lock()
	defer traeCNCheckinAttempts.Unlock()
	return traeCNCheckinAttempts.entries[traeCNCheckinAttemptKey(accountID, day)]
}

func traeCNCheckinRecordAttempt(accountID int64, day string) int {
	traeCNCheckinAttempts.Lock()
	defer traeCNCheckinAttempts.Unlock()
	key := traeCNCheckinAttemptKey(accountID, day)
	if len(traeCNCheckinAttempts.entries) > 4096 {
		traeCNCheckinAttempts.entries = map[string]int{}
	}
	traeCNCheckinAttempts.entries[key]++
	return traeCNCheckinAttempts.entries[key]
}

// TraeCNCheckinDisabled 允许运维关闭自动签到（TRAECN_CHECKIN_DISABLED=1）。
func TraeCNCheckinDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(traeCNCheckinDisableEnv))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func traeCNCheckinSalt() string {
	if value := strings.TrimSpace(os.Getenv(traeCNCheckinSaltEnv)); value != "" {
		return value
	}
	return "codex2api-trae-checkin"
}

// traeCNCheckinTargetOffset 返回账号在指定日期（本地时区）的签到偏移量，
// 取值范围 [0, 24h)。相同 (日期, 账号, salt) 永远得到同一个偏移量。
func traeCNCheckinTargetOffset(day string, accountID int64, salt string) time.Duration {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s", day, accountID, salt)))
	minutes := binary.BigEndian.Uint32(digest[:4]) % (24 * 60)
	return time.Duration(minutes) * time.Minute
}

// StartTraeCNCheckinScheduler 启动每日签到循环。它自己带节流：只有到达当天随机
// 时刻、且账号当天尚未签到成功时才发请求。
func StartTraeCNCheckinScheduler(ctx context.Context, store *auth.Store, db *database.DB) {
	if store == nil || TraeCNCheckinDisabled() {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		// 启动后先等一小会儿，避免与冷启动的其它探针抢出口。
		select {
		case <-ctx.Done():
			return
		case <-time.After(45 * time.Second):
		}
		ticker := time.NewTicker(traeCNCheckinPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				runTraeCNCheckinDue(ctx, store, db, now)
			}
		}
	}()
}

// runTraeCNCheckinDue 检查并执行到点的账号；抽成独立函数便于测试注入时间。
func runTraeCNCheckinDue(ctx context.Context, store *auth.Store, db *database.DB, now time.Time) {
	if store == nil {
		return
	}
	now = now.In(time.Local)
	day := now.Format(traeCNCheckinDayFormat)
	salt := traeCNCheckinSalt()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	for _, account := range store.Accounts() {
		if account == nil || !account.IsTraeCNAPI() || account.DBID <= 0 {
			continue
		}
		if !traeCNCheckinDue(account, day, now, midnight, salt) {
			continue
		}
		runTraeCNCheckinForAccount(ctx, store, db, account, day, TraeCNCheckinHost)
	}
}

// traeCNCheckinDue 判断账号当天是否已经到点且还没签到。抽成纯函数便于测试：
// 只有"当天日期没记录过" + "已过当天随机时刻"才返回 true。
func traeCNCheckinDue(account *auth.Account, day string, now, midnight time.Time, salt string) bool {
	if account == nil || account.DBID <= 0 {
		return false
	}
	if strings.TrimSpace(account.TraeCNCheckinDate()) == day {
		return false
	}
	if traeCNCheckinAttemptsToday(account.DBID, day) >= traeCNCheckinMaxAttemptsPerDay {
		return false
	}
	target := midnight.Add(traeCNCheckinTargetOffset(day, account.DBID, salt))
	return !now.Before(target)
}

// runTraeCNCheckinForAccount 执行一次签到并把结果写回账号（内存 + 凭据）与账号事件。
func runTraeCNCheckinForAccount(ctx context.Context, store *auth.Store, db *database.DB, account *auth.Account, day, host string) {
	attemptCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	attempt := traeCNCheckinRecordAttempt(account.DBID, day)
	outcome, err := runTraeCNCheckin(attemptCtx, store, account, "", host)
	result := outcome.Message()
	snapshot := auth.TraeCNCheckinSnapshot{Date: day, At: time.Now(), Credits: outcome.Status.Credits, Result: result}
	if err != nil {
		// 失败不写日期，留出当天补签机会；尝试次数上限与轮询间隔一起兜住频率。
		if db != nil {
			db.InsertAccountEventAsync(account.DBID, "traecn_checkin_failed", fmt.Sprintf("第 %d 次尝试失败: %v", attempt, err))
		}
		log.Printf("[traecn-checkin] 账号 %d 第 %d 次签到失败: %v", account.DBID, attempt, err)
		return
	}
	store.PersistTraeCNCheckin(account.DBID, snapshot)
	if db != nil {
		db.InsertAccountEventAsync(account.DBID, "traecn_checkin", result)
	}
	log.Printf("[traecn-checkin] 账号 %d %s", account.DBID, result)
}
