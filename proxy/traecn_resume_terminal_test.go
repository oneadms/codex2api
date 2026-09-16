package proxy

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// 上游已失败的任务不能靠重复回放恢复；只有仍保留原订阅时才会接回，其余交给静默重开策略。
func TestTraeCNResumeRejectsEndedFailures(t *testing.T) {
	for _, tc := range []struct {
		name, event, localFailure, reason, terminalCode string
	}{
		{"incomplete", `{"type":"response.incomplete","response":{"id":"resp-a","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`, "", "resume_task_incomplete", "max_output_tokens"},
		{"eof", "", "", "resume_stream_incomplete", ""},
		{"storage", "", "resume_storage_limit", "resume_storage_limit", ""},
		{"http_error", "", "", "resume_task_failed", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var registry traeCNResumeRegistry
			t.Cleanup(func() { cleanupTraeCNResumeRegistry(t, &registry) })
			identity := traeCNResumeIdentity{key: "owner", root: "root", input: []string{"input"}}
			task, _, reader, _, _ := registry.acquire(identity, 1)
			w := newTraeCNResumeWriter(task)
			if tc.name == "http_error" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.WriteString(`{"error":{"code":"upstream_error"}}`)
			} else {
				w.Header().Set("Content-Type", "text/event-stream")
				writeTraeCNResumeTestEvent(t, w, `{"type":"response.created","response":{"id":"resp-a"}}`)
				if tc.event != "" {
					writeTraeCNResumeTestEvent(t, w, tc.event)
					if _, _, _, _, reason := registry.acquire(identity, 1); reason != tc.reason {
						t.Fatalf("reattached before worker cleanup: %s", reason)
					}
				}
			}
			task.failure = tc.localFailure
			w.finish()
			if _, _, _, created, reason := registry.acquire(identity, 1); created || reason != tc.reason {
				t.Fatalf("ended task reused: created=%t reason=%s", created, reason)
			}
			if registry.count != 1 || task.reader != reader || !task.attached {
				t.Fatal("rejected retry created a task or displaced the original reader")
			}
			if task.terminalCode != tc.terminalCode {
				t.Fatalf("terminal code=%q, want %q", task.terminalCode, tc.terminalCode)
			}
			c, recorder := traeCNResumeTestContext(traeCNResumeTestBody, "req-a")
			serveTraeCNResumeTask(c, task, nil, reader)
			if tc.name == "http_error" {
				if recorder.Code != http.StatusBadGateway {
					t.Fatalf("original status=%d", recorder.Code)
				}
				return
			}
			terminals := 0
			for _, event := range canonicalSSEEvents(t, recorder.Body.Bytes()) {
				switch event.Get("type").String() {
				case "response.failed", "response.incomplete":
					terminals++
				case "response.completed":
					t.Fatal("failure reported as success")
				}
			}
			if terminals != 1 {
				t.Fatalf("original subscriber received %d terminals: %s", terminals, recorder.Body.String())
			}
		})
	}
}

