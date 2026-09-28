package proxy

import (
	"context"
	"errors"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/codex2api/security"
)

// 保留断流的原始读错误及两端取消状态，避免仅凭 response.failed 或 499 推断断连先后。
func (s *traeCNCanonicalState) logStreamReadError(readErr error, startedAt, lastReadAt time.Time) {
	requestID, upstreamContext, downstreamContext := "", "", ""
	var ctx context.Context
	if s.diagnostic != nil {
		requestID = s.diagnostic.id
		ctx = s.diagnostic.ctx
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			upstreamContext = err.Error()
		}
		if keepalive, ok := continuousRetryKeepaliveForContext(ctx).(*requestContinuousRetryKeepalive); ok && keepalive.ctx != nil {
			if err := keepalive.ctx.Err(); err != nil {
				downstreamContext = err.Error()
			}
		}
	}
	idleMs := int64(-1)
	if !lastReadAt.IsZero() {
		idleMs = time.Since(lastReadAt).Milliseconds()
	}
	message := traeCNStreamReadErrorSummary(readErr, s.diagnostic)
	log.Printf("[TRAECN] stage=stream_break request_id=%q model=%q response_id=%q read_error=%q upstream_context=%q downstream_context=%q stream_ms=%d upstream_idle_ms=%d",
		requestID, s.model, s.responseID, message, upstreamContext, downstreamContext, time.Since(startedAt).Milliseconds(), idleMs)
}

func traeCNStreamReadErrorSummary(err error, diagnostic *traeCNDiagnostic) string {
	if err == nil {
		return ""
	}
	// Resin 的 URL 路径可能携带凭据，只记录底层网络错误。
	var requestErr *url.Error
	if errors.As(err, &requestErr) {
		err = requestErr.Err
	}
	message := err.Error()
	if diagnostic != nil {
		message = string(diagnostic.redact([]byte(message)))
	}
	message = security.SanitizeLog(strings.Join(strings.Fields(message), " "))
	runes := []rune(message)
	if len(runes) > 1024 {
		message = string(runes[:1024]) + "..."
	}
	return message
}
