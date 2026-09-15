package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const traeCNResumeTestBody = `{"model":"DeepSeek-V4-Pro","stream":true,"input":[{"role":"user","content":"hello"}]}`

func traeCNResumeTestContext(body, requestID string) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	c.Request.Header.Set("Authorization", "Bearer shared-channel-key")
	c.Request.Header.Set("X-NewAPI-Request-ID", requestID)
	c.Set(contextAPIKeyID, int64(91081))
	c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 91081, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelTraeCN}})
	return c, recorder
}

func cleanupTraeCNResumeRegistry(t *testing.T, r *traeCNResumeRegistry) {
	t.Helper()
	r.mu.Lock()
	var tasks []*traeCNResumeTask
	for _, list := range r.tasks {
		tasks = append(tasks, list...)
	}
	r.mu.Unlock()
	for _, task := range tasks {
		task.mu.Lock()
		if task.timer != nil {
			task.timer.Stop()
		}
		if task.retireTimer != nil {
			task.retireTimer.Stop()
		}
		task.expired = true
		if task.cancel != nil {
			task.cancel()
		}
		task.log.close()
		task.notifyLocked()
		task.mu.Unlock()
	}
}

func TestTraeCNResumeIdentityIsolation(t *testing.T) {
	c, _ := traeCNResumeTestContext(traeCNResumeTestBody, "req-a")
	first, ok := traeCNResumeRequestIdentity(c, []byte(traeCNResumeTestBody))
	if !ok {
		t.Fatal("missing identity")
	}
	c.Request.Header.Set("X-NewAPI-User-ID", "forged-user")
	same, _ := traeCNResumeRequestIdentity(c, []byte(traeCNResumeTestBody))
	if first.key != same.key {
		t.Fatal("trusted an unsigned user header")
	}
	c.Request.Header.Set("X-NewAPI-Request-ID", "req-b")
	other, _ := traeCNResumeRequestIdentity(c, []byte(traeCNResumeTestBody))
	if first.key == other.key {
		t.Fatal("shared key mixed different NewAPI requests")
	}
	c.Request.Header.Set(codexTurnMetadataHeader, `{"thread_id":"thread-a","turn_id":"turn-a"}`)
	verified := verifiedNewAPIIdentityContext{APIKeyID: 91081, Platform: "newapi", Identity: newAPIIdentity{UserID: "user-a"}}
	c.Set(newAPIIdentityContextKey, verified)
	first, ok = traeCNResumeRequestIdentity(c, []byte(traeCNResumeTestBody))
	if !ok {
		t.Fatal("missing verified identity")
	}
	c.Request.Header.Set("X-NewAPI-Request-ID", "req-c")
	same, _ = traeCNResumeRequestIdentity(c, []byte(traeCNResumeTestBody))
	if first.key != same.key {
		t.Fatal("verified user could not resume with a fresh request ID")
	}
	verified.Identity.UserID = "user-b"
	c.Set(newAPIIdentityContextKey, verified)
	other, _ = traeCNResumeRequestIdentity(c, []byte(traeCNResumeTestBody))
	if first.key == other.key {
		t.Fatal("verified users shared a task")
	}
	c.Set(newAPIIdentityContextKey, nil)
	c.Request.Header.Del("X-NewAPI-Request-ID")
	if _, ok = traeCNResumeRequestIdentity(c, []byte(traeCNResumeTestBody)); ok {
		t.Fatal("missing identity was accepted")
	}
}

func writeTraeCNResumeTestEvent(t *testing.T, w *traeCNResumeWriter, event string) {
	t.Helper()
	frame := "data: " + event + "\n\n"
	if n, err := io.WriteString(w, frame); err != nil || n != len(frame) {
		t.Fatalf("write=%d, %v", n, err)
	}
}

