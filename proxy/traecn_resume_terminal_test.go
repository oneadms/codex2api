package proxy

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tidwall/gjson"
)

// 上游已失败的任务不能靠重复回放恢复；拒绝重连时也不能抢占原订阅连接。
func TestTraeCNResumeRejectsEndedFailures(t *testing.T) {
	for _, tc := range []struct {
		name, event, localFailure, reason, terminalCode string
	}{
		{"failed", `{"type":"response.failed","response":{"id":"resp-a","status":"failed","error":{"code":"upstream_empty_output"}}}`, "", "resume_task_failed", "upstream_empty_output"},
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

func TestTraeCNResumeReasoningOnlyCompletionReportsFailure(t *testing.T) {
	t.Setenv("TRAECN_RESUME_ENABLED", "1")
	var calls atomic.Int32
	handler := newTraeCNContextTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: output\ndata: {\"reasoning_content\":\"正在检查文件\"}\n\n")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "event: token_usage\ndata: {\"prompt_tokens\":3,\"completion_tokens\":4,\"total_tokens\":7}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
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
	retry, retryRecorder := traeCNResumeTestContext(traeCNResumeTestBody, "reasoning-stop")
	handler.Responses(retry)
	if retryRecorder.Code != http.StatusConflict || gjson.GetBytes(retryRecorder.Body.Bytes(), "error.code").String() != "resume_task_failed" {
		t.Fatalf("ended task was replayed: %d %s", retryRecorder.Code, retryRecorder.Body.String())
	}
	if calls.Load() != 1 || strings.Contains(retryRecorder.Body.String(), "response.created") {
		t.Fatalf("retry started another generation: %d", calls.Load())
	}
}
