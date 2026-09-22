package auth

import (
	"net/http"
	"strings"
)

const maxCodexTicketCookieBytes = 8192

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
