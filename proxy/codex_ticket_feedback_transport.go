package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

type codexTicketFeedbackAttemptKey struct{}

// 每次出站独享观测状态；与审计日志解耦，未启用日志时也能续票。
type codexTicketFeedbackAttempt struct {
	account   *auth.Account
	ticket    *auth.CodexTicket
	cookies   codexTicketCookies
	returned  string
	completed bool
}

func withCodexTicketFeedbackAttempt(ctx context.Context, account *auth.Account, ticket *auth.CodexTicket) context.Context {
	captured := ticket.CookieCapturedAt
	if captured.IsZero() {
		captured = ticket.CapturedAt
	}
	if captured.IsZero() {
		shape, _ := auth.ParseCodexTicketShape(ticket.State)
		captured = shape.IssuedAt
	}
	return context.WithValue(ctx, codexTicketFeedbackAttemptKey{}, &codexTicketFeedbackAttempt{
		account: account, ticket: ticket,
		cookies: codexTicketCookies{Header: ticket.Cookie, ExpiresAt: ticket.CookieExpiresAt, CapturedAt: captured},
	})
}

func ticketFeedbackAttempt(ctx context.Context) *codexTicketFeedbackAttempt {
	if ctx == nil || codexTicketFromContext(ctx) == nil {
		return nil
	}
	value, _ := ctx.Value(codexTicketFeedbackAttemptKey{}).(*codexTicketFeedbackAttempt)
	return value
}

// ObserveCodexTicketHandshake 只在连接首次建立时调用，复用旧握手不得刷新 Cookie 时间。
func ObserveCodexTicketHandshake(ctx context.Context, response *http.Response) {
	if attempt := ticketFeedbackAttempt(ctx); attempt != nil && response != nil {
		endpoint, _ := url.Parse(CodexBaseURL + "/responses")
		attempt.cookies = captureCodexTicketCookies(endpoint, attempt.cookies, response)
		attempt.returned = observedCodexTurnState(response.Header.Get(codexTurnStateHeader))
	}
}

// ObserveCodexTicketFeedbackFrame 在终态前收集 metadata，只有成功终态才提交票据链。
func ObserveCodexTicketFeedbackFrame(ctx context.Context, frame []byte) {
	attempt := ticketFeedbackAttempt(ctx)
	if attempt == nil || attempt.completed {
		return
	}
	if state := codexTurnStateFromFrame(frame); state != "" {
		attempt.returned = state
	}
	kind := gjson.GetBytes(frame, "type").String()
	if kind == "error" || kind == "response.failed" {
		for _, path := range []string{"status", "status_code", "response.status_code", "error.status_code"} {
			status := int(gjson.GetBytes(frame, path).Int())
			if status == 401 || status == 403 {
				ObserveCodexTicketRejection(ctx, status)
				break
			}
		}
		attempt.completed = true
		return
	}
	if kind != "response.completed" {
		return
	}
	attempt.completed = true
	observeCodexTicketFeedback(attempt.account, attempt.ticket.State, attempt.returned, http.StatusOK, attempt.cookies, attempt.ticket)
}

func ObserveCodexTicketRejection(ctx context.Context, status int) {
	if attempt := ticketFeedbackAttempt(ctx); attempt != nil && (status == 401 || status == 403) {
		observeCodexTicketFeedback(attempt.account, attempt.ticket.State, "", status, attempt.cookies, attempt.ticket)
	}
}

func observeCodexTicketHTTPResponse(ctx context.Context, response *http.Response) {
	attempt := ticketFeedbackAttempt(ctx)
	if attempt == nil || response == nil {
		return
	}
	ObserveCodexTicketHandshake(ctx, response)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		ObserveCodexTicketRejection(ctx, response.StatusCode)
		return
	}
	if response.Body != nil && strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") {
		response.Body = &codexTicketFeedbackBody{ReadCloser: response.Body, ctx: ctx}
	}
}

// 旁路解析 SSE 不改变下游字节；只有读到成功终态才提交回票。
type codexTicketFeedbackBody struct {
	io.ReadCloser
	ctx      context.Context
	pending  []byte
	disabled bool
}

func (b *codexTicketFeedbackBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if !b.disabled {
		b.pending = append(b.pending, p[:n]...)
		for {
			index := bytes.IndexByte(b.pending, '\n')
			if index < 0 {
				break
			}
			line := bytes.TrimSpace(b.pending[:index])
			if bytes.HasPrefix(line, []byte("data:")) {
				ObserveCodexTicketFeedbackFrame(b.ctx, bytes.TrimSpace(line[5:]))
			}
			b.pending = b.pending[index+1:]
		}
		if len(b.pending) > 4<<20 {
			b.pending = nil
			b.disabled = true
		}
	}
	return n, err
}
