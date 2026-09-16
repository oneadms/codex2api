package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

// 明确鉴权拒绝可在原任务内换号；鉴权、限流预算耗尽后的下游重试不能被失败缓存锁死。
// 未知 5xx 的下游重试同样降级为全新生成，不再把 409 交给客户端。
func TestTraeCNResumeRejectedRequestRecovery(t *testing.T) {
	t.Setenv("TRAECN_RESUME_ENABLED", "1")
	for _, testCase := range []struct {
		name         string
		budget       int
		status       int
		rejectAll    bool
		streamReject bool
	}{
		{name: "switch_account", budget: 1, status: http.StatusUnauthorized},
		{name: "retry_after_budget_exhausted", budget: 0, status: http.StatusUnauthorized},
		{name: "bounded_auth_attempts", budget: 1, status: http.StatusUnauthorized, rejectAll: true},
		{name: "ambiguous_server_failure", budget: 1, status: http.StatusInternalServerError},
		{name: "http_rate_limit_recovery", budget: 0, status: http.StatusTooManyRequests},
		{name: "sse_rate_limit_recovery", budget: 0, status: http.StatusTooManyRequests, streamReject: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var calls atomic.Int32
			var credentials sync.Map
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				attempt := calls.Add(1)
				if _, loaded := credentials.LoadOrStore(request.Header.Get("X-Ide-Token"), true); loaded {
					t.Error("reused a rejected credential")
				}
				if attempt == 1 || testCase.rejectAll {
					if testCase.streamReject {
						writer.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(writer, "event: error\ndata: {\"code\":3004,\"message\":\"Your requests have exceeded the rate limit.\"}\n\n")
						return
					}
					writer.Header().Set("Content-Type", "application/json")
					writer.WriteHeader(testCase.status)
					_, _ = io.WriteString(writer, `{"code":1001,"message":"authentication rejected"}`)
					return
				}
				writer.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(writer, "event: output\ndata: {\"content\":\"recovered\"}\n\nevent: done\ndata: {}\n\n")
			}))
			t.Cleanup(upstream.Close)
			store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, MaxRetries: testCase.budget})
			t.Cleanup(store.Stop)
			store.SetMaxRetries(testCase.budget)
			store.SetRetryIntervalMS(0)
			for index, token := range []string{"AT-a", "AT-b", "AT-c"} {
				store.AddAccount(&auth.Account{DBID: int64(91800 + index), UpstreamType: auth.UpstreamTraeCN, AccessToken: token, ExpiresAt: time.Now().Add(time.Hour), TraeCNHost: upstream.URL})
			}
			handler := NewHandler(store, nil, nil, nil)
			t.Cleanup(func() { cleanupTraeCNResumeRegistry(t, &handler.traeCNResumeTasks) })
			request, recorder := traeCNResumeTestContext(traeCNResumeTestBody, testCase.name)
			handler.Responses(request)
			if testCase.rejectAll {
				if calls.Load() != 2 || recorder.Code != http.StatusServiceUnavailable {
					t.Fatalf("unbounded retry: calls=%d status=%d body=%s", calls.Load(), recorder.Code, recorder.Body.String())
				}
				return
			}
			if testCase.status == http.StatusInternalServerError {
				// 任务内不自动重试状态不明的 5xx；客户端重发时放弃续传、按普通流程重新生成。
				retry, recovered := traeCNResumeTestContext(traeCNResumeTestBody, testCase.name)
				handler.Responses(retry)
				if calls.Load() != 2 || recovered.Code != http.StatusOK || !strings.Contains(recovered.Body.String(), "response.completed") {
					t.Fatalf("ambiguous failure not recovered: calls=%d status=%d body=%s", calls.Load(), recovered.Code, recovered.Body.String())
				}
				return
			}
			if testCase.budget == 0 {
				wantStatus := http.StatusServiceUnavailable
				if testCase.status == http.StatusTooManyRequests {
					wantStatus = http.StatusTooManyRequests
				}
				if calls.Load() != 1 || recorder.Code != wantStatus {
					t.Fatalf("ignored disabled budget: calls=%d status=%d", calls.Load(), recorder.Code)
				}
				retry, recovered := traeCNResumeTestContext(traeCNResumeTestBody, testCase.name)
				handler.Responses(retry)
				recorder = recovered
			}
			if calls.Load() != 2 || recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "response.completed") {
				t.Fatalf("recovery failed: calls=%d status=%d body=%s", calls.Load(), recorder.Code, recorder.Body.String())
			}
			if strings.Contains(recorder.Body.String(), "response.failed") {
				t.Fatal("rejected attempt leaked into recovered stream")
			}
		})
	}
}

