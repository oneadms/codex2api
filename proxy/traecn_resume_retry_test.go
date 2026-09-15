package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// 同一后台任务内只替换被明确拒绝的首包前尝试，已发布的思考和工具不能回滚。
func TestTraeCNResumeRateLimitRetryBoundary(t *testing.T) {
	t.Setenv("TRAECN_RESUME_ENABLED", "1")
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := previous
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
	ApplyRuntimeSettings(settings)
	for _, tc := range []struct {
		name, message, output         string
		httpStatus, budget, wantCalls int
	}{
		{"sse_rate_before_output", "Your requests have exceeded the rate limit.", "", 200, 1, 2},
		{"sse_quota_before_output", "Your requests have exceeded the quota.", "", 200, 1, 2},
		{"http_rate_before_output", "Your requests have exceeded the rate limit.", "", 429, 1, 2},
		{"http_quota_incorrect_status", "Your requests have exceeded the quota.", "", 400, 1, 2},
		{"reasoning_rate", "Your requests have exceeded the rate limit.", `{"reasoning_content":"已开始检查"}`, 200, 1, 1},
		{"reasoning_quota", "Your requests have exceeded the quota.", `{"reasoning_content":"已开始检查"}`, 200, 1, 1},
		{"text_rate", "Your requests have exceeded the rate limit.", `{"content":"部分输出"}`, 200, 1, 1},
		{"tool_rate", "Your requests have exceeded the rate limit.", `{"tool_calls":[{"id":"call_a","function_call":{"name":"read_file","arguments":"{\"path\":"}}]}`, 200, 1, 1},
		{"sse_budget_disabled", "Your requests have exceeded the rate limit.", "", 200, 0, 1},
		{"http_budget_disabled", "Your requests have exceeded the quota.", "", 429, 0, 1},
		{"other_failure", "all models failed", "", 200, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var credentials []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				credentials = append(credentials, r.Header.Get("X-Ide-Token"))
				attempt := len(credentials)
				mu.Unlock()
				if attempt > 1 {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: output\ndata: {\"content\":\"completed with the next account\"}\n\nevent: done\ndata: {}\n\n")
					return
				}
				if tc.httpStatus != http.StatusOK {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.httpStatus)
					_, _ = fmt.Fprintf(w, `{"code":3004,"message":%q}`, tc.message)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if tc.output != "" {
					_, _ = fmt.Fprintf(w, "event: output\ndata: %s\n\n", tc.output)
					w.(http.Flusher).Flush()
				}
				_, _ = fmt.Fprintf(w, "event: error\ndata: {\"code\":3004,\"message\":%q}\n\n", tc.message)
			}))
			t.Cleanup(upstream.Close)
			store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, MaxRetries: 3, MaxRateLimitRetries: tc.budget})
			t.Cleanup(store.Stop)
			store.SetRetryIntervalMS(0)
			for i := 0; i < 2; i++ {
				store.AddAccount(&auth.Account{DBID: int64(91710 + i), UpstreamType: auth.UpstreamTraeCN, AccessToken: fmt.Sprintf("AT-%d", i), RefreshToken: "RT", ExpiresAt: time.Now().Add(time.Hour), TraeCNHost: upstream.URL})
			}
			handler := NewHandler(store, nil, nil, nil)
			t.Cleanup(func() { cleanupTraeCNResumeRegistry(t, &handler.traeCNResumeTasks) })
			c, recorder := traeCNResumeTestContext(traeCNResumeTestBody, "rate-limit-retry")
			handler.Responses(c)
			mu.Lock()
			seen := append([]string(nil), credentials...)
			mu.Unlock()
			if len(seen) != tc.wantCalls {
				t.Fatalf("upstream calls=%d want=%d, status=%d body=%s", len(seen), tc.wantCalls, recorder.Code, recorder.Body.String())
			}
			if tc.wantCalls == 2 {
				if seen[0] == "" || seen[0] == seen[1] {
					t.Fatal("rate-limit retry reused the rejected account")
				}
				events := canonicalSSEEvents(t, recorder.Body.Bytes())
				if _, ok := findCanonicalEvent(events, "response.completed"); !ok {
					t.Fatalf("missing successful retry: %s", recorder.Body.String())
				}
				created := 0
				for _, event := range events {
					if event.Get("type").String() == "response.created" {
						created++
					}
					if event.Get("type").String() == "response.failed" {
						t.Fatal("leaked rejected attempt into the task journal")
					}
				}
				if created != 1 {
					t.Fatalf("created events=%d", created)
				}
			} else if tc.output != "" {
				failed, ok := findCanonicalEvent(canonicalSSEEvents(t, recorder.Body.Bytes()), "response.failed")
				if !ok || failed.Get("response.error.status_code").Int() != 429 {
					t.Fatalf("lost midstream failure: %s", recorder.Body.String())
				}
			} else if tc.name != "other_failure" && (recorder.Code != http.StatusTooManyRequests || !strings.Contains(recorder.Body.String(), "3004")) {
				t.Fatalf("lost final 429: %d %s", recorder.Code, recorder.Body.String())
			}
			limited := 0
			for _, account := range store.Accounts() {
				if account.RuntimeStatus() == "rate_limited" {
					limited++
				}
			}
			if tc.name != "other_failure" && limited != 1 {
				t.Fatalf("limited accounts=%d", limited)
			}
			handler.traeCNResumeTasks.mu.Lock()
			taskCount := handler.traeCNResumeTasks.count
			handler.traeCNResumeTasks.mu.Unlock()
			if taskCount != 1 {
				t.Fatalf("retry created %d retained tasks", taskCount)
			}
		})
	}
}
