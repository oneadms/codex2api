package api

import (
	"bytes"
	"fmt"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/codex2api/internal/diag"
	"github.com/gin-gonic/gin"
)

const (
	diagnosticErrorKey     = "diag_error"
	diagnosticUpstreamKey  = "diag_upstream"
	diagnosticSchedulerKey = "diag_scheduler"
	diagnosticBodyLimit    = 64 << 10
)

// SetDiagnosticError also covers errors sent after HTTP 200 SSE headers.
func SetDiagnosticError(c *gin.Context, err *APIError, status int) {
	if c == nil || err == nil || !diag.Enabled() {
		return
	}
	c.Set(diagnosticErrorKey, diag.Event{Status: status, ErrorCode: string(err.Code), ErrorType: string(err.Type), Message: err.Message})
}

func SetDiagnosticScheduler(c *gin.Context, state string) {
	if c != nil && diag.Enabled() {
		c.Set(diagnosticSchedulerKey, state)
	}
}

func SetDiagnosticUpstream(c *gin.Context, attempt diag.UpstreamAttempt) {
	if c != nil && diag.Enabled() {
		c.Set(diagnosticUpstreamKey, attempt)
	}
}

// Capture only failed JSON responses. Never buffer successful responses, SSE
// data, requests or headers; keep the writer's flush/hijack behavior intact.
type diagnosticResponseWriter struct {
	gin.ResponseWriter
	body      bytes.Buffer
	oversized bool
}

func (w *diagnosticResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *diagnosticResponseWriter) capture(p []byte) {
	if w.oversized || w.Status() < 500 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		return
	}
	if w.body.Len()+len(p) > diagnosticBodyLimit {
		w.body.Reset()
		w.oversized = true
		return
	}
	_, _ = w.body.Write(p)
}

func (w *diagnosticResponseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.capture(p[:n])
	return n, err
}

func (w *diagnosticResponseWriter) WriteString(s string) (int, error) {
	n, err := w.ResponseWriter.WriteString(s)
	if w.Status() >= 500 {
		w.capture([]byte(s[:n]))
	}
	return n, err
}

// DiagnosticMiddleware must wrap recovery to observe the final response status.
func DiagnosticMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !diag.Enabled() {
			c.Next()
			return
		}
		writer := &diagnosticResponseWriter{ResponseWriter: c.Writer}
		c.Writer = writer
		defer func() { c.Writer = writer.ResponseWriter }()
		c.Next()
		if !diag.Enabled() || c.GetBool("diag_panic") {
			return
		}
		status := c.Writer.Status()
		// Streaming handlers may have already sent HTTP 200 before failing.
		if value, ok := c.Get("x-access-log-status"); ok {
			if override, ok := value.(int); ok && override >= 100 && override <= 599 {
				status = override
			}
		}
		detail, _ := c.Get(diagnosticErrorKey)
		failure, _ := detail.(diag.Event)
		if status == http.StatusOK && c.GetString(diagnosticSchedulerKey) != "" {
			status = http.StatusServiceUnavailable
		} else if status == http.StatusOK && failure.Status >= 500 {
			status = failure.Status
		}
		if status < 500 {
			return
		}
		e := diagnosticEvent(c, "http", status)
		e.Message = http.StatusText(status)
		e.ErrorCode, e.ErrorType = failure.ErrorCode, failure.ErrorType
		if failure.Message != "" {
			e.Message = failure.Message
		}
		code, kind, message := diag.ResponseError(writer.body.Bytes())
		if code != "" || kind != "" || message != "" {
			e.ErrorCode, e.ErrorType = code, kind
			if message != "" {
				e.Message = message
			}
		} else if failure.Message == "" && len(c.Errors) > 0 {
			e.Message = c.Errors.Last().Error()
		}
		e.SchedulerState = c.GetString(diagnosticSchedulerKey)
		if e.SchedulerState == "" {
			switch e.ErrorCode {
			case "no_available_account", "account_pool_concurrency_saturated":
				e.SchedulerState = e.ErrorCode
			}
		}
		if value, ok := c.Get(diagnosticUpstreamKey); ok {
			if upstream, ok := value.(diag.UpstreamAttempt); ok {
				e.Upstream = &upstream
				if failure.Message == "" && message == "" && upstream.Message != "" {
					e.Message = upstream.Message
				}
			}
		}
		diag.Record(e)
	}
}

func diagnosticEvent(c *gin.Context, kind string, status int) diag.Event {
	route := c.FullPath()
	if route == "" {
		route = "<unmatched>"
	} // Never log a user-controlled raw URL.
	e := diag.Event{Kind: kind, Method: c.Request.Method, Route: route, Status: status, Model: c.GetString("x-model")}
	if rc := GetRequestContext(c); rc != nil {
		e.RequestID = rc.RequestID
	}
	return e
}

func recordDiagnosticPanic(c *gin.Context, recovered any) {
	if !diag.Enabled() {
		return
	}
	c.Set("diag_panic", true)
	e := diagnosticEvent(c, "panic", http.StatusInternalServerError)
	e.Message = fmt.Sprintf("%v", recovered)
	// Only repository frames, with no function arguments, home paths or headers.
	pcs := make([]uintptr, 32)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	var stack strings.Builder
	for {
		frame, more := frames.Next()
		pkg := strings.TrimPrefix(frame.Function, "github.com/codex2api/")
		if pkg != frame.Function || strings.HasPrefix(pkg, "main.") {
			if i := strings.IndexByte(pkg, '.'); i >= 0 {
				pkg = pkg[:i]
			}
			file := filepath.Base(frame.File)
			if pkg != "main" {
				file = pkg + "/" + file
			}
			fmt.Fprintf(&stack, "%s\n\t%s:%d\n", frame.Function, file, frame.Line)
		}
		if !more {
			break
		}
	}
	e.Stack = stack.String()
	diag.Record(e)
}
