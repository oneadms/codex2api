package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const traeCNResumeWorkerKey = "traecn_resume_worker"
const traeCNResumeMaxRequests = 64
const traeCNResumeInputBudget = 64 << 20
const traeCNResumeRequestBytes = 16 << 20

// 容量耗尽只说明网关自己的保留表满了，不代表这次请求不能服务。
const traeCNResumeCapacityExceeded = "resume_capacity_exceeded"

func traeCNResumeEnabled() bool { return strings.TrimSpace(os.Getenv("TRAECN_RESUME_ENABLED")) == "1" }

// 上游游标能力需要单独开启，未验证的接口不能通过重发 POST 猜测续传。
func traeCNResumeUpstreamEnabled() bool {
	return strings.TrimSpace(os.Getenv("TRAECN_RESUME_UPSTREAM_ENABLED")) == "1"
}
func isTraeCNResumeWorker(c *gin.Context) bool {
	value, _ := c.Get(traeCNResumeWorkerKey)
	return value == true
}

func responsesRelayUpstreamContext(c *gin.Context) (context.Context, context.CancelFunc) {
	if isTraeCNResumeWorker(c) {
		// 后台任务已提供断线保留期；任务自身取消时无需再增加用量读取宽限期。
		return context.WithCancel(c.Request.Context())
	}
	return newDrainableUpstreamContext(c.Request.Context(), upstreamDrainTimeout)
}
func traeCNResumeTTL() time.Duration {
	seconds, err := strconv.Atoi(os.Getenv("TRAECN_RESUME_TTL_SECONDS"))
	if err != nil || seconds < 1 || seconds > 600 {
		seconds = 60
	}
	return time.Duration(seconds) * time.Second
}

type traeCNResumeRegistry struct {
	mu                sync.Mutex
	tasks             map[string][]*traeCNResumeTask
	count, inputBytes int
}

type traeCNResumeTask struct {
	mu                sync.Mutex
	id                string
	identity          traeCNResumeIdentity
	log               traeCNResumeLog
	completed         map[string]string
	toolSubmitted     bool
	done, expired     bool
	failure           string
	terminalEvent     string
	terminalCode      string
	responseID        string
	retrySafe         bool
	generationStarted bool
	policyRefused     bool
	restarts          int
	header            http.Header
	status            int
	changed           chan struct{}
	cancel            context.CancelFunc
	reader            uint64
	attached          bool
	timer             *time.Timer
	retireTimer       *time.Timer
	ttl               time.Duration
}

func (t *traeCNResumeTask) notifyLocked() { close(t.changed); t.changed = make(chan struct{}) }

// match 只接受原输入以及本任务已完成输出的回传；新增用户消息和工具结果属于新调用。
func (t *traeCNResumeTask) match(identity traeCNResumeIdentity) (map[string]bool, bool) {
	if identity.root != t.identity.root || len(identity.input) < len(t.identity.input) {
		return nil, false
	}
	for i, digest := range t.identity.input {
		if identity.input[i] != digest {
			return nil, false
		}
	}
	skip := map[string]bool{}
	for _, digest := range identity.input[len(t.identity.input):] {
		id, ok := t.completed[digest]
		if !ok {
			return nil, false
		}
		skip[id] = true
	}
	return skip, true
}

