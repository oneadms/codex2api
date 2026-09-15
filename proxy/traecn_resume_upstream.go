package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type traeCNResumeUpstreamContextKey struct{}

// 游标只在完整事件交付后推进。重连沿用原请求及转换器，避免另起一轮生成。
type traeCNResumableSource struct {
	ctx            context.Context
	reopen         func(string) (io.ReadCloser, error)
	mu             sync.Mutex
	body           io.ReadCloser
	closed         bool
	reader         *bufio.Reader
	pending        []byte
	cursor         string
	seen           map[string]string
	attempts       int
	reconnecting   bool
	replayedCursor bool
}

func newTraeCNResumableSource(ctx context.Context, body io.ReadCloser, reopen func(string) (io.ReadCloser, error)) *traeCNResumableSource {
	return &traeCNResumableSource{ctx: ctx, body: body, reader: bufio.NewReader(body), reopen: reopen, seen: make(map[string]string)}
}

func (s *traeCNResumableSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return s.body.Close()
}

func (s *traeCNResumableSource) reconnect(cause error) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if s.cursor == "" {
		return fmt.Errorf("TRAE stream interrupted without an event cursor: %w", cause)
	}
	for s.attempts < 3 {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return io.ErrClosedPipe
		}
		_ = s.body.Close()
		s.mu.Unlock()
		s.attempts++
		timer := time.NewTimer(time.Duration(s.attempts) * 250 * time.Millisecond)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return s.ctx.Err()
		case <-timer.C:
		}
		body, err := s.reopen(s.cursor)
		if err != nil {
			cause = err
			continue
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = body.Close()
			return io.ErrClosedPipe
		}
		s.body = body
		s.mu.Unlock()
		s.reader = bufio.NewReader(body)
		s.reconnecting = true
		s.replayedCursor = false
		return nil
	}
	return fmt.Errorf("TRAE cursor resume exhausted: %w", cause)
}

func (s *traeCNResumableSource) frame() ([]byte, error) {
	var frame []byte
	lineStart := 0
	for {
		line, err := s.reader.ReadSlice('\n')
		if len(frame)+len(line) > traeCNResumeFrameBytes {
			return nil, errors.New("TRAE event exceeds resume frame limit")
		}
		frame = append(frame, line...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			// 半个事件不能交给转换器；恢复后由服务端从上个完整游标补齐。
			return nil, err
		}
		completeLine := frame[lineStart:]
		if bytes.Equal(completeLine, []byte("\n")) || bytes.Equal(completeLine, []byte("\r\n")) {
			return frame, nil
		}
		lineStart = len(frame)
	}
}

func (s *traeCNResumableSource) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(s.pending) == 0 {
		if err := s.ctx.Err(); err != nil {
			return 0, err
		}
		frame, err := s.frame()
		if err != nil {
			if resumeErr := s.reconnect(err); resumeErr != nil {
				return 0, resumeErr
			}
			continue
		}
		id, hasData := "", false
		for _, line := range bytes.Split(frame, []byte("\n")) {
			line = bytes.TrimSuffix(line, []byte("\r"))
			if bytes.HasPrefix(line, []byte("id:")) {
				id = string(bytes.TrimPrefix(line[3:], []byte(" ")))
			}
			if bytes.HasPrefix(line, []byte("data:")) {
				hasData = true
			}
		}
		if !hasData {
			s.pending = frame
			break
		}
		if strings.ContainsAny(id, "\x00\r\n") || len(id) > 256 {
			return 0, errors.New("TRAE returned an invalid event cursor")
		}
		if s.reconnecting && id == "" {
			return 0, errors.New("TRAE resume did not return event cursors")
		}
		if id != "" {
			digest := traeCNResumeDigest(frame)
			if previous, exists := s.seen[id]; exists {
				// 允许服务端补发最后一个事件；从更早位置重播说明游标未被接受。
				if s.reconnecting && !s.replayedCursor && id == s.cursor && previous == digest {
					s.replayedCursor = true
					continue
				}
				return 0, errors.New("TRAE resume returned a repeated or conflicting event cursor")
			}
			if len(s.seen) >= traeCNResumeRecordLimit {
				return 0, errors.New("TRAE resume cursor limit exceeded")
			}
			s.seen[id] = digest
			s.cursor = id
		} else {
			// 已交付但没有游标的正文不能靠旧游标恢复，否则会重复追加正文。
			s.cursor = ""
		}
		s.reconnecting = false
		s.pending = frame
	}
	// 每次最多交付一个事件，下一次读取失败前转换器已消费上个完整事件。
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

func wrapTraeCNResumeUpstream(ctx context.Context, client *http.Client, req *http.Request, body io.ReadCloser) io.ReadCloser {
	if enabled, _ := ctx.Value(traeCNResumeUpstreamContextKey{}).(bool); !enabled {
		return body
	}
	return newTraeCNResumableSource(ctx, body, func(cursor string) (io.ReadCloser, error) {
		if req.GetBody == nil {
			return nil, errors.New("TRAE request cannot be replayed")
		}
		retry := req.Clone(ctx)
		var err error
		retry.Body, err = req.GetBody()
		if err != nil {
			return nil, err
		}
		retry.Header.Set("Last-Event-ID", cursor)
		log.Printf("[TRAE-RESUME] stage=upstream_reconnect trae_request_id=%q cursor=%q", responsesIdentityLogValue(req.Header.Get("X-Trae-Request-ID")), cursor)
		response, err := client.Do(retry)
		if err != nil {
			return nil, err
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			_ = response.Body.Close()
			return nil, fmt.Errorf("TRAE cursor resume HTTP status %d", response.StatusCode)
		}
		return response.Body, nil
	})
}