func TestTraeCNResumeReplayAcknowledgedItemsAndToolBoundary(t *testing.T) {
	var registry traeCNResumeRegistry
	t.Cleanup(func() { cleanupTraeCNResumeRegistry(t, &registry) })
	c, _ := traeCNResumeTestContext(traeCNResumeTestBody, "req-a")
	identity, _ := traeCNResumeRequestIdentity(c, []byte(traeCNResumeTestBody))
	task, _, _, _, _ := registry.acquire(identity, len(traeCNResumeTestBody))
	w := newTraeCNResumeWriter(task)
	w.Header().Set("Content-Type", "text/event-stream")
	writeTraeCNResumeTestEvent(t, w, `{"type":"response.created","response":{"id":"resp-a"}}`)
	writeTraeCNResumeTestEvent(t, w, `{"type":"response.output_text.delta","item_id":"msg-a","delta":"confirmed"}`)
	item := `{"id":"msg-a","type":"message","status":"completed","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"confirmed","annotations":[]}]}`
	writeTraeCNResumeTestEvent(t, w, `{"type":"response.output_item.done","item":`+item+`}`)
	reasoning := `{"id":"rs-a","type":"reasoning","summary":[{"type":"summary_text","text":"checked"}]}`
	writeTraeCNResumeTestEvent(t, w, `{"type":"response.reasoning_summary_text.delta","item_id":"rs-a","delta":"checked"}`)
	writeTraeCNResumeTestEvent(t, w, `{"type":"response.output_item.done","item":`+reasoning+`}`)
	writeTraeCNResumeTestEvent(t, w, `{"type":"response.output_text.delta","item_id":"msg-b","delta":"remaining"}`)
	writeTraeCNResumeTestEvent(t, w, `{"type":"response.completed","response":{"id":"resp-a","status":"completed","output":[]}}`)
	w.finish()
	ack := `{"id":"msg-a","type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"confirmed"}]}`
	retryBody := strings.TrimSuffix(traeCNResumeTestBody, "]}") + "," + ack + "," + reasoning + "]}"
	retryIdentity, _ := traeCNResumeRequestIdentity(c, []byte(retryBody))
	reused, skip, reader, created, reason := registry.acquire(retryIdentity, len(retryBody))
	if reused != task || created || reason != "" || !skip["msg-a"] || !skip["rs-a"] {
		t.Fatalf("failed to match acknowledged output: %v, %s", skip, reason)
	}
	retryContext, recorder := traeCNResumeTestContext(retryBody, "req-a")
	serveTraeCNResumeTask(retryContext, task, skip, reader)
	for _, event := range canonicalSSEEvents(t, recorder.Body.Bytes()) {
		if skip[event.Get("item_id").String()] || skip[event.Get("item.id").String()] {
			t.Fatal("replayed acknowledged message")
		}
	}
	if !strings.Contains(recorder.Body.String(), "remaining") {
		t.Fatal("lost unacknowledged message")
	}
	tampered, _ := traeCNResumeRequestIdentity(c, []byte(strings.Replace(retryBody, "confirmed", "tampered", 1)))
	if _, matches := task.match(tampered); matches {
		t.Fatal("accepted mismatched acknowledged text")
	}
	writeTraeCNResumeTestEvent(t, w, `{"type":"response.output_item.done","item":{"id":"fc-a","type":"function_call","call_id":"call-a","name":"shell","arguments":"{}"}}`)
	if _, _, _, _, reason = registry.acquire(identity, 1); reason != "resume_tool_call_unsupported" {
		t.Fatalf("tool replay allowed: %s", reason)
	}
}

func TestTraeCNResumeRegistryFencesReadersAndExpiry(t *testing.T) {
	var registry traeCNResumeRegistry
	t.Cleanup(func() { cleanupTraeCNResumeRegistry(t, &registry) })
	identity := traeCNResumeIdentity{key: "owner", root: "root", input: []string{"input"}}
	task, _, first, _, _ := registry.acquire(identity, 10)
	ctx, cancel := context.WithCancel(context.Background())
	task.cancel = cancel
	_, _, second, created, _ := registry.acquire(identity, 10)
	if created || second <= first || registry.count != 1 {
		t.Fatal("duplicate worker")
	}
	registry.detach(task, first)
	if !task.attached {
		t.Fatal("old reader detached new reader")
	}
	registry.detach(task, second)
	registry.expire(task, first)
	if task.expired {
		t.Fatal("stale expiry cancelled a newer attachment")
	}
	registry.expire(task, second)
	if !task.expired || ctx.Err() == nil {
		t.Fatal("expiry did not cancel worker")
	}
	if _, _, _, _, reason := registry.acquire(identity, 10); reason != "resume_task_expired" {
		t.Fatalf("expired task restarted: %s", reason)
	}
}