func (r *traeCNResumeRegistry) acquire(identity traeCNResumeIdentity, inputBytes int) (*traeCNResumeTask, map[string]bool, uint64, bool, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tasks == nil {
		r.tasks = make(map[string][]*traeCNResumeTask)
	}
	restarts := 0
	for index, task := range r.tasks[identity.key] {
		task.mu.Lock()
		skip, matches := task.match(identity)
		if !matches {
			task.mu.Unlock()
			continue
		}
		if task.canRetryLocked() && task.done && !task.attached {
			// 原订阅与后台均已收尾，原子替换失败记录，避免并发重试创建多个任务。
			if inputBytes > traeCNResumeRequestBytes || r.inputBytes+inputBytes > traeCNResumeInputBudget {
				task.mu.Unlock()
				return nil, nil, 0, false, traeCNResumeCapacityExceeded
			}
			if task.timer != nil {
				task.timer.Stop()
			}
			if task.retireTimer != nil {
				task.retireTimer.Stop()
			}
			task.expired = true
			task.log.close()
			list := r.tasks[identity.key]
			r.tasks[identity.key] = append(list[:index], list[index+1:]...)
			r.count--
			restarts = task.restarts + 1
			log.Printf("[TRAE-RESUME] task=%s stage=retry reason=pre_generation_failure terminal_event=%q silent_restarts=%d", task.id, task.terminalEvent, restarts)
			task.mu.Unlock()
			break
		}
		if task.expired {
			task.mu.Unlock()
			return nil, nil, 0, false, "resume_task_expired"
		}
		if task.toolSubmitted {
			task.mu.Unlock()
			return nil, nil, 0, false, "resume_tool_call_unsupported"
		}
		// 失败终态已结束原生成。重复回放它只会耗尽客户端重试次数，不能恢复进度。
		reason := task.failure
		if reason == "" {
			switch {
			case task.terminalEvent == "response.failed" || task.status >= http.StatusBadRequest:
				reason = "resume_task_failed"
			case task.terminalEvent == "response.incomplete":
				reason = "resume_task_incomplete"
			}
		}
		if reason != "" && !task.canRetryLocked() {
			// 续传层拒绝接回；调用方随后放弃续传，按普通流程继续服务这次请求。
			log.Printf("[TRAE-RESUME] task=%s stage=refuse reason=%q terminal_event=%q terminal_code=%q", task.id, reason, task.terminalEvent, task.terminalCode)
			task.mu.Unlock()
			return nil, nil, 0, false, reason
		}
		if task.timer != nil {
			task.timer.Stop()
			task.timer = nil
		}
		task.reader++
		task.attached = true
		generation := task.reader
		task.notifyLocked()
		task.mu.Unlock()
		return task, skip, generation, false, ""
	}
	if inputBytes > traeCNResumeRequestBytes || r.inputBytes+inputBytes > traeCNResumeInputBudget || r.count >= traeCNResumeMaxRequests {
		return nil, nil, 0, false, traeCNResumeCapacityExceeded
	}
	task := &traeCNResumeTask{id: uuid.NewString(), identity: identity, completed: make(map[string]string), changed: make(chan struct{}), ttl: traeCNResumeTTL(), reader: 1, attached: true, restarts: restarts}
	r.tasks[identity.key] = append(r.tasks[identity.key], task)
	r.count++
	r.inputBytes += inputBytes
	return task, map[string]bool{}, 1, true, ""
}

func (r *traeCNResumeRegistry) forget(task *traeCNResumeTask) {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.tasks[task.identity.key]
	for i, candidate := range list {
		if candidate == task {
			list = append(list[:i], list[i+1:]...)
			r.count--
			break
		}
	}
	if len(list) == 0 {
		delete(r.tasks, task.identity.key)
	} else {
		r.tasks[task.identity.key] = list
	}
}

func (r *traeCNResumeRegistry) expire(task *traeCNResumeTask, reader uint64) {
	task.mu.Lock()
	if task.reader != reader || task.attached || task.expired {
		task.mu.Unlock()
		return
	}
	task.expired = true
	task.failure = "resume_task_expired"
	if task.cancel != nil {
		task.cancel()
	}
	task.log.close()
	task.notifyLocked()
	r.retireLocked(task)
	task.mu.Unlock()
	log.Printf("[TRAE-RESUME] task=%s stage=expired", task.id)
}

// 必须等原任务收尾后再计算失败标记的保留期，防止旧上游仍存活时创建重复任务。
func (r *traeCNResumeRegistry) retireLocked(task *traeCNResumeTask) {
	if task.expired && task.done && task.retireTimer == nil {
		task.retireTimer = time.AfterFunc(task.ttl, func() { r.forget(task) })
	}
}

func (r *traeCNResumeRegistry) detach(task *traeCNResumeTask, reader uint64) {
	task.mu.Lock()
	defer task.mu.Unlock()
	if task.reader != reader || !task.attached {
		return
	}
	task.attached = false
	if task.timer != nil {
		task.timer.Stop()
	}
	task.timer = time.AfterFunc(task.ttl, func() { r.expire(task, reader) })
	log.Printf("[TRAE-RESUME] task=%s stage=detach reader=%d retention_seconds=%d", task.id, reader, int(task.ttl/time.Second))
}