func TestTraeCNResumeSafeFailureWaitsForCleanup(t *testing.T) {
	for _, finished := range []bool{false, true} {
		var registry traeCNResumeRegistry
		t.Cleanup(func() { cleanupTraeCNResumeRegistry(t, &registry) })
		identity := traeCNResumeIdentity{key: "owner", root: "root"}
		task, _, reader, _, _ := registry.acquire(identity, 10)
		task.retrySafe = true
		writer := newTraeCNResumeWriter(task)
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = writer.WriteString(`{"error":{"code":"no_available_account"}}`)
		if finished {
			writer.finish()
		} else {
			registry.detach(task, reader)
		}
		matched, _, generation, created, reason := registry.acquire(identity, 10)
		if matched != task || created || reason != "" || generation != reader+1 || registry.inputBytes != 10 || registry.count != 1 {
			t.Fatalf("replaced before cleanup: finished=%t created=%t reason=%s", finished, created, reason)
		}
		writer.finish()
		request, recorder := traeCNResumeTestContext(traeCNResumeTestBody, "cleanup")
		serveTraeCNResumeTask(request, task, nil, generation)
		if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "no_available_account") {
			t.Fatalf("lost original failure: %d %s", recorder.Code, recorder.Body.String())
		}
	}
}

// 上游明确失败（含已交付正文）时替换失败记录重新调度；没有终态或未完整生成仍然拒绝替换。
func TestTraeCNResumeSafeFailureReplacement(t *testing.T) {
	for _, mode := range []string{"http", "sse", "expired", "published", "unknown", "incomplete", "delivered"} {
		t.Run(mode, func(t *testing.T) {
			var registry traeCNResumeRegistry
			t.Cleanup(func() { cleanupTraeCNResumeRegistry(t, &registry) })
			identity := traeCNResumeIdentity{key: "owner", root: "root"}
			task, _, reader, _, _ := registry.acquire(identity, 10)
			request, _ := traeCNResumeTestContext(traeCNResumeTestBody, mode)
			request.Set(traeCNResumeTaskKey, task)
			markTraeCNResumeRetrySafe(request, mode != "unknown")
			writer := newTraeCNResumeWriter(task)
			if mode == "http" || mode == "expired" {
				writer.WriteHeader(http.StatusServiceUnavailable)
				_, _ = writer.WriteString(`{"error":{"code":"no_available_account"}}`)
			} else {
				writer.Header().Set("Content-Type", "text/event-stream")
				if mode == "published" {
					writeTraeCNResumeTestEvent(t, writer, `{"type":"response.created","response":{"id":"resp-a"}}`)
					markTraeCNResumeRetrySafe(request, true)
				}
				if mode == "delivered" {
					writeTraeCNResumeTestEvent(t, writer, `{"type":"response.output_text.delta","item_id":"msg-a","delta":"部分输出"}`)
				}
				if mode != "incomplete" {
					writeTraeCNResumeTestEvent(t, writer, `{"type":"response.failed","response":{"error":{"code":"upstream_error"}}}`)
				}
			}
			writer.finish()
			registry.mu.Lock()
			registry.inputBytes -= 10
			registry.mu.Unlock()
			registry.detach(task, reader)
			if mode == "expired" {
				registry.expire(task, reader)
			}
			replacement, _, _, created, reason := registry.acquire(identity, 10)
			if mode == "incomplete" {
				if created || reason == "" {
					t.Fatalf("unsafe replacement: created=%t reason=%s", created, reason)
				}
				return
			}
			if !created || reason != "" || replacement == task || registry.count != 1 || registry.inputBytes != 10 || task.log.reserved != 0 {
				t.Fatalf("invalid replacement: created=%t reason=%s count=%d bytes=%d", created, reason, registry.count, registry.inputBytes)
			}
			registry.forget(task)
			var workers sync.WaitGroup
			for range 8 {
				workers.Go(func() {
					matched, _, _, fresh, rejection := registry.acquire(identity, 10)
					if matched != replacement || fresh || rejection != "" {
						t.Error("concurrent retry created another task")
					}
				})
			}
			workers.Wait()
		})
	}
}

func TestTraeCNResumePoolRecovery(t *testing.T) {
	t.Setenv("TRAECN_RESUME_ENABLED", "1")
	var calls atomic.Int32
	handler := newTraeCNContextTestHandler(t, func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "event: output\ndata: {\"content\":\"pool recovered\"}\n\nevent: done\ndata: {}\n\n")
	})
	t.Cleanup(func() { cleanupTraeCNResumeRegistry(t, &handler.traeCNResumeTasks) })
	account := handler.store.Accounts()[0]
	handler.store.MarkTransientRateLimited(account, time.Minute)
	request, recorder := traeCNResumeTestContext(traeCNResumeTestBody, "pool-recovery")
	handler.Responses(request)
	if calls.Load() != 0 || (recorder.Code < 400 && !strings.Contains(recorder.Body.String(), "response.failed")) {
		t.Fatalf("expected queue failure without upstream dispatch: %d %s", recorder.Code, recorder.Body.String())
	}
	handler.store.AddAccount(&auth.Account{DBID: 91810, UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT-new", ExpiresAt: time.Now().Add(time.Hour), TraeCNHost: account.TraeCNHost})
	retry, recovered := traeCNResumeTestContext(traeCNResumeTestBody, "pool-recovery")
	handler.Responses(retry)
	if calls.Load() != 1 || recovered.Code != http.StatusOK || gjson.GetBytes(recovered.Body.Bytes(), "error.code").String() != "" || !strings.Contains(recovered.Body.String(), "response.completed") {
		t.Fatalf("pool recovery blocked: calls=%d status=%d body=%s", calls.Load(), recovered.Code, recovered.Body.String())
	}
}
