package api

import (
	"fmt"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/codex2api/internal/diag"
	"github.com/gin-gonic/gin"
)

// DiagnosticMiddleware must wrap recovery to observe the final response status.
func DiagnosticMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
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
		if status < 500 {
			return
		}
		e := diagnosticEvent(c, "http", status)
		e.Message = http.StatusText(status)
		if len(c.Errors) > 0 {
			e.Message = c.Errors.Last().Error()
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
