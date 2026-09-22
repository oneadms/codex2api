package wsrelay

import (
	"crypto/sha256"
	"fmt"
)

// websocketCookieKey 只将摘要写进连接池键，避免凭据出现在诊断日志中。
func websocketCookieKey(cookie string) string {
	if cookie == "" {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(cookie)))
}

func withWebsocketCookieKey(sessionKey, cookieKey string) string {
	if sessionKey == "" || cookieKey == "" {
		return sessionKey
	}
	return sessionKey + "#cookie-" + cookieKey
}
