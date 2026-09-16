package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const traeCNResumeFrameBytes = 8 << 20

// 后台写入器只发布完整 SSE 事件，客户端的写入速度不影响上游读取。
type traeCNResumeWriter struct {
	task         *traeCNResumeTask
	header       http.Header
	status, size int
	pending      []byte
	terminal     bool
	sequence     int64
}

var _ gin.ResponseWriter = (*traeCNResumeWriter)(nil)

func newTraeCNResumeWriter(task *traeCNResumeTask) *traeCNResumeWriter {
	return &traeCNResumeWriter{task: task, header: make(http.Header), status: http.StatusOK, size: -1}
}

func (w *traeCNResumeWriter) Header() http.Header { return w.header }
func (w *traeCNResumeWriter) Status() int         { return w.status }
func (w *traeCNResumeWriter) Size() int           { return w.size }
func (w *traeCNResumeWriter) Written() bool       { return w.size >= 0 }
func (w *traeCNResumeWriter) WriteHeader(code int) {
	if !w.Written() && code > 0 {
		w.status = code
	}
}
func (w *traeCNResumeWriter) commitLocked() {
	if w.Written() {
		return
	}
	w.size = 0
	w.task.status, w.task.header = w.status, w.header.Clone()
	w.task.notifyLocked()
}
func (w *traeCNResumeWriter) WriteHeaderNow() {
	w.task.mu.Lock()
	defer w.task.mu.Unlock()
	w.commitLocked()
}
func (w *traeCNResumeWriter) Flush()                                { w.WriteHeaderNow() }
func (w *traeCNResumeWriter) WriteString(value string) (int, error) { return w.Write([]byte(value)) }
func (w *traeCNResumeWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("TRAE resume writer does not support hijacking")
}
func (w *traeCNResumeWriter) CloseNotify() <-chan bool { return nil }
func (w *traeCNResumeWriter) Pusher() http.Pusher      { return nil }

func (w *traeCNResumeWriter) failLocked(code string) {
	if w.task.failure == "" {
		w.task.failure = code
	}
	if w.task.cancel != nil {
		w.task.cancel()
	}
	w.task.notifyLocked()
}

func (w *traeCNResumeWriter) Write(data []byte) (int, error) {
	w.task.mu.Lock()
	defer w.task.mu.Unlock()
	if w.task.expired || w.task.failure != "" {
		return 0, io.ErrClosedPipe
	}
	written := len(data)
	w.commitLocked()
	if !strings.Contains(w.header.Get("Content-Type"), "text/event-stream") {
		if err := w.task.log.append(data, ""); err != nil {
			w.failLocked("resume_storage_limit")
			return 0, err
		}
	} else {
		// 核心 Responses 转换器使用 LF；允许一次 Write 携带多个事件。
		for len(data) > 0 {
			limit := traeCNResumeFrameBytes - len(w.pending)
			if limit <= 0 {
				w.failLocked("resume_event_too_large")
				return 0, errTraeCNResumeStorage
			}
			n := min(len(data), limit)
			w.pending = append(w.pending, data[:n]...)
			data = data[n:]
			for {
				end := bytes.Index(w.pending, []byte("\n\n"))
				if end < 0 {
					break
				}
				if err := w.appendFrameLocked(w.pending[:end+2]); err != nil {
					w.failLocked("resume_storage_limit")
					return 0, err
				}
				w.pending = w.pending[end+2:]
			}
			if len(w.pending) == 0 {
				w.pending = nil
			}
		}
	}
	w.size += written
	w.task.notifyLocked()
	return written, nil
}

func (w *traeCNResumeWriter) appendFrameLocked(frame []byte) error {
	var data []byte
	for _, line := range bytes.Split(frame, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("data:")) {
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, bytes.TrimSpace(line[5:])...)
		}
	}
	if len(data) == 0 {
		return nil
	} // 保活由每个订阅连接自行发送。
	if bytes.Equal(data, []byte("[DONE]")) {
		return w.task.log.append(frame, "")
	}
	var event map[string]json.RawMessage
	if json.Unmarshal(data, &event) != nil {
		return errors.New("invalid Responses event")
	}
	root := gjson.ParseBytes(data)
	typeName := root.Get("type").String()
	if typeName != "response.failed" {
		// 一旦发布生成事件，后续失败不能再按未生成请求重新派发。
		w.task.generationStarted = true
		w.task.retrySafe = false
	}
	itemID := strings.Clone(firstNonEmptyString(root.Get("item_id").String(), root.Get("item.id").String()))
	event["sequence_number"], _ = json.Marshal(w.sequence)
	w.sequence++
	encoded, err := json.Marshal(event)
	if err != nil {
		return err
	}
	encoded = append(append([]byte("data: "), encoded...), '\n', '\n')
	if err = w.task.log.append(encoded, itemID); err != nil {
		return err
	}
	if typeName == "response.created" {
		w.task.responseID = strings.Clone(root.Get("response.id").String())
	}
	if typeName == "response.output_item.done" {
		item := root.Get("item")
		switch item.Get("type").String() {
		case "message", "reasoning":
			if itemID != "" {
				w.task.completed[traeCNResumeItemDigest([]byte(item.Raw))] = itemID
			}
		default:
			// 工具已交付后无法判断客户端是否执行过，拒绝后续自动重放。
			w.task.toolSubmitted = true
		}
	}
	if typeName == "response.completed" || typeName == "response.failed" || typeName == "response.incomplete" {
		w.terminal = true
		w.task.terminalEvent = strings.Clone(typeName)
		w.task.terminalCode = strings.Clone(responsesIdentityLogValue(firstNonEmptyString(root.Get("response.error.code").String(), root.Get("response.incomplete_details.reason").String())))
	}
	return nil
}

func (w *traeCNResumeWriter) finish() {
	w.task.mu.Lock()
	defer w.task.mu.Unlock()
	if w.task.failure == "" && strings.Contains(w.header.Get("Content-Type"), "text/event-stream") && !w.terminal {
		w.task.failure = "resume_stream_incomplete"
	}
	w.pending = nil
	w.task.done = true
	w.task.notifyLocked()
}