func TestTraeCNResumeLogSpillsAndReleasesBudget(t *testing.T) {
	before := traeCNResumeStoredBytes.Load()
	var journal traeCNResumeLog
	t.Cleanup(journal.close)
	data := []byte(strings.Repeat("x", traeCNResumeMemoryBytes+1))
	if err := journal.append(data, "item"); err != nil {
		t.Fatal(err)
	}
	if journal.file == nil || journal.memory != nil {
		t.Fatal("large replay retained its memory buffer")
	}
	path := journal.file.Name()
	got, err := journal.read(0)
	if err != nil || string(got) != string(data) {
		t.Fatal("spilled replay corrupted")
	}
	journal.close()
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("temporary file not removed: %v", err)
	}
	if traeCNResumeStoredBytes.Load() != before {
		t.Fatal("storage reservation leaked")
	}
	journal.size = traeCNResumeTaskBytes
	if err = journal.append([]byte("x"), ""); err == nil {
		t.Fatal("task exceeded cache limit")
	}
}

// 使用真实 HTTP 连接断开验证：原上游持续运行，重试不占第二个账号并发位。
func TestTraeCNResumeHTTPDisconnectKeepsOriginalGeneration(t *testing.T) {
	t.Setenv("TRAECN_RESUME_ENABLED", "1")
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := previous
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
	ApplyRuntimeSettings(settings)
	var calls atomic.Int32
	continueOutput := make(chan struct{})
	var continueOnce sync.Once
	upstreamGone := make(chan struct{}, 1)
	handler := newTraeCNContextTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "id: 1\nevent: output\ndata: {\"type\":\"text\",\"content\":\"before\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-continueOutput:
		case <-r.Context().Done():
			upstreamGone <- struct{}{}
			return
		}
		_, _ = io.WriteString(w, "id: 2\nevent: output\ndata: {\"type\":\"text\",\"content\":\"after\"}\n\nid: 3\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
	})
	t.Cleanup(func() {
		continueOnce.Do(func() { close(continueOutput) })
		cleanupTraeCNResumeRegistry(t, &handler.traeCNResumeTasks)
	})
	router := gin.New()
	router.POST("/v1/responses", func(c *gin.Context) {
		c.Set(contextAPIKeyID, int64(91081))
		c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 91081, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelTraeCN}})
		handler.Responses(c)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	request := func(ctx context.Context) *http.Response {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(traeCNResumeTestBody))
		req.Header.Set("Authorization", "Bearer shared-channel-key")
		req.Header.Set("X-NewAPI-Request-ID", "req-a")
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first := request(ctx)
	reader := bufio.NewReader(first.Body)
	responseID := ""
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("stream was buffered until completion: %v", err)
		}
		if strings.HasPrefix(line, "data: ") {
			event := gjson.Parse(strings.TrimSpace(line[6:]))
			if event.Get("type").String() == "response.created" {
				responseID = event.Get("response.id").String()
			}
			if event.Get("type").String() == "response.output_text.delta" {
				break
			}
		}
	}
	_ = first.Body.Close()
	cancel()
	select {
	case <-upstreamGone:
		t.Fatal("downstream disconnect cancelled upstream")
	case <-time.After(50 * time.Millisecond):
	}
	secondCtx, secondCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer secondCancel()
	second := request(secondCtx)
	defer second.Body.Close()
	continueOnce.Do(func() { close(continueOutput) })
	payload, err := io.ReadAll(second.Body)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("started %d upstream generations", calls.Load())
	}
	events := canonicalSSEEvents(t, payload)
	completed, ok := findCanonicalEvent(events, "response.completed")
	if !ok {
		t.Fatalf("missing completed response: %s", payload)
	}
	if responseID == "" || completed.Get("response.id").String() != responseID {
		t.Fatal("response identity changed across reconnect")
	}
	var text string
	for _, event := range events {
		if event.Get("type").String() == "response.output_text.delta" {
			text += event.Get("delta").String()
		}
	}
	if text != "beforeafter" {
		t.Fatalf("replayed text=%q", text)
	}
}

func TestTraeCNResumeWriterRejectsIncompleteTerminal(t *testing.T) {
	task := &traeCNResumeTask{changed: make(chan struct{}), completed: make(map[string]string)}
	t.Cleanup(task.log.close)
	w := newTraeCNResumeWriter(task)
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprint(w, "data: {\"type\":\"response.created\"")
	if len(task.log.records) != 0 {
		t.Fatal("published partial event")
	}
	w.finish()
	if task.failure != "resume_stream_incomplete" {
		t.Fatal("missing incomplete stream error")
	}
	// 大整数不能在规范化时因 float64 舍入而被误认为同一输入。
	if traeCNResumeItemDigest(json.RawMessage(`{"value":9007199254740992}`)) == traeCNResumeItemDigest(json.RawMessage(`{"value":9007199254740993}`)) {
		t.Fatal("rounded distinct input values")
	}
}