func (h *Handler) serveTraeCNResumableResponses(c *gin.Context, validated responsesValidatedRequest) bool {
	if !traeCNResumeEnabled() || requestUpstreamChannel(c) != database.UpstreamChannelTraeCN || !gjson.GetBytes(validated.body, "stream").Bool() || validated.bodySignalCompact {
		return false
	}
	if len(validated.body) > traeCNResumeRequestBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": gin.H{"code": "resume_request_too_large", "message": "TRAE 续传请求体上限为 16 MiB"}})
		return true
	}
	identity, eligible := traeCNResumeRequestIdentity(c, validated.body)
	if !eligible {
		log.Printf("[TRAE-RESUME] stage=unavailable reason=missing_stable_identity gateway_request_id=%q", ensurePromptPolicyRequestCorrelationID(c))
		return false
	}
	task, skip, reader, created, reason := h.traeCNResumeTasks.acquire(identity, len(validated.body))
	if reason != "" {
		// 续传层只决定"能不能接回上一条生成"，不决定"这一次请求能不能服务"。
		// 接不回时放弃续传保护、按普通流程照常生成，不把 409 丢给客户端。
		h.traeCNResumeTasks.mu.Lock()
		count, inputBytes := h.traeCNResumeTasks.count, h.traeCNResumeTasks.inputBytes
		h.traeCNResumeTasks.mu.Unlock()
		c.Header(traeCNResumeStatusHeader, traeCNResumeStatusBypass)
		log.Printf("[TRAE-RESUME] stage=bypass reason=%s tasks=%d input_bytes=%d gateway_request_id=%q", reason, count, inputBytes, ensurePromptPolicyRequestCorrelationID(c))
		return false
	}
	status := traeCNResumeStatusFresh
	if created && traeCNResumeTaskRestarts(task) > 0 {
		status = traeCNResumeStatusRestart
	} else if !created {
		status = traeCNResumeStatusAttached
	}
	c.Header(traeCNResumeStatusHeader, status)
	if created {
		worker := c.Copy()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 20*time.Minute)
		ctx = context.WithValue(ctx, traeCNResumeUpstreamContextKey{}, traeCNResumeUpstreamEnabled())
		worker.Request = c.Request.Clone(ctx)
		writer := newTraeCNResumeWriter(task)
		worker.Writer = writer
		worker.Set(traeCNResumeWorkerKey, true)
		worker.Set(traeCNResumeTaskKey, task)
		task.mu.Lock()
		task.cancel = cancel
		if task.expired {
			cancel()
		}
		task.mu.Unlock()
		go func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					task.mu.Lock()
					task.failure = "resume_worker_failed"
					task.mu.Unlock()
					log.Printf("[TRAE-RESUME] task=%s worker panic: %v", task.id, recovered)
				}
				cancel()
				h.traeCNResumeTasks.mu.Lock()
				h.traeCNResumeTasks.inputBytes -= len(validated.body)
				h.traeCNResumeTasks.mu.Unlock()
				writer.finish()
				task.mu.Lock()
				h.traeCNResumeTasks.retireLocked(task)
				log.Printf("[TRAE-RESUME] task=%s stage=worker_done response_id=%q status=%d terminal_event=%q terminal_code=%q failure=%q", task.id, task.responseID, task.status, task.terminalEvent, task.terminalCode, task.failure)
				task.mu.Unlock()
			}()
			h.responsesValidated(worker, validated)
		}()
	}
	log.Printf("[TRAE-RESUME] task=%s stage=attach created=%t reader=%d skipped_items=%d newapi_request_id=%q", task.id, created, reader, len(skip), responsesIdentityLogValue(c.GetHeader("X-NewAPI-Request-ID")))
	defer h.traeCNResumeTasks.detach(task, reader)
	serveTraeCNResumeTask(c, task, skip, reader)
	return true
}

func serveTraeCNResumeTask(c *gin.Context, task *traeCNResumeTask, skip map[string]bool, reader uint64) {
	index := 0
	committed := false
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	controller := http.NewResponseController(c.Writer)
	defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	for {
		if c.Request.Context().Err() != nil {
			return
		}
		task.mu.Lock()
		if task.reader != reader {
			task.mu.Unlock()
			return
		}
		if !committed && task.status != 0 {
			for key, values := range task.header {
				c.Writer.Header()[key] = append([]string(nil), values...)
			}
			c.Writer.Header().Del("Content-Length")
			c.Status(task.status)
			committed = true
		}
		for index < len(task.log.records) && skip[task.log.records[index].itemID] {
			index++
		}
		if index < len(task.log.records) {
			data, err := task.log.read(index)
			index++
			task.mu.Unlock()
			if err != nil {
				task.mu.Lock()
				task.failure = "resume_storage_read_failed"
				task.log.close()
				if task.cancel != nil {
					task.cancel()
				}
				task.notifyLocked()
				task.mu.Unlock()
				continue
			}
			_ = controller.SetWriteDeadline(time.Now().Add(15 * time.Second))
			if _, err = c.Writer.Write(data); err != nil {
				return
			}
			c.Writer.Flush()
			continue
		}
		done, failure, changed, responseID := task.done || task.expired, task.failure, task.changed, task.responseID
		task.mu.Unlock()
		if done {
			if failure != "" {
				if !committed {
					c.JSON(http.StatusConflict, gin.H{"error": gin.H{"code": failure, "message": "TRAE 续传失败"}})
				} else {
					payload, _ := json.Marshal(gin.H{"type": "response.failed", "response": gin.H{"id": responseID, "object": "response", "status": "failed", "error": gin.H{"code": failure, "message": "TRAE 续传失败"}}})
					_ = controller.SetWriteDeadline(time.Now().Add(15 * time.Second))
					_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", payload)
					c.Writer.Flush()
				}
			}
			return
		}
		select {
		case <-c.Request.Context().Done():
			return
		case <-changed:
		case <-heartbeat.C:
			if committed && strings.Contains(c.Writer.Header().Get("Content-Type"), "text/event-stream") {
				_ = http.NewResponseController(c.Writer).SetWriteDeadline(time.Now().Add(15 * time.Second))
				if _, err := c.Writer.Write([]byte(": keepalive\n\n")); err != nil {
					return
				}
				c.Writer.Flush()
			}
		}
	}
}
