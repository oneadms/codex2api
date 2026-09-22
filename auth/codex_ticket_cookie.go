package auth

import (
	"net/http"
	"strings"
	"time"
)

const maxCodexTicketCookieBytes = 8192

// CodexTicketCookieFreshness 是 Cookie 的注入窗口，与 turn-state 的签发时间分开计算。
const CodexTicketCookieFreshness = 240 * time.Second

// CodexHarvestCookie 复用同账号、同模型仍新鲜的 Cookie，不要求旧票本身仍可注入。
func (a *Account) CodexHarvestCookie(model string, now time.Time) string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	previous := a.CodexTickets[NormalizeCodexTicketModel(model)]
	if previous.CookieFresh(now) {
		return NormalizeCodexTicketCookie(previous.Cookie)
	}
	return ""
}

func (t *CodexTicket) CookieEffectiveExpiry() time.Time {
	if t == nil || NormalizeCodexTicketCookie(t.Cookie) == "" {
		return time.Time{}
	}
	captured := t.CookieCapturedAt
	if captured.IsZero() {
		captured = t.CapturedAt
	}
	if captured.IsZero() {
		// 兼容尚未保存捕获时间的旧记录，不能借重载把窗口重新计时。
		shape, _ := ParseCodexTicketShape(t.State)
		captured = shape.IssuedAt
	}
	if captured.IsZero() {
		return time.Time{}
	}
	expires := captured.Add(CodexTicketCookieFreshness)
	if !t.CookieExpiresAt.IsZero() && t.CookieExpiresAt.Before(expires) {
		expires = t.CookieExpiresAt
	}
	return expires
}

func (t *CodexTicket) CookieFresh(now time.Time) bool {
	expires := t.CookieEffectiveExpiry()
	return !expires.IsZero() && now.Before(expires)
}

func (t *CodexTicket) CookieCount() int {
	if t == nil || NormalizeCodexTicketCookie(t.Cookie) == "" {
		return 0
	}
	cookies, _ := http.ParseCookie(t.Cookie)
	return len(cookies)
}

// NormalizeCodexTicketCookie 只接受可直接写入请求头的 Cookie，拒绝控制字符和损坏的凭据。
func NormalizeCodexTicketCookie(value string) string {
	if len(value) > maxCodexTicketCookieBytes {
		return ""
	}
	for _, r := range value {
		if r < 0x20 || r >= 0x7f {
			return ""
		}
	}
	value = strings.TrimSpace(value)
	cookies, err := http.ParseCookie(value)
	if err != nil || len(cookies) == 0 {
		return ""
	}
	return value
}