func TestTraeCNResumeDisconnectTTLReleasesWorker(t *testing.T) {
	t.Setenv("TRAECN_RESUME_ENABLED", "1")
	t.Setenv("TRAECN_RESUME_TTL_SECONDS", "1")
	upstreamGone := make(chan struct{})
	handler := newTraeCNContextTestHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: output\ndata: {\"content\":\"waiting\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamGone)
	})
	t.Cleanup(func() { cleanupTraeCNResumeRegistry(t, &handler.traeCNResumeTasks) })
	c, _ := traeCNResumeTestContext(traeCNResumeTestBody, "ttl-request")
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	finished := make(chan struct{})
	go func() { handler.Responses(c); close(finished) }()
	deadline := time.After(5 * time.Second)
	var task *traeCNResumeTask
	for task == nil {
		handler.traeCNResumeTasks.mu.Lock()
		for _, list := range handler.traeCNResumeTasks.tasks {
			if len(list) > 0 {
				task = list[0]
			}
		}
		handler.traeCNResumeTasks.mu.Unlock()
		if task == nil {
			select {
			case <-deadline:
				t.Fatal("task not registered")
			case <-time.After(time.Millisecond):
			}
		}
	}
	for {
		task.mu.Lock()
		ready, changed := len(task.log.records) > 0, task.changed
		task.mu.Unlock()
		if ready {
			break
		}
		select {
		case <-deadline:
			t.Fatal("upstream did not stream")
		case <-changed:
		}
	}
	cancel()
	select {
	case <-finished:
	case <-deadline:
		t.Fatal("subscriber did not detach")
	}
	select {
	case <-upstreamGone:
	case <-time.After(8 * time.Second):
		t.Fatal("expired task kept upstream alive")
	}
	for {
		task.mu.Lock()
		done, changed, expired := task.done, task.changed, task.expired
		task.mu.Unlock()
		if done {
			if !expired {
				t.Fatal("worker ended before expiry")
			}
			break
		}
		select {
		case <-changed:
		case <-time.After(time.Second):
			t.Fatal("worker did not finish accounting")
		}
	}
	handler.traeCNResumeTasks.mu.Lock()
	inputBytes := handler.traeCNResumeTasks.inputBytes
	handler.traeCNResumeTasks.mu.Unlock()
	if inputBytes != 0 {
		t.Fatalf("retained %d input bytes after worker exit", inputBytes)
	}
	account := handler.store.Accounts()[0]
	account.Mu().RLock()
	failures := account.FailureStreak
	account.Mu().RUnlock()
	if failures != 0 {
		t.Fatalf("local expiry penalized upstream account: streak=%d", failures)
	}
	if _, _, _, _, reason := handler.traeCNResumeTasks.acquire(task.identity, len(traeCNResumeTestBody)); reason != "resume_task_expired" {
		t.Fatalf("late retry restarted task: %s", reason)
	}
}

func TestTraeCNResumeCapacityAndUpstreamOptIn(t *testing.T) {
	var registry traeCNResumeRegistry
	registry.count = traeCNResumeMaxRequests
	if _, _, _, _, reason := registry.acquire(traeCNResumeIdentity{key: "owner"}, 1); reason != "resume_capacity_exceeded" {
		t.Fatal("task limit not enforced")
	}
	registry.count = 0
	registry.inputBytes = traeCNResumeInputBudget
	if _, _, _, _, reason := registry.acquire(traeCNResumeIdentity{key: "owner"}, 1); reason != "resume_capacity_exceeded" {
		t.Fatal("input budget not enforced")
	}
	t.Setenv("TRAECN_RESUME_UPSTREAM_ENABLED", "")
	if traeCNResumeUpstreamEnabled() {
		t.Fatal("unverified upstream resumption enabled by default")
	}
	t.Setenv("TRAECN_RESUME_UPSTREAM_ENABLED", "1")
	if !traeCNResumeUpstreamEnabled() {
		t.Fatal("upstream opt-in ignored")
	}
}
