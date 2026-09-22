package wsrelay

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// websocketCookieKey 只将摘要写进连接池键，避免凭据出现在诊断日志中。
func websocketCookieKey(cookie string, session ...string) string {
	identity := strings.Join(session, "\x00")
	if cookie == "" && identity == "" {
		return ""
	}
	if identity != "" {
		cookie += "\x00" + identity
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(cookie)))
}

func withWebsocketCookieKey(sessionKey, cookieKey string) string {
	if sessionKey == "" || cookieKey == "" {
		return sessionKey
	}
	return sessionKey + "#cookie-" + cookieKey
}
