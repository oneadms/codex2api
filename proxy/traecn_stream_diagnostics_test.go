package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestTraeCNStreamReadErrorSummaryRedactsCredentials(t *testing.T) {
	diagnostic := &traeCNDiagnostic{secrets: []string{"account-secret"}}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"eof", io.EOF, "EOF"},
		{"cancel", context.Canceled, "context canceled"},
		{"reset", errors.New("stream error: INTERNAL_ERROR; received from peer"), "INTERNAL_ERROR"},
		{"resin-url", &url.Error{Op: "POST", URL: "http://resin.invalid/lease-secret/platform/https/traecn.example", Err: io.ErrUnexpectedEOF}, "unexpected EOF"},
		{"credential", errors.New("read failed: account-secret\nnext line"), "[REDACTED] next line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := traeCNStreamReadErrorSummary(tc.err, diagnostic)
			if !strings.Contains(got, tc.want) || strings.ContainsAny(got, "\r\n") {
				t.Fatalf("error summary = %q, want %q", got, tc.want)
			}
			for _, secret := range []string{"account-secret", "lease-secret", "http://resin.invalid"} {
				if strings.Contains(got, secret) {
					t.Fatalf("error summary exposed credential or request URL: %q", got)
				}
			}
		})
	}
}

func TestTraeCNStreamReadDiagnosticReportsCancellationStates(t *testing.T) {
	var output bytes.Buffer
	previousWriter := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previousWriter) })
	for _, canceled := range []bool{false, true} {
		output.Reset()
		downstreamCtx, cancelDownstream := context.WithCancel(context.Background())
		upstreamCtx, cancelUpstream := context.WithCancel(context.WithoutCancel(downstreamCtx))
		upstreamCtx = context.WithValue(upstreamCtx, continuousRetryKeepaliveContextKey{}, &requestContinuousRetryKeepalive{ctx: downstreamCtx})
		if canceled {
			cancelDownstream()
			cancelUpstream()
		}
		state := newTraeCNCanonicalState("qwen3.8-max")
		state.diagnostic = &traeCNDiagnostic{id: "read-diagnostic-test", ctx: upstreamCtx}
		state.logStreamReadError(io.EOF, time.Now().Add(-time.Second), time.Time{})
		cancelDownstream()
		cancelUpstream()
		got := output.String()
		for _, want := range []string{`stage=stream_break`, `request_id="read-diagnostic-test"`, `read_error="EOF"`, `stream_ms=`, `upstream_idle_ms=-1`} {
			if !strings.Contains(got, want) {
				t.Fatalf("diagnostic missing %q: %s", want, got)
			}
		}
		wantContext := `context=""`
		if canceled {
			wantContext = `context="context canceled"`
		}
		if !strings.Contains(got, "upstream_"+wantContext) || !strings.Contains(got, "downstream_"+wantContext) {
			t.Fatalf("incorrect cancellation state: %s", got)
		}
	}
}
