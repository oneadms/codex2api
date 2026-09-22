package proxy

import (
	"context"
	"net/http"
	"strings"

	"github.com/tidwall/sjson"
)

func CodexTicketSessionForRequest(ctx context.Context) string {
	if ticket := codexTicketFromContext(ctx); ticket != nil {
		return ticket.HarvestSessionID
	}
	return ""
}

// 门票绑定参考 sub2api v2.8.0 的出口、会话快照语义，接入本项目的请求上下文。
var codexTicketForeignIdentityHeaders = []string{
	"Session-Id", "Session_id", "Conversation_id", "Thread-Id", "Thread_id",
	"Turn-Id", "Turn_id", "Installation_id", "X-Codex-Installation-Id",
	"Window_id", "X-Codex-Window-Id", "X-Client-Request-Id", "X-Codex-Turn-Metadata",
}

func applyCodexTicketSession(headers http.Header, session string) {
	if headers == nil || session == "" {
		return
	}
	for _, name := range codexTicketForeignIdentityHeaders {
		headers.Del(name)
	}
	// 采票和业务使用同一种会话头，避免客户端身份覆盖采票身份。
	headers.Set("Session_id", session)
}

// CodexTicketProxyForRequest 只使用已选票据里的出口，配置热更新不会改变在途请求。
func CodexTicketProxyForRequest(ctx context.Context, fallback string) string {
	if ticket := codexTicketFromContext(ctx); ticket != nil && strings.TrimSpace(ticket.HarvestProxyURL) != "" {
		return ticket.HarvestProxyURL
	}
	return fallback
}

// ApplyCodexTicketRequestBody 在最终传输编码前去掉与采票会话冲突的身份。
// WS 的 turn-state 保留在帧内，以便同一连接上的后续请求更新票据。
func ApplyCodexTicketRequestBody(ctx context.Context, body []byte, websocket bool) []byte {
	ticket := codexTicketFromContext(ctx)
	if ticket == nil || ticket.HarvestSessionID == "" {
		return body
	}
	for _, key := range []string{"prompt_cache_key", "client_metadata", "device_id"} {
		if next, err := sjson.DeleteBytes(body, key); err == nil {
			body = next
		}
	}
	if websocket {
		if next, err := sjson.SetBytes(body, "client_metadata."+codexTurnStateMetadataKey, ticket.State); err == nil {
			body = next
		}
	}
	return body
}
