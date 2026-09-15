package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func newTraeCNProbeTestStore(t *testing.T, account *auth.Account) *auth.Store {
	t.Helper()
	store := auth.NewStore(nil, nil, &database.SystemSettings{})
	t.Cleanup(store.Stop)
	store.AddAccount(account)
	return store
}

func traeCNProbeTestAccount(mode string) *auth.Account {
	return &auth.Account{DBID: 7001, UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT", TraeCNCreditsPool: mode}
}

func TestTraeCNCreditsProbeDue(t *testing.T) {
	now := time.Now().In(time.Local)
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	day := now.Format(traeCNCreditsProbeDayFormat)
	salt := "test-salt"

	cooling := traeCNProbeTestAccount(auth.TraeCNCreditsPoolAuto)
	active := traeCNProbeTestAccount(auth.TraeCNCreditsPoolAuto)
	disabled := traeCNProbeTestAccount(auth.TraeCNCreditsPoolAuto)
	markCooled := func(account *auth.Account) {
		account.Mu().Lock()
		account.Status = auth.StatusCooldown
		account.CooldownUtil = time.Now().Add(traeCNQuotaCooldownDefault)
		account.Mu().Unlock()
	}
	markCooled(cooling)
	markCooled(disabled)
	disabled.Mu().Lock()
	disabled.Disabled = 1
	disabled.Mu().Unlock()

	// 按当天派生出的时刻判断：这个账号的探针时刻可能还没到。
	offset := traeCNCreditsProbeOffset(day, cooling.DBID, salt)
	before := midnight.Add(offset).Add(-time.Minute)
	after := midnight.Add(offset).Add(time.Minute)

	// 状态：冷却中的账号才需要探针。
	if traeCNCreditsProbeDue(cooling, day, after, midnight, salt) != true {
		t.Fatal("cooling account must be probed at its daily slot")
	}
	if traeCNCreditsProbeDue(cooling, day, before, midnight, salt) != false {
		t.Fatal("probe must wait for the daily slot")
	}
	if traeCNCreditsProbeDue(active, day, after, midnight, salt) != false {
		t.Fatal("healthy account must not be probed")
	}
	if traeCNCreditsProbeDue(disabled, day, after, midnight, salt) != false {
		t.Fatal("administrator-disabled account must not be probed")
	}
	// 同一天同账号的时刻必须稳定（重启不会挪动）。
	if traeCNCreditsProbeOffset(day, cooling.DBID, salt) != offset {
		t.Fatal("probe offset must be deterministic")
	}
	if traeCNCreditsProbeOffset(day, cooling.DBID+1, salt) == offset {
		t.Fatal("different accounts should get different slots")
	}
}

func TestRunTraeCNCreditsProbeRecoversExhaustedAccount(t *testing.T) {
	now := time.Now()
	account := traeCNProbeTestAccount(auth.TraeCNCreditsPoolAuto)
	store := newTraeCNProbeTestStore(t, account)
	account.SetTraeCNCreditsBalance(auth.TraeCNCreditsBalance{CodeRemaining: 0, WorkRemaining: 0, ObservedAt: now})
	store.MarkCooldownWithError(account, traeCNQuotaCooldownDefault, "rate_limited", "额度不足")

	recovered, err := parseTraeCNCredits([]byte(traeCNCreditsPacksFixture), now)
	if err != nil {
		t.Fatal(err)
	}
	// 让 Code 池真的有余额：把已用改成一半。
	recovered.Pools[0].Used = recovered.Pools[0].Total / 2
	recovered.Pools[0].Remaining = recovered.Pools[0].Total / 2

	runTraeCNCreditsProbe(t.Context(), store, nil, account, func(context.Context, *auth.Account) (TraeCNCreditsSnapshot, error) {
		return recovered, nil
	})
	if account.TraeCNCreditsState() != auth.TraeCNCreditsStateOK {
		t.Fatalf("probe must record the recovered balance: %+v", account.TraeCNCreditsBalance())
	}
	if !account.IsAvailable() {
		t.Fatalf("recovered account must be schedulable again: status=%v", account.Status)
	}
}

func TestRunTraeCNCreditsProbeKeepsExhaustedAccountParked(t *testing.T) {
	now := time.Now()
	account := traeCNProbeTestAccount(auth.TraeCNCreditsPoolAuto)
	store := newTraeCNProbeTestStore(t, account)
	snapshot, err := parseTraeCNCredits([]byte(traeCNCreditsPacksFixture), now)
	if err != nil {
		t.Fatal(err)
	}
	// 两个池都用光。
	for index := range snapshot.Pools {
		snapshot.Pools[index].Used = snapshot.Pools[index].Total
		snapshot.Pools[index].Remaining = 0
	}
	runTraeCNCreditsProbe(t.Context(), store, nil, account, func(context.Context, *auth.Account) (TraeCNCreditsSnapshot, error) {
		return snapshot, nil
	})
	if account.Status != auth.StatusCooldown {
		t.Fatalf("exhausted account status = %v, want cooldown", account.Status)
	}
	remaining := time.Until(account.CooldownUtil)
	if remaining < 23*time.Hour {
		t.Fatalf("cooldown = %v, want it parked until the next daily probe", remaining)
	}
}

func TestRunTraeCNCreditsProbeQueryFailureDoesNotUnpark(t *testing.T) {
	account := traeCNProbeTestAccount(auth.TraeCNCreditsPoolAuto)
	store := newTraeCNProbeTestStore(t, account)
	store.MarkCooldownWithError(account, traeCNQuotaCooldownDefault, "rate_limited", "额度不足")
	attempts := 0
	runTraeCNCreditsProbe(t.Context(), store, nil, account, func(context.Context, *auth.Account) (TraeCNCreditsSnapshot, error) {
		attempts++
		return TraeCNCreditsSnapshot{}, errors.New("积分查询连接失败或超时")
	})
	if attempts != 1 {
		t.Fatalf("probe attempts = %d", attempts)
	}
	if account.Status != auth.StatusCooldown {
		t.Fatal("a failed probe must not un-park the account")
	}
}

func TestLiftTraeCNCreditsCooldownClearsLongQuotaCooldownOnly(t *testing.T) {
	now := time.Now()
	account := traeCNProbeTestAccount(auth.TraeCNCreditsPoolAuto)
	store := newTraeCNProbeTestStore(t, account)
	account.SetTraeCNCreditsBalance(auth.TraeCNCreditsBalance{CodeRemaining: 0, WorkRemaining: 2000, ObservedAt: now})
	store.MarkCooldownWithError(account, traeCNQuotaCooldownDefault, "rate_limited", "额度不足")
	liftTraeCNCreditsCooldown(store, account)
	if !account.IsAvailable() {
		t.Fatal("credits that came back must release the long quota cooldown")
	}

	// 短限流冷却不提前解冻，让它自然到期。
	short := traeCNProbeTestAccount(auth.TraeCNCreditsPoolAuto)
	short.DBID = 7002
	store.AddAccount(short)
	short.SetTraeCNCreditsBalance(auth.TraeCNCreditsBalance{CodeRemaining: 100, ObservedAt: now})
	store.MarkCooldownWithError(short, 30*time.Second, "rate_limited", "限流")
	liftTraeCNCreditsCooldown(store, short)
	if short.IsAvailable() {
		t.Fatal("short rate-limit cooldowns must run their course")
	}
}
