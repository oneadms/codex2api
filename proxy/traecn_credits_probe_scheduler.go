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

// Trae CN 额度不足的账号不按分钟级反复试探：上游回 4008 时直接按限流处理（默认冷却
// 到下一次日探针），每天固定时段用一次便宜的积分查询确认额度是否恢复，恢复了才解冻。
//
// 用积分查询而不是发一次推理请求：额度不足的根因就是积分，查积分能直接回答
// “额度回来了吗”，而且不消耗账号积分、不需要模型目录。
const (
	traeCNCreditsProbeDisableEnv  = "TRAECN_CREDITS_PROBE_DISABLED"
	traeCNCreditsProbeSaltEnv     = "TRAECN_CREDITS_PROBE_SALT"
	traeCNCreditsProbeDayFormat   = "2006-01-02"
	traeCNCreditsProbePollTick    = 5 * time.Minute
	traeCNCreditsProbeMaxAttempts = 3
	traeCNCreditsProbeTimeout     = 60 * time.Second
	// traeCNCreditsProbeWindow 是探针后仍然额度不足时的冷却时长：刚好覆盖到下一次
	// 日探针，避免每 5 分钟、15 分钟空撞一次。
	traeCNCreditsProbeWindow = 24 * time.Hour
)

// traeCNCreditsProbeAttempts 记录每个账号当天已探针次数（内存即可，重启重算）。
var traeCNCreditsProbeAttempts = struct {
	sync.Mutex
	entries map[string]int
}{entries: map[string]int{}}

func traeCNCreditsProbeAttemptKey(accountID int64, day string) string {
	return fmt.Sprintf("%d|%s", accountID, day)
}

func traeCNCreditsProbeAttemptsToday(accountID int64, day string) int {
	traeCNCreditsProbeAttempts.Lock()
	defer traeCNCreditsProbeAttempts.Unlock()
	return traeCNCreditsProbeAttempts.entries[traeCNCreditsProbeAttemptKey(accountID, day)]
}

func traeCNCreditsProbeRecordAttempt(accountID int64, day string) int {
	traeCNCreditsProbeAttempts.Lock()
	defer traeCNCreditsProbeAttempts.Unlock()
	key := traeCNCreditsProbeAttemptKey(accountID, day)
	if len(traeCNCreditsProbeAttempts.entries) > 4096 {
		traeCNCreditsProbeAttempts.entries = map[string]int{}
	}
	traeCNCreditsProbeAttempts.entries[key]++
	return traeCNCreditsProbeAttempts.entries[key]
}

// TraeCNCreditsProbeDisabled 允许运维关闭日探针（TRAECN_CREDITS_PROBE_DISABLED=1）。
func TraeCNCreditsProbeDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(traeCNCreditsProbeDisableEnv))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func traeCNCreditsProbeSalt() string {
	if value := strings.TrimSpace(os.Getenv(traeCNCreditsProbeSaltEnv)); value != "" {
		return value
	}
	return "codex2api-trae-credits-probe"
}

// traeCNCreditsProbeOffset 返回账号当天的探针时刻偏移（[0, 24h)），与签到同一套
// 派生方式：同一天同一账号总是同一个时刻，进程重启也不会把探针挪走。
func traeCNCreditsProbeOffset(day string, accountID int64, salt string) time.Duration {
	digest := sha256.Sum256([]byte(fmt.Sprintf("credits|%s|%d|%s", day, accountID, salt)))
	minutes := binary.BigEndian.Uint32(digest[:4]) % (24 * 60)
	return time.Duration(minutes) * time.Minute
}

// StartTraeCNCreditsProbeScheduler 启动每日额度探针循环。
func StartTraeCNCreditsProbeScheduler(ctx context.Context, store *auth.Store, db *database.DB) {
	if store == nil || TraeCNCreditsProbeDisabled() {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(90 * time.Second):
		}
		ticker := time.NewTicker(traeCNCreditsProbePollTick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				runTraeCNCreditsProbeDue(ctx, store, db, now)
			}
		}
	}()
}

// runTraeCNCreditsProbeDue 检查并执行到点的账号；抽出来便于测试注入时间。
func runTraeCNCreditsProbeDue(ctx context.Context, store *auth.Store, db *database.DB, now time.Time) {
	if store == nil {
		return
	}
	now = now.In(time.Local)
	day := now.Format(traeCNCreditsProbeDayFormat)
	salt := traeCNCreditsProbeSalt()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	for _, account := range store.Accounts() {
		if account == nil || !account.IsTraeCNAPI() || account.DBID <= 0 {
			continue
		}
		if !traeCNCreditsProbeDue(account, day, now, midnight, salt) {
			continue
		}
		runTraeCNCreditsProbeForAccount(ctx, store, db, account, day, TraeCNCheckinHost)
	}
}

