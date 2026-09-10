package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

// 每天一次、时刻随机：同 (日期, 账号, salt) 必须稳定（重启不漏不重），
// 不同账号/不同日期必须落在不同时刻。
func TestTraeCNCheckinTargetOffsetIsRandomAndStable(t *testing.T) {
	t.Parallel()
	const salt = "test-salt"
	day := "2026-09-11"
	first := traeCNCheckinTargetOffset(day, 2, salt)
	if second := traeCNCheckinTargetOffset(day, 2, salt); first != second {
		t.Fatalf("同账号同日期偏移不稳定: %v vs %v", first, second)
	}
	if first < 0 || first >= 24*time.Hour {
		t.Fatalf("偏移越界: %v", first)
	}
	if other := traeCNCheckinTargetOffset(day, 3, salt); other == first {
		t.Fatalf("不同账号拿到了同一偏移: %v", first)
	}
	if nextDay := traeCNCheckinTargetOffset("2026-09-12", 2, salt); nextDay == first {
		t.Fatalf("相邻两天偏移相同（等于固定时段）: %v", first)
	}
	// 随机性粗检：20 个账号不应挤在同一分钟内。
	seen := make(map[time.Duration]struct{})
	for id := int64(1); id <= 20; id++ {
		seen[traeCNCheckinTargetOffset(day, id, salt)] = struct{}{}
	}
	if len(seen) < 18 {
		t.Fatalf("随机时刻过于集中: %d 个不同偏移 / 20 个账号", len(seen))
	}
}

func TestTraeCNCheckinDueRespectsDayRecordAndTargetTime(t *testing.T) {
	t.Parallel()
	const salt = "test-salt"
	day := "2026-09-11"
	midnight := time.Date(2026, 9, 11, 0, 0, 0, 0, time.Local)
	account := &auth.Account{DBID: 42, UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT"}
	target := midnight.Add(traeCNCheckinTargetOffset(day, account.DBID, salt))

	if traeCNCheckinDue(account, day, target.Add(-time.Minute), midnight, salt) {
		t.Fatal("到点前就触发了签到")
	}
	if !traeCNCheckinDue(account, day, target, midnight, salt) {
		t.Fatal("到点后没有触发签到")
	}
	// 当天已经签到过（含失败记录）就不再重复。
	account.ApplyTraeCNCheckinForTest(auth.TraeCNCheckinSnapshot{Date: day, Result: "今日已签到（积分 150）"})
	if traeCNCheckinDue(account, day, target.Add(time.Hour), midnight, salt) {
		t.Fatal("当天已签到仍然重复触发")
	}
}

// 未签到 → 领取一次 → 复读状态拿到积分；不重复领取。
func TestRunTraeCNCheckinClaimsOnceWhenNotCheckedIn(t *testing.T) {
	var claimCalls, statusCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case traeCNCheckinStatusPath:
			call := statusCalls.Add(1)
			if call == 1 {
				io.WriteString(w, `{"checked_in":false,"code":0,"credits":0,"enable":true,"message":"success"}`)
				return
			}
			io.WriteString(w, `{"checked_in":true,"code":0,"credits":150,"did_checked_in":true,"enable":true,"extra_credits":50,"message":"success"}`)
		case traeCNCheckinClaimPath:
			claimCalls.Add(1)
			if got := r.Header.Get("x-market-client-id"); got != traeCNCheckinMarketClientID {
				t.Errorf("缺少市场客户端头: %q", got)
			}
			io.WriteString(w, `{"code":0,"message":"success"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	account := &auth.Account{DBID: 7, UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT", ExpiresAt: time.Now().Add(time.Hour)}
	outcome, err := runTraeCNCheckin(t.Context(), nil, account, "", server.URL)
	if err != nil {
		t.Fatalf("runTraeCNCheckin() error = %v", err)
	}
	if !outcome.Claimed || !outcome.CheckedIn || outcome.Skipped != "" {
		t.Fatalf("outcome = %+v", outcome)
	}
	if outcome.Status.Credits != 150 || outcome.Status.Extra != 50 {
		t.Fatalf("积分未刷新: %+v", outcome.Status)
	}
	if claimCalls.Load() != 1 || statusCalls.Load() != 2 {
		t.Fatalf("claim=%d status=%d, want 1/2", claimCalls.Load(), statusCalls.Load())
	}
}

func TestRunTraeCNCheckinSkipsClaimWhenAlreadyCheckedInOrDisabled(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		status   string
		wantSkip string
	}{
		{name: "already", status: `{"checked_in":true,"code":0,"credits":150,"enable":true}`, wantSkip: "already"},
		{name: "disabled", status: `{"checked_in":false,"code":0,"enable":false}`, wantSkip: "disabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var claimCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == traeCNCheckinClaimPath {
					claimCalls.Add(1)
					io.WriteString(w, `{"code":0,"message":"success"}`)
					return
				}
				io.WriteString(w, tc.status)
			}))
			defer server.Close()
			account := &auth.Account{DBID: 8, UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT", ExpiresAt: time.Now().Add(time.Hour)}
			outcome, err := runTraeCNCheckin(t.Context(), nil, account, "", server.URL)
			if err != nil {
				t.Fatalf("runTraeCNCheckin() error = %v", err)
			}
			if claimCalls.Load() != 0 {
				t.Fatalf("不该发起领取: %d 次", claimCalls.Load())
			}
			if outcome.Skipped != tc.wantSkip || outcome.Claimed {
				t.Fatalf("outcome = %+v, want skip=%s", outcome, tc.wantSkip)
			}
		})
	}
}