// 客户端重发同一请求时的重新调度边界：上游明确失败就重开，工具已交付与没有终态的失败不重开。
func TestTraeCNResumeFailedTaskRestartPolicy(t *testing.T) {
	failed := `{"type":"response.failed","response":{"id":"resp-a","status":"failed","error":{"code":"upstream_empty_output"}}}`
	created := `{"type":"response.created","response":{"id":"resp-a"}}`
	for _, tc := range []struct {
		name, wantReason string
		events           []string
		wantCreated      bool
	}{
		{name: "reasoning_only", events: []string{
			created,
			`{"type":"response.reasoning_summary_text.delta","item_id":"rs-a","delta":"正在检查"}`,
			failed,
		}, wantCreated: true},
		{name: "empty_output", events: []string{created, failed}, wantCreated: true},
		{name: "delivered_text", events: []string{
			created,
			`{"type":"response.output_text.delta","item_id":"msg-a","delta":"部分输出"}`,
			failed,
		}, wantCreated: true},
		{name: "delivered_tool", wantReason: "resume_tool_call_unsupported", events: []string{
			created,
			`{"type":"response.output_item.done","item":{"id":"fc-a","type":"function_call","call_id":"call-a","name":"shell","arguments":"{}"}}`,
			failed,
		}},
		{name: "policy_refusal", wantReason: "resume_task_failed", events: []string{
			created,
			`{"type":"response.failed","response":{"error":{"code":"cyber_policy","message":"blocked"}}}`,
		}},
		{name: "incomplete", wantReason: "resume_task_incomplete", events: []string{
			created,
			`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}`,
		}},
		{name: "no_terminal_event", wantReason: "resume_stream_incomplete", events: []string{
			created,
			`{"type":"response.output_text.delta","item_id":"msg-a","delta":"半截输出"}`,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var registry traeCNResumeRegistry
			t.Cleanup(func() { cleanupTraeCNResumeRegistry(t, &registry) })
			identity := traeCNResumeIdentity{key: "owner", root: "root", input: []string{"input"}}
			task, _, reader, created, reason := registry.acquire(identity, 1)
			if !created || reason != "" {
				t.Fatalf("first acquire created=%t reason=%s", created, reason)
			}
			w := newTraeCNResumeWriter(task)
			w.Header().Set("Content-Type", "text/event-stream")
			for _, event := range tc.events {
				writeTraeCNResumeTestEvent(t, w, event)
			}
			w.finish()
			registry.detach(task, reader)
			_, _, _, created, reason = registry.acquire(identity, 1)
			if tc.wantCreated {
				if !created || reason != "" {
					t.Fatalf("restart blocked: created=%t reason=%s", created, reason)
				}
				return
			}
			wantReason := tc.wantReason
			if wantReason == "" {
				wantReason = "resume_task_failed"
			}
			if created || reason != wantReason {
				t.Fatalf("unsafe restart allowed: created=%t reason=%s", created, reason)
			}
		})
	}
}

// 静默重开有次数上限，避免上游持续空输出时无限重开。
func TestTraeCNResumeSilentRestartIsBounded(t *testing.T) {
	var registry traeCNResumeRegistry
	t.Cleanup(func() { cleanupTraeCNResumeRegistry(t, &registry) })
	identity := traeCNResumeIdentity{key: "owner", root: "root", input: []string{"input"}}
	replacement, _, reader, created, reason := registry.acquire(identity, 1)
	if !created || reason != "" {
		t.Fatalf("first acquire created=%t reason=%s", created, reason)
	}
	for attempt := 0; attempt <= traeCNResumeSilentRestartLimit; attempt++ {
		w := newTraeCNResumeWriter(replacement)
		w.Header().Set("Content-Type", "text/event-stream")
		writeTraeCNResumeTestEvent(t, w, `{"type":"response.created","response":{"id":"resp-a"}}`)
		writeTraeCNResumeTestEvent(t, w, `{"type":"response.failed","response":{"error":{"code":"upstream_empty_output"}}}`)
		w.finish()
		registry.detach(replacement, reader)
		next, _, nextReader, created, reason := registry.acquire(identity, 1)
		if attempt < traeCNResumeSilentRestartLimit {
			if !created || reason != "" || next == replacement {
				t.Fatalf("restart %d blocked: created=%t reason=%s", attempt+1, created, reason)
			}
			replacement, reader = next, nextReader
			continue
		}
		if created || reason != "resume_task_failed" {
			t.Fatalf("restart budget not enforced: created=%t reason=%s", created, reason)
		}
	}
}

