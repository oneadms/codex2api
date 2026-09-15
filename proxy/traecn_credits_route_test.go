package proxy

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

const traeCNNotifyUsagePayload = `id:1 event:notify_usage data:{"notify_type":3,"usage_type":7,"billing_mode":"credits","cn_credits_remain_info":{"ide_credits":0,"work_credits":2000}} id:2 event:error data:{"code":4008,"message":"Your requests have exceeded the quota.","extra":null}`

func TestTraeCNAccessTypeFollowsPoolMode(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name    string
		mode    string
		balance auth.TraeCNCreditsBalance
		want    int
	}{
		{"work_mode_always", auth.TraeCNCreditsPoolWork, auth.TraeCNCreditsBalance{}, auth.TraeCNWorkAccessType},
		{"code_mode_never", auth.TraeCNCreditsPoolCode, auth.TraeCNCreditsBalance{CodeRemaining: 0, WorkRemaining: 2000, ObservedAt: now}, 0},
		{"auto_without_state", auth.TraeCNCreditsPoolAuto, auth.TraeCNCreditsBalance{}, 0},
		{"auto_switches_when_code_drained", auth.TraeCNCreditsPoolAuto, auth.TraeCNCreditsBalance{CodeRemaining: 0, WorkRemaining: 2000, ObservedAt: now}, auth.TraeCNWorkAccessType},
		{"auto_keeps_code_when_code_left", auth.TraeCNCreditsPoolAuto, auth.TraeCNCreditsBalance{CodeRemaining: 12.5, WorkRemaining: 2000, ObservedAt: now}, 0},
		{"auto_keeps_code_without_work_credits", auth.TraeCNCreditsPoolAuto, auth.TraeCNCreditsBalance{CodeRemaining: 0, WorkRemaining: 0, ObservedAt: now}, 0},
		{"auto_ignores_stale_state", auth.TraeCNCreditsPoolAuto, auth.TraeCNCreditsBalance{CodeRemaining: 0, WorkRemaining: 2000, ObservedAt: now.Add(-30 * time.Minute)}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := auth.TraeCNShouldUseWorkPool(tc.mode, tc.balance, now); got != (tc.want != 0) {
				t.Fatalf("TraeCNShouldUseWorkPool(%q, %+v) = %v, want %v", tc.mode, tc.balance, got, tc.want != 0)
			}
			account := &auth.Account{UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT", TraeCNCreditsPool: tc.mode}
			account.SetTraeCNCreditsBalance(tc.balance)
			if got := traeCNAccessTypeForAccount(account); got != tc.want {
				t.Fatalf("traeCNAccessTypeForAccount = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestApplyTraeCNAccessTypeKeepsCodeRequestsByteIdentical(t *testing.T) {
	body := []byte(`{"function":"chat_v3","stream":true,"messages":[]}`)
	if got := applyTraeCNAccessType(body, 0); !bytes.Equal(got, body) {
		t.Fatalf("access_type=0 must not touch the captured request shape: %s", got)
	}
	updated := applyTraeCNAccessType(body, auth.TraeCNWorkAccessType)
	if gjson.GetBytes(updated, "access_type").Int() != int64(auth.TraeCNWorkAccessType) {
		t.Fatalf("access_type missing: %s", updated)
	}
	if gjson.GetBytes(updated, "function").String() != "chat_v3" || !gjson.GetBytes(updated, "stream").Bool() {
		t.Fatalf("original fields lost: %s", updated)
	}
	if applyTraeCNAccessType([]byte("not-json"), 1) == nil {
		t.Fatal("invalid body must be returned unchanged")
	}
}

func TestParseTraeCNCreditsRemainFromNotifyUsage(t *testing.T) {
	balance, ok := parseTraeCNCreditsRemain([]byte(traeCNNotifyUsagePayload))
	if !ok || balance.CodeRemaining != 0 || balance.WorkRemaining != 2000 || balance.ObservedAt.IsZero() {
		t.Fatalf("balance = %+v, %v", balance, ok)
	}
	for _, payload := range []string{
		`{"event":"notify_usage"}`,
		`cn_credits_remain_info="{not an object`,
		`cn_credits_remain_info={"ide_credits":0}`,
		`cn_credits_remain_info={"ide_credits":"0","work_credits":"2000"}`,
	} {
		if got, ok := parseTraeCNCreditsRemain([]byte(payload)); ok {
			t.Fatalf("payload %q produced %+v", payload, got)
		}
	}
}

func TestTraeCNCreditsRemainScannerRecordsAndPassesThrough(t *testing.T) {
	body := io.NopCloser(strings.NewReader(traeCNNotifyUsagePayload))
	account := &auth.Account{UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT"}
	reader := wrapTraeCNCreditsRemainScanner(body, account)
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != traeCNNotifyUsagePayload {
		t.Fatalf("scanner altered the stream: %s", raw)
	}
	balance := account.TraeCNCreditsBalance()
	if balance.CodeRemaining != 0 || balance.WorkRemaining != 2000 {
		t.Fatalf("balance not learned from the stream: %+v", balance)
	}
}

func TestTraeCNCreditsRemainScannerHandlesChunkedStream(t *testing.T) {
	account := &auth.Account{UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT"}
	reader := wrapTraeCNCreditsRemainScanner(&chunkedReadCloser{chunks: []string{
		"id:1 event:notify_us", `age data:{"cn_credits_remain_info":{"ide_credits":3.5,`, `"work_credits":42}}`,
	}}, account)
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "work_credits") {
		t.Fatalf("stream lost: %s", raw)
	}
	balance := account.TraeCNCreditsBalance()
	if balance.CodeRemaining != 3.5 || balance.WorkRemaining != 42 {
		t.Fatalf("chunked balance = %+v", balance)
	}
}

type chunkedReadCloser struct {
	chunks []string
	index  int
}

func (c *chunkedReadCloser) Read(p []byte) (int, error) {
	if c.index >= len(c.chunks) {
		return 0, io.EOF
	}
	n := copy(p, c.chunks[c.index])
	c.index++
	return n, nil
}

func (c *chunkedReadCloser) Close() error { return nil }

// traeCNCanonicalQuotaPayload 是 handler 真正交给冷却逻辑的形状：流内的 4008 会被
// 转成 canonical 失败响应，分池余额则来自同一流上的 notify_usage（由扫描器记录）。
const traeCNCanonicalQuotaPayload = `{"type":"response.failed","response":{"error":{"code":"4008","message":"Your requests have exceeded the quota.","type":"insufficient_quota","status_code":429}}}`

func TestTraeCNQuotaCooldownShortensWhenWorkPoolCanTakeOver(t *testing.T) {
	now := time.Now()
	payload := []byte(traeCNCanonicalQuotaPayload)
	decisive := &auth.Account{UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT", TraeCNCreditsPool: auth.TraeCNCreditsPoolAuto}
	// 流上的 notify_usage 已经被扫描器记到账号上，这里再触发冷却。
	balance, ok := parseTraeCNCreditsRemain([]byte(traeCNNotifyUsagePayload))
	if !ok {
		t.Fatal("fixture must carry the split balance")
	}
	balance.ObservedAt = now
	decisive.SetTraeCNCreditsBalance(balance)
	decision := applyTraeCNLimitCooldown(nil, decisive, payload, nil)
	if decision.Reason != "usage_limit" {
		t.Fatalf("reason = %q", decision.Reason)
	}
	if got := decisive.TraeCNCreditsBalance(); got.CodeRemaining != 0 || got.WorkRemaining != 2000 || got.ObservedAt.Before(now.Add(-time.Minute)) {
		t.Fatalf("balance lost before the pool switch: %+v", got)
	}
	if decision.Cooldown > traeCNWorkPoolSwitchCooldown {
		t.Fatalf("cooldown = %v, want the short pool-switch window", decision.Cooldown)
	}

	// 没有 Work 额度时保持原来的 5 分钟额度冷却。
	drained := &auth.Account{UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT", TraeCNCreditsPool: auth.TraeCNCreditsPoolAuto}
	drained.SetTraeCNCreditsBalance(auth.TraeCNCreditsBalance{ObservedAt: now})
	drainedDecision := applyTraeCNLimitCooldown(nil, drained, payload, nil)
	if drainedDecision.Cooldown < 5*time.Minute {
		t.Fatalf("drained cooldown = %v, want the full quota cooldown", drainedDecision.Cooldown)
	}
}

func TestTraeCNQuotaCooldownParksDrainedAccountUntilNextProbe(t *testing.T) {
	now := time.Now()
	account := &auth.Account{UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT", TraeCNCreditsPool: auth.TraeCNCreditsPoolAuto}
	account.SetTraeCNCreditsBalance(auth.TraeCNCreditsBalance{CodeRemaining: 0, WorkRemaining: 0, ObservedAt: now})
	decision := applyTraeCNLimitCooldown(nil, account, []byte(traeCNCanonicalQuotaPayload), nil)
	if decision.Reason != "usage_limit" || decision.Cooldown != traeCNQuotaCooldownDefault {
		t.Fatalf("exhausted account cooldown = %+v, want %v", decision, traeCNQuotaCooldownDefault)
	}
	if decision.Cooldown < 24*time.Hour {
		t.Fatalf("额度不足必须直接按限流处理到下一次日探针，实际 %v", decision.Cooldown)
	}
	// 状态列据此显示"积分用尽"，而不是等下一次请求再短暂标限流。
	if state := account.TraeCNCreditsState(); state != auth.TraeCNCreditsStateExhausted {
		t.Fatalf("credits state = %q", state)
	}
}

func TestTraeCNQuotaCooldownParksAccountsWithoutSnapshot(t *testing.T) {
	// 还没查到余额的账号同样按限流处理：额度不足不会因为"不知道余额"而每 5 分钟空撞。
	account := &auth.Account{UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT"}
	decision := applyTraeCNLimitCooldown(nil, account, []byte(traeCNCanonicalQuotaPayload), nil)
	if decision.Cooldown != traeCNQuotaCooldownDefault {
		t.Fatalf("unknown-balance cooldown = %v, want %v", decision.Cooldown, traeCNQuotaCooldownDefault)
	}
}

func TestTraeCNQuotaCooldownEnvOverride(t *testing.T) {
	account := &auth.Account{UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT"}
	t.Setenv(traeCNQuotaCooldownEnv, "240")
	if got := applyTraeCNLimitCooldown(nil, account, []byte(traeCNCanonicalQuotaPayload), nil).Cooldown; got != 4*time.Hour {
		t.Fatalf("env override cooldown = %v, want 4h", got)
	}
	t.Setenv(traeCNQuotaCooldownEnv, "1")
	if got := traeCNQuotaCooldown(); got != traeCNQuotaCooldownMin {
		t.Fatalf("too-small override = %v", got)
	}
	t.Setenv(traeCNQuotaCooldownEnv, "99999")
	if got := traeCNQuotaCooldown(); got != traeCNQuotaCooldownMax {
		t.Fatalf("too-large override = %v", got)
	}
	t.Setenv(traeCNQuotaCooldownEnv, "not-a-number")
	if got := traeCNQuotaCooldown(); got != traeCNQuotaCooldownDefault {
		t.Fatalf("invalid override = %v", got)
	}
}

func TestTraeCNQuotaCooldownIgnoresRateLimitPayload(t *testing.T) {
	account := &auth.Account{UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT", TraeCNCreditsPool: auth.TraeCNCreditsPoolAuto}
	account.SetTraeCNCreditsBalance(auth.TraeCNCreditsBalance{WorkRemaining: 2000, ObservedAt: time.Now()})
	payload := []byte(`{"response":{"error":{"code":"3004","message":"Your requests have exceeded the rate limit."}}}`)
	decision := applyTraeCNLimitCooldown(nil, account, payload, nil)
	if decision.Reason != "rate_limited" || decision.Cooldown > time.Minute {
		t.Fatalf("rate limit must keep its own cooldown: %+v", decision)
	}
}

func TestTraeCNQuotaPayloadStaysUsageLimit(t *testing.T) {
	if !IsTraeCNQuotaError([]byte(traeCNCanonicalQuotaPayload)) {
		t.Fatal("4008 payload must stay a usage_limit error")
	}
	if status, ok := responseFailedStatusCodeWithEvidence([]byte(traeCNCanonicalQuotaPayload)); !ok || status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, %v", status, ok)
	}
	// 流内原始 SSE 里的分池余额必须能被单独解析出来（不依赖它是合法 JSON）。
	balance, ok := parseTraeCNCreditsRemain([]byte(traeCNNotifyUsagePayload))
	if !ok || balance.WorkRemaining != 2000 {
		t.Fatalf("raw SSE balance = %+v, %v", balance, ok)
	}
}
