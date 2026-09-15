package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestTraeCNLimitErrorClassification(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, payload  string
		limited, quota bool
	}{
		{"3004_rate", `{"response":{"error":{"code":"3004","message":"We're sorry, your requests have exceeded the rate limit. Please wait and try again later or contact our support team for assistance."}}}`, true, false},
		{"3004_quota", `{"response":{"error":{"code":"3004","message":"Your requests have exceeded the quota."}}}`, true, true},
		{"nested", `{"data":{"data":{"code":3004,"message":"Your requests have exceeded the quota."}}}`, true, true},
		{"quota_type", `{"error":{"code":"3004","type":"insufficient_quota","message":"exhausted"}}`, true, true},
		{"quota_code", `{"error":{"code":"quota_exhausted","message":"exhausted"}}`, true, true},
		{"4011", `{"error":{"code":"4011"}}`, true, false},
		{"outer_error_with_data", `{"code":4011,"message":"limited","data":{}}`, true, false},
		{"unknown_3004", `{"error":{"code":"3004","message":"all models failed"}}`, false, false},
		{"input_echo", `{"error":{"code":"invalid_request","message":"invalid request"},"input":"Your requests have exceeded the quota."}`, false, false},
		{"data_echo", `{"error":{"code":"invalid_request","message":"invalid request"},"data":{"message":"Your requests have exceeded the quota."}}`, false, false},
		{"text", `{"type":"response.output_text.delta","delta":"Your requests have exceeded the quota."}`, false, false},
		{"invalid_json", `not-json`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte(tc.payload)
			if IsTraeCNRateLimitError(payload) != tc.limited || IsTraeCNQuotaError(payload) != tc.quota {
				t.Fatalf("unexpected classification for %s", tc.payload)
			}
			if tc.quota && !isPermanentQuotaFailure(payload) {
				t.Fatal("quota exhaustion can reenter the same request's retry pool")
			}
		})
	}
}

func TestTraeCNCanonicalQuotaPreservesErrorAndReset(t *testing.T) {
	t.Parallel()
	provider := "event: error\ndata: {\"code\":3004,\"message\":\"Your requests have exceeded the quota.\",\"resets_in_seconds\":90}\n\n"
	raw, err := io.ReadAll(traeCNCanonicalStream(io.NopCloser(strings.NewReader(provider)), "DeepSeek-V4-Pro"))
	if err != nil {
		t.Fatal(err)
	}
	failed, ok := findCanonicalEvent(canonicalSSEEvents(t, raw), "response.failed")
	if !ok || failed.Get("response.error.code").String() != "3004" || failed.Get("response.error.type").String() != "insufficient_quota" || failed.Get("response.error.resets_in_seconds").Int() != 90 {
		t.Fatalf("lost quota error metadata: %s", raw)
	}
	outcome := classifyResponseFailedOutcome([]byte(failed.Raw))
	if outcome.logStatusCode != http.StatusTooManyRequests || outcome.failureKind != "usage_limit" {
		t.Fatalf("quota outcome=%+v", outcome)
	}
}

func TestTraeCNLimitCooldownUpdatesAccountWithRelayPolicyOff(t *testing.T) {
	for _, tc := range []struct {
		name, body, retryAfter string
		want                   time.Duration
	}{
		{"rate", `{"error":{"code":"3004","message":"Your requests have exceeded the rate limit."}}`, "", time.Minute},
		// 额度不足不再用 5 分钟这种短冷却：直接按限流处理到下一次日探针。
		{"quota", `{"error":{"code":"3004","message":"Your requests have exceeded the quota."}}`, "", traeCNQuotaCooldownDefault},
		{"header", `{"error":{"code":"3004","message":"Your requests have exceeded the quota."}}`, "12", 12 * time.Second},
		{"body", `{"error":{"code":"3004","message":"Your requests have exceeded the quota.","resets_in_seconds":90}}`, "", 90 * time.Second},
		{"body_retry_after", `{"error":{"code":"3004","message":"Your requests have exceeded the quota.","retry_after":45}}`, "", 45 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
			t.Cleanup(store.Stop)
			account := &auth.Account{DBID: 91700, UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT", RefreshToken: "RT", ExpiresAt: time.Now().Add(time.Hour)}
			store.AddAccount(account)
			if store.ResolveModelCooldownPolicy(account).Mode != database.ModelCooldownModeOff {
				t.Fatal("test needs the default disabled relay policy")
			}
			resp := &http.Response{Header: http.Header{"Retry-After": []string{tc.retryAfter}}}
			decision := Apply429Cooldown(store, account, []byte(tc.body), resp, "DeepSeek-V4-Pro")
			if decision.Scope != rateLimitScopeAccount || decision.Cooldown != tc.want || account.RuntimeStatus() != "rate_limited" {
				t.Fatalf("cooldown=%+v status=%s", decision, account.RuntimeStatus())
			}
			account.Mu().RLock()
			message, percentValid := account.ErrorMsg, account.UsagePercent7dValid
			account.Mu().RUnlock()
			if !strings.Contains(message, "3004") || percentValid {
				t.Fatal("lost original error or fabricated a Codex usage window")
			}
		})
	}
}

func TestTraeCNQuotaCooldownSurvivesLaterRateLimit(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	account := &auth.Account{UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT"}
	first := Apply429Cooldown(store, account, []byte(`{"message":"Your requests have exceeded the quota."}`), nil, "")
	second := Apply429Cooldown(store, account, []byte(`{"message":"Your requests have exceeded the rate limit."}`), nil, "")
	if second.ResetAt.Before(first.ResetAt) {
		t.Fatal("rate limit shortened the quota cooldown")
	}
}

func TestTraeCNLimitHTTPNormalizationPreservesBody(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"quota", `{"code":3004,"message":"Your requests have exceeded the quota."}`, http.StatusTooManyRequests},
		{"rate", `{"code":3004,"message":"Your requests have exceeded the rate limit."}`, http.StatusTooManyRequests},
		{"other", `{"code":3003,"message":"all models failed"}`, http.StatusBadRequest},
		{"large", `{"message":"Your requests have exceeded the quota.","padding":"` + strings.Repeat("x", 65536) + `"}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(tc.body))}
			normalizeTraeCNLimitHTTPResponse(resp)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil || string(body) != tc.body || resp.StatusCode != tc.want {
				t.Fatalf("status=%d body_preserved=%t err=%v", resp.StatusCode, string(body) == tc.body, err)
			}
		})
	}
}
