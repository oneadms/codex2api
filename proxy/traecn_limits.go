package proxy

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

// TRAE 的业务错误可能藏在 HTTP 200 流内，限流与额度耗尽都需要按 429 处理。
func IsTraeCNRateLimitError(payload []byte) bool {
	return traeCNLimitPayloadKind(payload) != ""
}

func IsTraeCNQuotaError(payload []byte) bool {
	return traeCNLimitPayloadKind(payload) == "usage_limit"
}

func traeCNLimitPayloadKind(payload []byte) string {
	if !gjson.ValidBytes(payload) {
		return ""
	}
	root := gjson.ParseBytes(payload)
	if root.Type == gjson.String {
		root = gjson.Parse(root.String())
	}
	for depth := 0; depth <= 4; depth++ {
		current := root
		explicitError := false
		for _, path := range []string{"response.error", "response.status_details.error", "error"} {
			if nested := root.Get(path); nested.IsObject() {
				current, explicitError = nested, true
				break
			}
		}
		code := traeFirstText(current, "code", "error_code", "errorCode") + " " + current.Get("type").String()
		message := traeFirstText(current, "message", "msg", "detail", "extra.message")
		if message == "" && current.Get("error").Type == gjson.String {
			message = current.Get("error").String()
		}
		kind := traeCNLimitKind(code, message)
		if kind != "" || explicitError {
			return kind
		}
		if nested := root.Get("data"); nested.IsObject() {
			root = nested
		} else {
			break
		}
	}
	return ""
}

func traeCNLimitKind(code, message string) string {
	fields := strings.Fields(strings.ToLower(code))
	message = strings.ToLower(strings.TrimSpace(message))
	for _, field := range fields {
		switch field {
		case "insufficient_quota", "quota_exceeded", "quota_exhausted", "usage_limit_reached", "usage_limited":
			return "usage_limit"
		}
	}
	if strings.Contains(message, "exceeded the quota") || strings.Contains(message, "quota has been exhausted") {
		return "usage_limit"
	}
	for _, field := range fields {
		switch field {
		case "4011", "429", "rate_limit", "rate_limited", "rate_limit_exceeded":
			return "rate_limited"
		}
	}
	if strings.Contains(message, "exceeded the rate limit") || strings.Contains(message, "too many requests") || strings.Contains(message, "rate limit exceeded") {
		return "rate_limited"
	}
	return ""
}

// 错误体只检查有界前缀，并原样交还调用方；大型或非 JSON 错误不猜测状态码。
func normalizeTraeCNLimitHTTPResponse(resp *http.Response) {
	if resp == nil || resp.Body == nil || resp.StatusCode < 400 {
		return
	}
	const maxErrorBytes = 64 << 10
	original := resp.Body
	prefix, err := io.ReadAll(io.LimitReader(original, maxErrorBytes+1))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(prefix), original), original}
	if err == nil && len(prefix) <= maxErrorBytes && IsTraeCNRateLimitError(prefix) {
		resp.StatusCode = http.StatusTooManyRequests
		resp.Status = "429 Too Many Requests"
	}
}

// 账号被明确拒绝时需要退出调度，不能受普通中转默认关闭模型冷却的设置影响。
// 未给出恢复时间时采用短期冷却；该时长不代表上游额度的实际重置周期。
func applyTraeCNLimitCooldown(store *auth.Store, account *auth.Account, payload []byte, resp *http.Response) codex429Decision {
	now := time.Now()
	// 额度不足的报错里带着分池余额：记下来，下一次请求就能切到 Work 池。
	recordTraeCNCreditsRemainFromPayload(account, payload)
	reason, duration := "rate_limited", time.Minute
	if IsTraeCNQuotaError(payload) {
		reason, duration = "usage_limit", 5*time.Minute
		// Code 池见底但 Work 池还有额度时，账号仍然可用：只做短冷却，让紧接着的
		// 重试（此时已切到 access_type=1）先把 Work 池用起来。
		if account != nil && account.TraeCNShouldUseWorkPool() {
			reason, duration = "usage_limit", traeCNWorkPoolSwitchCooldown
		}
	}
	hint := parseRetryAfterHeaderAt(firstGJSONString(payload, "error.retry_after", "response.error.retry_after", "response.status_details.error.retry_after", "retry_after"), now)
	if resetAt, ok := parseRetryAfterResetAt(responseFailedErrorBody(payload), now); ok {
		hint = max(hint, resetAt.Sub(now))
	}
	if resp != nil {
		hint = max(hint, parseRetryAfterHeaderAt(resp.Header.Get("Retry-After"), now))
	}
	if hint > 0 {
		duration = min(hint, 24*time.Hour)
	}
	if account != nil {
		account.Mu().RLock()
		if account.CooldownReason == "rate_limited" && account.CooldownUtil.After(now.Add(duration)) {
			duration = account.CooldownUtil.Sub(now)
		}
		account.Mu().RUnlock()
	}
	decision := codex429Decision{Scope: rateLimitScopeAccount, Reason: reason, Cooldown: duration, ResetAt: now.Add(duration)}
	if store != nil && account != nil {
		store.MarkCooldownWithError(account, duration, "rate_limited", usageLogErrorMessage(http.StatusTooManyRequests, payload))
		log.Printf("[TRAECN] stage=cooldown account=%d reason=%q cooldown_seconds=%d", account.ID(), reason, int(duration/time.Second))
	}
	return decision
}

// traeCNWorkPoolSwitchCooldown 是「换池」用的短冷却：IDE 池刚报额度不足、Work 池还
// 有积分时只挡一下，让重试尽快用 Work 端点，而不是把账号整整停 5 分钟。
const traeCNWorkPoolSwitchCooldown = 5 * time.Second
