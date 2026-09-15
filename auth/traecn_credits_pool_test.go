package auth

import (
	"testing"
	"time"
)

func TestTraeCNCreditsStateFor(t *testing.T) {
	now := time.Now()
	fresh := func(code, work float64) TraeCNCreditsBalance {
		return TraeCNCreditsBalance{CodeRemaining: code, WorkRemaining: work, ObservedAt: now}
	}
	for _, tc := range []struct {
		name    string
		mode    string
		balance TraeCNCreditsBalance
		want    string
	}{
		{"auto_code_left", TraeCNCreditsPoolAuto, fresh(12, 0), TraeCNCreditsStateOK},
		{"auto_code_left_with_work", TraeCNCreditsPoolAuto, fresh(12, 2000), TraeCNCreditsStateOK},
		{"auto_work_takes_over", TraeCNCreditsPoolAuto, fresh(0, 2000), TraeCNCreditsStateWorkOnly},
		{"auto_both_drained", TraeCNCreditsPoolAuto, fresh(0, 0), TraeCNCreditsStateExhausted},
		{"code_only_ignores_work", TraeCNCreditsPoolCode, fresh(0, 2000), TraeCNCreditsStateExhausted},
		{"work_only_ignores_code", TraeCNCreditsPoolWork, fresh(50, 0), TraeCNCreditsStateExhausted},
		{"work_only_with_work_credits", TraeCNCreditsPoolWork, fresh(0, 50), TraeCNCreditsStateWorkOnly},
		{"unknown_without_snapshot", TraeCNCreditsPoolAuto, TraeCNCreditsBalance{}, TraeCNCreditsStateUnknown},
		{"unknown_when_stale", TraeCNCreditsPoolAuto, TraeCNCreditsBalance{ObservedAt: now.Add(-30 * time.Minute)}, TraeCNCreditsStateUnknown},
		{"unknown_mode_falls_back_to_auto", "weird", fresh(0, 2000), TraeCNCreditsStateWorkOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := TraeCNCreditsStateFor(tc.mode, tc.balance, now); got != tc.want {
				t.Fatalf("TraeCNCreditsStateFor(%q, %+v) = %q, want %q", tc.mode, tc.balance, got, tc.want)
			}
			account := &Account{UpstreamType: UpstreamTraeCN, TraeCNCreditsPool: tc.mode}
			account.SetTraeCNCreditsBalance(tc.balance)
			if got := account.TraeCNCreditsState(); got != tc.want {
				t.Fatalf("TraeCNCreditsState() = %q, want %q", got, tc.want)
			}
		})
	}
}