// 客户端重试命中失败任务时，客户端看到的是新生成，而不是 409。
func TestTraeCNResumeReasoningOnlyCompletionRestartsSilently(t *testing.T) {
	t.Setenv("TRAECN_RESUME_ENABLED", "1")
	var calls atomic.Int32
	handler := newTraeCNContextTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		attempt := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if attempt == 1 {
			_, _ = io.WriteString(w, "event: output\ndata: {\"reasoning_content\":\"正在检查文件\"}\n\n")
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, "event: token_usage\ndata: {\"prompt_tokens\":3,\"completion_tokens\":4,\"total_tokens\":7}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
			return
		}
		_, _ = io.WriteString(w, "event: output\ndata: {\"content\":\"重开后正常回答\"}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
	})
	t.Cleanup(func() { cleanupTraeCNResumeRegistry(t, &handler.traeCNResumeTasks) })
	c, recorder := traeCNResumeTestContext(traeCNResumeTestBody, "reasoning-stop")
	handler.Responses(c)
	events := canonicalSSEEvents(t, recorder.Body.Bytes())
	failed, ok := findCanonicalEvent(events, "response.failed")
	if !ok || failed.Get("response.error.code").String() != "upstream_empty_output" {
		t.Fatalf("silent completion: %s", recorder.Body.String())
	}
	if _, ok := findCanonicalEvent(events, "response.completed"); ok {
		t.Fatal("empty output reported success")
	}
	if _, ok := findCanonicalEvent(events, "response.reasoning_summary_text.delta"); !ok {
		t.Fatal("reasoning was lost before failure")
	}
	if calls.Load() != 1 {
		t.Fatalf("first attempt dispatched %d upstream calls", calls.Load())
	}
	retry, retryRecorder := traeCNResumeTestContext(traeCNResumeTestBody, "reasoning-stop")
	handler.Responses(retry)
	if retryRecorder.Code != http.StatusOK {
		t.Fatalf("client retry not served: %d %s", retryRecorder.Code, retryRecorder.Body.String())
	}
	retryEvents := canonicalSSEEvents(t, retryRecorder.Body.Bytes())
	if _, ok := findCanonicalEvent(retryEvents, "response.completed"); !ok {
		t.Fatalf("retry did not complete a fresh generation: %s", retryRecorder.Body.String())
	}
	if _, ok := findCanonicalEvent(retryEvents, "response.failed"); ok {
		t.Fatalf("retry replayed the failed task: %s", retryRecorder.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("retry dispatched %d upstream calls", calls.Load())
	}
}

// 保留表满时请求回落到普通流程，而不是把网关自身容量变成客户端可见的 409。
func TestTraeCNResumeCapacityBypassesWithoutConflict(t *testing.T) {
	t.Setenv("TRAECN_RESUME_ENABLED", "1")
	var calls atomic.Int32
	handler := newTraeCNContextTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: output\ndata: {\"content\":\"容量满时仍按普通流程服务\"}\n\nevent: done\ndata: {}\n\n")
	})
	registry := &handler.traeCNResumeTasks
	registry.mu.Lock()
	if registry.tasks == nil {
		registry.tasks = make(map[string][]*traeCNResumeTask)
	}
	for index := 0; index < traeCNResumeMaxRequests; index++ {
		key := "saturated-" + string(rune('a'+index%26)) + string(rune('a'+index/26))
		registry.tasks[key] = []*traeCNResumeTask{{
			id: key, changed: make(chan struct{}), identity: traeCNResumeIdentity{key: key},
		}}
	}
	registry.count = traeCNResumeMaxRequests
	// 输入体预算同样占满，覆盖容量判断的第二个条件。
	registry.inputBytes = traeCNResumeInputBudget
	registry.mu.Unlock()
	t.Cleanup(func() { cleanupTraeCNResumeRegistry(t, registry) })
	c, recorder := traeCNResumeTestContext(traeCNResumeTestBody, "capacity")
	handler.Responses(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("capacity turned into a client error: %d %s", recorder.Code, recorder.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls=%d, want 1", calls.Load())
	}
	if _, ok := findCanonicalEvent(canonicalSSEEvents(t, recorder.Body.Bytes()), "response.completed"); !ok {
		t.Fatalf("normal path did not serve the request: %s", recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), traeCNResumeCapacityExceeded) {
		t.Fatal("capacity reason leaked to the client")
	}
}

func seedTraeCNResumeTask(t *testing.T, handler *Handler, requestID string, configure func(*traeCNResumeTask)) {
	t.Helper()
	c, _ := traeCNResumeTestContext(traeCNResumeTestBody, requestID)
	identity, ok := traeCNResumeRequestIdentity(c, []byte(traeCNResumeTestBody))
	if !ok {
		t.Fatal("missing resume identity for the seeded task")
	}
	task := &traeCNResumeTask{
		id: requestID, identity: identity, completed: map[string]string{},
		changed: make(chan struct{}), ttl: traeCNResumeTTL(), reader: 1, attached: true,
	}
	if configure != nil {
		configure(task)
	}
	registry := &handler.traeCNResumeTasks
	registry.mu.Lock()
	if registry.tasks == nil {
		registry.tasks = make(map[string][]*traeCNResumeTask)
	}
	registry.tasks[identity.key] = append(registry.tasks[identity.key], task)
	registry.count++
	registry.mu.Unlock()
	t.Cleanup(func() { cleanupTraeCNResumeRegistry(t, registry) })
}

