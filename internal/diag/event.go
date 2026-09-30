// Package diag collects bounded diagnostic events and prepares reviewable repairs.
// Model output is data: it is never executed on the service or worker host.
package diag

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/codex2api/internal/util/logredact"
)

type Event struct {
	Time           time.Time        `json:"time"`
	Kind           string           `json:"kind"`
	Method         string           `json:"method,omitempty"`
	Route          string           `json:"route,omitempty"`
	Status         int              `json:"status"`
	Model          string           `json:"model,omitempty"`
	RequestID      string           `json:"request_id,omitempty"`
	Message        string           `json:"message"`
	Stack          string           `json:"stack,omitempty"`
	Revision       string           `json:"revision,omitempty"`
	ErrorCode      string           `json:"error_code,omitempty"`
	ErrorType      string           `json:"error_type,omitempty"`
	SchedulerState string           `json:"scheduler_state,omitempty"`
	Upstream       *UpstreamAttempt `json:"upstream,omitempty"`
}

// UpstreamAttempt is an allowlist of request-correlated diagnostics, without
// credentials, request bodies, proxy addresses or account names.
type UpstreamAttempt struct {
	Status    int    `json:"status"`
	ErrorKind string `json:"error_kind,omitempty"`
	Message   string `json:"message,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	AccountID int64  `json:"account_id,omitempty"`
	Attempt   int    `json:"attempt,omitempty"`
	Retry     bool   `json:"retry,omitempty"`
}

var (
	bearerPattern = regexp.MustCompile(`(?i)\bBearer\s+[^\s,"']+`)
	keyPattern    = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]+|eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+)\b`)
	emailPattern  = regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`)
	urlPattern    = regexp.MustCompile(`https?://[^\s"'<>]+`)
	uuidPattern   = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	numberPattern = regexp.MustCompile(`\b(?:0x[0-9a-fA-F]+|[0-9]+)\b`)
	shaPattern    = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// SafeText is a last line of redaction, not a substitute for field allowlists.
func SafeText(s string) string {
	s = bearerPattern.ReplaceAllString(s, "Bearer [redacted]")
	s = keyPattern.ReplaceAllString(s, "[credential]")
	s = emailPattern.ReplaceAllString(s, "[email]")
	s = urlPattern.ReplaceAllStringFunc(s, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return "[url]"
		}
		u.User, u.RawQuery, u.Fragment = nil, "", ""
		return u.String()
	})
	s = logredact.RedactText(s, "api_key", "apikey", "token", "secret", "authorization", "cookie", "set-cookie")
	return strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, s)
}

func bounded(s string, n int) string {
	if len(s) > n {
		s = s[:n]
	}
	return strings.ToValidUTF8(s, "")
}

func sanitizeEvent(e Event) Event {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	e.Kind = bounded(SafeText(e.Kind), 32)
	e.Method = bounded(SafeText(e.Method), 16)
	e.Route = bounded(SafeText(strings.SplitN(e.Route, "?", 2)[0]), 256)
	e.Model = bounded(SafeText(e.Model), 128)
	e.RequestID = bounded(SafeText(e.RequestID), 128)
	e.Message = bounded(SafeText(bounded(e.Message, 8192)), 2048)
	e.Stack = bounded(SafeText(e.Stack), 8192)
	e.ErrorCode = bounded(SafeText(e.ErrorCode), 128)
	e.ErrorType = bounded(SafeText(e.ErrorType), 128)
	e.SchedulerState = bounded(SafeText(e.SchedulerState), 64)
	if e.Upstream != nil {
		u := *e.Upstream
		u.ErrorKind = bounded(SafeText(u.ErrorKind), 128)
		u.Message = bounded(SafeText(bounded(u.Message, 8192)), 2048)
		u.RequestID = bounded(SafeText(u.RequestID), 128)
		e.Upstream = &u
	}
	if !shaPattern.MatchString(e.Revision) {
		e.Revision = ""
	}
	return e
}

func Fingerprint(e Event) string {
	e = sanitizeEvent(e)
	message := uuidPattern.ReplaceAllString(e.Message, "<id>")
	message = numberPattern.ReplaceAllString(message, "<n>")
	// Stack addresses and goroutine IDs are volatile; retain file/line locations.
	var frames []string
	for _, line := range strings.Split(e.Stack, "\n") {
		if strings.Contains(line, ".go:") && !strings.Contains(line, "/runtime/") && !strings.Contains(line, "/internal/diag/") {
			frames = append(frames, strings.Fields(strings.TrimSpace(line))[0])
		}
		if len(frames) == 4 {
			break
		}
	}
	key := fmt.Sprintf("%s|%s|%s|%d|%s|%s|%s", e.Kind, e.Method, e.Route, e.Status, e.Model, message, strings.Join(frames, "|"))
	// Keep legacy fingerprints stable. IDs, account selection and retry numbers
	// are evidence, not causes, so they must not split otherwise identical errors.
	if e.ErrorCode != "" || e.ErrorType != "" || e.SchedulerState != "" || e.Upstream != nil {
		key += fmt.Sprintf("|%s|%s|%s", e.ErrorCode, e.ErrorType, e.SchedulerState)
		if e.Upstream != nil {
			key += fmt.Sprintf("|%d|%s", e.Upstream.Status, e.Upstream.ErrorKind)
		}
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// UpstreamMessage extracts only the error envelope, never prompts or request bodies.
func UpstreamMessage(body []byte) string {
	if len(body) > 64<<10 {
		return "upstream error (large body omitted)"
	}
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return "upstream error (non-JSON body omitted)"
	}
	var detail struct {
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
	}
	if json.Unmarshal(envelope.Error, &detail) != nil {
		return "upstream error (unrecognized body omitted)"
	}
	// code is expected to be a scalar, never an arbitrary nested payload.
	var code string
	if len(detail.Code) <= 128 && json.Unmarshal(detail.Code, &code) != nil {
		var number json.Number
		if json.Unmarshal(detail.Code, &number) == nil {
			code = number.String()
		}
	}
	return bounded(SafeText(fmt.Sprintf("%s %s %s", detail.Type, code, detail.Message)), 2048)
}

// ResponseError extracts only scalar error fields. Unknown and oversized bodies
// are omitted rather than copied to diagnostic logs.
func ResponseError(body []byte) (code, kind, message string) {
	if len(body) > 64<<10 {
		return
	}
	var envelope struct {
		Error struct {
			Code    json.RawMessage `json:"code"`
			Type    string          `json:"type"`
			Message string          `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil {
		return
	}
	if len(envelope.Error.Code) <= 128 && json.Unmarshal(envelope.Error.Code, &code) != nil {
		var n json.Number
		if json.Unmarshal(envelope.Error.Code, &n) == nil {
			code = n.String()
		}
	}
	return bounded(SafeText(code), 128), bounded(SafeText(envelope.Error.Type), 128), bounded(SafeText(envelope.Error.Message), 2048)
}
