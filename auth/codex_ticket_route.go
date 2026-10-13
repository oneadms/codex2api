package auth

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// CodexRouteCookie 只保留同次签发的两个路由 Cookie；JWT 声明仅用于检查到期时间。
func CodexRouteCookie(header string, now time.Time) (string, time.Time, bool) {
	cookies, err := http.ParseCookie(NormalizeCodexTicketCookie(header))
	if err != nil {
		return "", time.Time{}, false
	}
	pair := map[string]string{}
	for _, cookie := range cookies {
		if cookie.Name != "__cflb" && cookie.Name != "__oailb" {
			continue
		}
		if cookie.Value == "" || pair[cookie.Name] != "" {
			return "", time.Time{}, false
		}
		pair[cookie.Name] = cookie.Value
	}
	parts := strings.Split(pair["__oailb"], ".")
	if pair["__cflb"] == "" || len(parts) != 3 {
		return "", time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err != nil || json.Unmarshal(raw, &claims) != nil || claims.Exp <= now.Unix() || claims.Exp >= 4102444800 {
		return "", time.Time{}, false
	}
	return "__cflb=" + pair["__cflb"] + "; __oailb=" + pair["__oailb"], time.Unix(claims.Exp, 0), true
}

func ValidCodexRouteCookie(header string, now time.Time) bool {
	_, _, valid := CodexRouteCookie(header, now)
	return valid
}