// 续传层接不回上一条生成时，只放弃续传保护，不把 409 交给客户端。
func TestTraeCNResumeRefusalsDegradeToNormalFlow(t *testing.T) {
	t.Setenv("TRAECN_RESUME_ENABLED", "1")
	for _, tc := range []struct {
		name      string
		configure func(*traeCNResumeTask)
	}{
		{
			// 生成仍在运行但工具调用已交付：不接回也不重开。
			name: "live_task_with_delivered_tool",
			configure: func(task *traeCNResumeTask) {
				task.toolSubmitted = true
				task.done, task.attached = false, true
			},
		},
		{
			// 不是明确失败终态（内容过滤/长度截断）：无法证明上游没在继续生成。
			name: "ended_incomplete",
			configure: func(task *traeCNResumeTask) {
				task.terminalEvent = "response.incomplete"
				task.terminalCode = "max_output_tokens"
				task.done, task.attached = true, false
			},
		},
		{
			// 上游明确判定内容不合规：重开只会被再拒一次。
			name: "policy_refusal",
			configure: func(task *traeCNResumeTask) {
				task.terminalEvent = "response.failed"
				task.policyRefused = true
				task.done, task.attached = true, false
			},
		},
		{
			// 静默重开预算用尽。
			name: "restart_budget_exhausted",
			configure: func(task *traeCNResumeTask) {
				task.terminalEvent = "response.failed"
				task.restarts = traeCNResumeSilentRestartLimit
				task.done, task.attached = true, false
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			handler := newTraeCNContextTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: output\ndata: {\"content\":\"放弃续传后照常生成\"}\n\nevent: done\ndata: {}\n\n")
			})
			seedTraeCNResumeTask(t, handler, tc.name, tc.configure)
			request, recorder := traeCNResumeTestContext(traeCNResumeTestBody, tc.name)
			handler.Responses(request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("refusal leaked to the client: %d %s", recorder.Code, recorder.Body.String())
			}
			if calls.Load() != 1 {
				t.Fatalf("upstream calls=%d, want 1", calls.Load())
			}
			if _, ok := findCanonicalEvent(canonicalSSEEvents(t, recorder.Body.Bytes()), "response.completed"); !ok {
				t.Fatalf("normal path did not serve the request: %s", recorder.Body.String())
			}
			if got := recorder.Header().Get(traeCNResumeStatusHeader); got != traeCNResumeStatusBypass {
				t.Fatalf("resume status header=%q", got)
			}
		})
	}
}

// 已交付正文的失败同样重新调度，客户端不会看到半截内容被当成终态。
func TestTraeCNResumeEndedFailureAfterTextRestarts(t *testing.T) {
	t.Setenv("TRAECN_RESUME_ENABLED", "1")
	var calls atomic.Int32
	handler := newTraeCNContextTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: output\ndata: {\"content\":\"重新生成后的完整回答\"}\n\nevent: done\ndata: {}\n\n")
	})
	seedTraeCNResumeTask(t, handler, "text-then-failed", func(task *traeCNResumeTask) {
		task.terminalEvent = "response.failed"
		task.terminalCode = "upstream_stream_break"
		task.status = http.StatusOK
		task.done, task.attached = true, false
	})
	request, recorder := traeCNResumeTestContext(traeCNResumeTestBody, "text-then-failed")
	handler.Responses(request)
	if recorder.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("restart not served: calls=%d status=%d body=%s", calls.Load(), recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get(traeCNResumeStatusHeader); got != traeCNResumeStatusRestart {
		t.Fatalf("resume status header=%q", got)
	}
	if _, ok := findCanonicalEvent(canonicalSSEEvents(t, recorder.Body.Bytes()), "response.completed"); !ok {
		t.Fatalf("restart did not complete: %s", recorder.Body.String())
	}
}
