package diag

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const defaultLogLimit = 16 << 20

var activeCollector atomic.Pointer[Collector]

type Collector struct {
	mu       sync.Mutex
	queue    chan Event
	done     chan struct{}
	closed   bool
	path     string
	maxBytes int64
	revision string
	dropped  atomic.Uint64
	writeErr error // read after done has closed
}

func LogPath() string {
	if path := strings.TrimSpace(os.Getenv("CODEX_DIAG_LOG_PATH")); path != "" {
		return path
	}
	dir := strings.TrimSpace(os.Getenv("LOG_DIR"))
	if dir == "" {
		dir = "logs"
	}
	return filepath.Join(dir, "diagnostics.jsonl")
}

// StartFromEnv is called once at service startup. Collection is opt-in.
func StartFromEnv() (*Collector, error) {
	value := strings.TrimSpace(os.Getenv("CODEX_DIAG_ENABLED"))
	if value == "" {
		return nil, nil
	}
	enabled, err := strconv.ParseBool(value)
	if err != nil {
		return nil, fmt.Errorf("CODEX_DIAG_ENABLED: %w", err)
	}
	if !enabled {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_DISABLED"))) {
	case "1", "true", "yes", "y", "on":
		return nil, nil
	}
	revision := strings.TrimSpace(os.Getenv("CODEX_DIAG_REVISION"))
	if revision != "" && !shaPattern.MatchString(revision) {
		return nil, errors.New("CODEX_DIAG_REVISION must be a full lowercase commit SHA")
	}
	if revision == "" {
		if info, ok := debug.ReadBuildInfo(); ok {
			dirty := false
			for _, setting := range info.Settings {
				if setting.Key == "vcs.revision" {
					revision = setting.Value
				}
				if setting.Key == "vcs.modified" && setting.Value == "true" {
					dirty = true
				}
			}
			if dirty {
				revision = ""
			}
		}
	}
	c, err := NewCollector(LogPath(), defaultLogLimit, revision)
	if err != nil {
		return nil, err
	}
	if !activeCollector.CompareAndSwap(nil, c) {
		_ = c.Close()
		return nil, errors.New("diagnostic collector already running")
	}
	return c, nil
}

func Enabled() bool { return activeCollector.Load() != nil }

// StartManagedCollector is used by the admin-configured runtime, without env edits.
func StartManagedCollector(path string) (*Collector, error) {
	if flag := strings.ToLower(strings.TrimSpace(os.Getenv("LOG_DISABLED"))); flag == "true" || flag == "1" || flag == "yes" || flag == "y" || flag == "on" {
		return nil, errors.New("服务器设置了 LOG_DISABLED，无法开启诊断文件采集")
	}
	revision := strings.TrimSpace(os.Getenv("CODEX_DIAG_REVISION"))
	if revision != "" && !shaPattern.MatchString(revision) {
		return nil, errors.New("CODEX_DIAG_REVISION 必须是完整 commit SHA")
	}
	c, err := NewCollector(path, defaultLogLimit, revision)
	if err != nil {
		return nil, err
	}
	if !activeCollector.CompareAndSwap(nil, c) {
		_ = c.Close()
		return nil, errors.New("已有诊断采集器运行")
	}
	return c, nil
}

// Record does no disk or network I/O on the request goroutine.
func Record(e Event) {
	if c := activeCollector.Load(); c != nil {
		c.Record(e)
	}
}

func NewCollector(path string, maxBytes int64, revision string) (*Collector, error) {
	if maxBytes <= 0 {
		return nil, errors.New("diagnostic log limit must be positive")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	c := &Collector{queue: make(chan Event, 256), done: make(chan struct{}), path: path, maxBytes: maxBytes, revision: revision}
	go c.writeLoop(f, info.Size())
	return c, nil
}

func (c *Collector) Record(e Event) {
	if c == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	// Bound data before enqueueing; redaction and JSON encoding run on the writer.
	e.Message, e.Stack = bounded(e.Message, 8192), bounded(e.Stack, 8192)
	e.Route, e.Model, e.RequestID = bounded(e.Route, 512), bounded(e.Model, 256), bounded(e.RequestID, 256)
	e.Kind, e.Method = bounded(e.Kind, 32), bounded(e.Method, 16)
	e.ErrorCode, e.ErrorType = bounded(e.ErrorCode, 256), bounded(e.ErrorType, 256)
	e.SchedulerState = bounded(e.SchedulerState, 128)
	if e.Upstream != nil {
		u := *e.Upstream
		u.ErrorKind, u.RequestID = bounded(u.ErrorKind, 256), bounded(u.RequestID, 256)
		u.Message = bounded(u.Message, 8192)
		e.Upstream = &u
	}
	e.Revision = c.revision
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	select {
	case c.queue <- e:
	default:
		c.dropped.Add(1)
	}
}

func (c *Collector) Dropped() uint64 { return c.dropped.Load() }

func (c *Collector) Close() error {
	if c == nil {
		return nil
	}
	activeCollector.CompareAndSwap(c, nil)
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.queue)
	}
	c.mu.Unlock()
	<-c.done
	return c.writeErr
}

func (c *Collector) writeLoop(f *os.File, size int64) {
	defer close(c.done)
	defer func() {
		if f != nil {
			c.writeErr = errors.Join(c.writeErr, f.Close())
		}
	}()
	for e := range c.queue {
		if c.writeErr != nil {
			c.dropped.Add(1)
			continue
		}
		data, err := json.Marshal(sanitizeEvent(e))
		if err == nil && size+int64(len(data)+1) > c.maxBytes {
			err = f.Close()
			f = nil
			if err == nil {
				if removeErr := os.Remove(c.path + ".1"); removeErr != nil && !os.IsNotExist(removeErr) {
					err = removeErr
				}
			}
			if err == nil {
				err = os.Rename(c.path, c.path+".1")
			}
			if err == nil {
				f, err = os.OpenFile(c.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			}
			size = 0
		}
		if err == nil {
			var n int
			n, err = f.Write(append(data, '\n'))
			size += int64(n)
		}
		if err != nil {
			c.writeErr = err
			c.dropped.Add(1)
			log.Printf("diag: collection stopped after write failure: %s", SafeText(err.Error()))
		}
	}
	if n := c.Dropped(); n > 0 {
		log.Printf("diag: dropped %d events (queue full or write failure)", n)
	}
}
