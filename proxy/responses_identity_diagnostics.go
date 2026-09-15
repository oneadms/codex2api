package proxy

import (
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// beginResponsesIdentityDiagnostics 记录入口实际收到的标识，供断线重试前后对照。
// 请求头只用于诊断，不作为已验证的用户身份；日志不包含凭据或消息正文。
func beginResponsesIdentityDiagnostics(c *gin.Context, body []byte) func() {
	if c == nil || c.Request == nil {
		return func() {}
	}
	started := time.Now()
	clientCtx := c.Request.Context()
	gatewayRequestID := ensurePromptPolicyRequestCorrelationID(c)
	newAPIRequestID := responsesIdentityLogValue(c.GetHeader("X-NewAPI-Request-ID"))
	newAPIUserIDPresent := strings.TrimSpace(c.GetHeader("X-NewAPI-User-ID")) != ""
	headers := c.Request.Header
	clientMetadata := gjson.GetBytes(body, "client_metadata")
	turnMetadata := gjson.Parse(headers.Get(codexTurnMetadataHeader))
	embeddedTurnMetadata := clientMetadata.Get("x-codex-turn-metadata")
	if embeddedTurnMetadata.Type == gjson.String {
		embeddedTurnMetadata = gjson.Parse(embeddedTurnMetadata.String())
	}
	sessionID := responsesIdentityLogValue(firstNonEmptyString(
		headers.Get("Session-Id"), headers.Get("Session_id"),
		turnMetadata.Get("session_id").String(), clientMetadata.Get("session_id").String(),
		embeddedTurnMetadata.Get("session_id").String(),
	))
	threadID := responsesIdentityLogValue(firstNonEmptyString(
		headers.Get("Thread-Id"), headers.Get("Thread_id"),
		turnMetadata.Get("thread_id").String(), clientMetadata.Get("thread_id").String(),
		embeddedTurnMetadata.Get("thread_id").String(),
	))
	turnID := responsesIdentityLogValue(firstNonEmptyString(
		turnMetadata.Get("turn_id").String(), embeddedTurnMetadata.Get("turn_id").String(),
	))
	clientRequestID := responsesIdentityLogValue(headers.Get("X-Client-Request-Id"))
	model := responsesIdentityLogValue(gjson.GetBytes(body, "model").String())
	stream := gjson.GetBytes(body, "stream").Bool()

	log.Printf("[RESPONSES-IDENTITY] stage=ingress gateway_request_id=%q newapi_request_id=%q newapi_request_id_present=%t newapi_user_id_present=%t api_key_id=%d session_id=%q thread_id=%q turn_id=%q client_request_id=%q model=%q stream=%t",
		gatewayRequestID, newAPIRequestID, newAPIRequestID != "", newAPIUserIDPresent,
		requestAPIKeyID(c), sessionID, threadID, turnID, clientRequestID, model, stream)

	return func() {
		clientContextError := ""
		if err := clientCtx.Err(); err != nil {
			clientContextError = err.Error()
		}
		log.Printf("[RESPONSES-IDENTITY] stage=finish gateway_request_id=%q newapi_request_id=%q status=%d duration_ms=%d response_bytes=%d client_context_error=%q",
			gatewayRequestID, newAPIRequestID, c.Writer.Status(), time.Since(started).Milliseconds(), c.Writer.Size(), clientContextError)
	}
}

// responsesIdentityLogValue 限制单个标识的长度；调用处使用 %q 转义换行等控制字符。
func responsesIdentityLogValue(value string) string {
	value = strings.TrimSpace(value)
	const maxBytes = 256
	if len(value) <= maxBytes {
		return value
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + "..."
}