// traeCNCreditsProbeDue 判断账号今天是否该探针：
// 只探当前不可用（冷却/错误）的账号，且当天到点、当天没探够次数。
func traeCNCreditsProbeDue(account *auth.Account, day string, now, midnight time.Time, salt string) bool {
	if account == nil || account.DBID <= 0 {
		return false
	}
	if !traeCNAccountNeedsCreditsProbe(account) {
		return false
	}
	if traeCNCreditsProbeAttemptsToday(account.DBID, day) >= traeCNCreditsProbeMaxAttempts {
		return false
	}
	target := midnight.Add(traeCNCreditsProbeOffset(day, account.DBID, salt))
	return !now.Before(target)
}

// traeCNAccountNeedsCreditsProbe 报告账号是否正处于“被挡住”的状态：只有这类账号
// 需要日探针确认额度是否恢复；正常可用或管理员手动停用的账号都不探。
func traeCNAccountNeedsCreditsProbe(account *auth.Account) bool {
	if account == nil {
		return false
	}
	account.Mu().RLock()
	defer account.Mu().RUnlock()
	if account.Disabled != 0 {
		return false
	}
	switch account.Status {
	case auth.StatusCooldown, auth.StatusError:
		return true
	}
	return false
}

type traeCNCreditsProbeQuery func(context.Context, *auth.Account) (TraeCNCreditsSnapshot, error)

// runTraeCNCreditsProbeForAccount 执行一次额度探针：查到可用额度就解冻，
// 仍然不足就把冷却续到下一次日探针。
func runTraeCNCreditsProbeForAccount(ctx context.Context, store *auth.Store, db *database.DB, account *auth.Account, day, host string) {
	runTraeCNCreditsProbe(ctx, store, db, account, func(probeCtx context.Context, target *auth.Account) (TraeCNCreditsSnapshot, error) {
		return queryTraeCNCredits(probeCtx, store, target, host)
	})
}

func runTraeCNCreditsProbe(ctx context.Context, store *auth.Store, db *database.DB, account *auth.Account, query traeCNCreditsProbeQuery) {
	if account == nil || query == nil {
		return
	}
	attemptCtx, cancel := context.WithTimeout(ctx, traeCNCreditsProbeTimeout)
	defer cancel()
	snapshot, err := query(attemptCtx, account)
	if err != nil {
		// 上游/网络问题不当作“额度恢复”，也不延长冷却：明天同一时刻再探。
		log.Printf("[traecn-credits-probe] account=%d result=query_failed error=%q", account.ID(), err.Error())
		if db != nil {
			db.InsertAccountEventAsync(account.DBID, "traecn_credits_probe_failed", err.Error())
		}
		return
	}
	// 把这次查到的余额写回账号，再据此判断能否解冻（真实查询也会写一遍，幂等）。
	recordTraeCNCreditsBalance(account, snapshot)
	state := account.TraeCNCreditsState()
	if state == auth.TraeCNCreditsStateOK || state == auth.TraeCNCreditsStateWorkOnly {
		if store != nil {
			store.ClearCooldown(account)
		}
		code, _ := snapshot.Pool(TraeCNCreditsPoolCode)
		work, _ := snapshot.Pool(TraeCNCreditsPoolWork)
		log.Printf("[traecn-credits-probe] account=%d result=recovered state=%s code_remaining=%.2f work_remaining=%.2f",
			account.ID(), state, code.Remaining, work.Remaining)
		if db != nil {
			db.InsertAccountEventAsync(account.DBID, "traecn_credits_probe_recovered",
				fmt.Sprintf("额度已恢复（Code 剩余 %.2f，Work 剩余 %.2f）", code.Remaining, work.Remaining))
		}
		return
	}
	// 仍然没有可用额度：冷却续到下一次日探针，不再按分钟级反复试探。
	if store != nil {
		store.MarkCooldownWithError(account, traeCNCreditsProbeWindow, "rate_limited", "Trae CN 积分不足，等待每日探针确认恢复")
	}
	log.Printf("[traecn-credits-probe] account=%d result=still_exhausted state=%s cooldown_hours=%.1f",
		account.ID(), state, traeCNCreditsProbeWindow.Hours())
	if db != nil {
		db.InsertAccountEventAsync(account.DBID, "traecn_credits_probe_exhausted", "积分仍不足，等待下一次日探针")
	}
}
