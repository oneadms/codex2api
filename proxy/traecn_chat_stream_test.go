package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// 使用真实 HTTP 入口和 Resin 路由，覆盖 TRAE 原始事件到 Chat 数据帧的完整转换。
func newTraeCNChatStreamServer(t *testing.T, filterOutput bool, serve http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	previousResin := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(previousResin) })
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/traecn-test/https/traecn.example"+auth.TraeCNChatPath || r.Header.Get("X-Resin-Account") != "91089" {
			t.Errorf("unexpected Resin route: path=%q account=%q", r.URL.Path, r.Header.Get("X-Resin-Account"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		serve(w, r)
	}))
	t.Cleanup(upstream.Close)
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "traecn-test"})
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, MaxRetries: 0})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{
		DBID: 91089, UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT", RefreshToken: "RT",
		ExpiresAt: time.Now().Add(time.Hour), TraeCNHost: "https://traecn.example",
	})
	if filterOutput {
		cfg := promptfilter.DefaultConfig()
		cfg.Enabled = true
		cfg.Advanced.Output = promptfilter.OutputConfig{Enabled: true, BufferBytes: 512, OverlapBytes: 64, StrictOnly: true}
		store.SetPromptFilterConfig(cfg)
	}
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	router.POST("/v1/chat/completions", func(c *gin.Context) {
		c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 91089, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelTraeCN}})
		handler.ChatCompletions(c)
	})
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server, &calls
}

func openTraeCNChatTestStream(t *testing.T, server *httptest.Server) *http.Response {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"qwen3.8-max","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	return resp
}

func writeTraeCNChatTestOutput(w http.ResponseWriter, key, text string) {
	data, _ := json.Marshal(map[string]string{key: text})
	_, _ = fmt.Fprintf(w, "event: output\ndata: %s\n\n", data)
	w.(http.Flusher).Flush()
}

// 大于 64 KiB 的思考数据不能被截断；静默期间的心跳必须保持 SSE 帧完整。
func TestTraeCNChatLongReasoningStream(t *testing.T) {
	previousInterval := continuousRetryKeepaliveInterval
	continuousRetryKeepaliveInterval = 10 * time.Millisecond
	t.Cleanup(func() { continuousRetryKeepaliveInterval = previousInterval })
	previousSettings := CurrentRuntimeSettings()
	ApplyRuntimeSettings(DefaultRuntimeSettings())
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })
	for _, filterOutput := range []bool{false, true} {
		for _, complete := range []bool{true, false} {
			t.Run(fmt.Sprintf("filter=%t/complete=%t", filterOutput, complete), func(t *testing.T) {
				reasoning := strings.Repeat("思", 62249)
				server, calls := newTraeCNChatStreamServer(t, filterOutput, func(w http.ResponseWriter, r *http.Request) {
					writeTraeCNChatTestOutput(w, "reasoning_content", reasoning)
					select {
					case <-time.After(80 * time.Millisecond):
					case <-r.Context().Done():
						return
					}
					writeTraeCNChatTestOutput(w, "reasoning_content", "补充")
					if complete {
						writeTraeCNChatTestOutput(w, "content", "完成")
						_, _ = io.WriteString(w, "event: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
					}
				})
				resp := openTraeCNChatTestStream(t, server)
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				var gotReasoning, gotContent strings.Builder
				var done, failures, heartbeats int
				for index, line := range strings.Split(string(body), "\n") {
					switch {
					case strings.HasPrefix(line, ":"):
						heartbeats++
					case strings.HasPrefix(line, "data: "):
						data := strings.TrimPrefix(line, "data: ")
						if data == "[DONE]" {
							done++
							continue
						}
						if !json.Valid([]byte(data)) {
							t.Fatalf("invalid Chat JSON at line %d: bytes=%d tail=%q", index, len(data), data[max(0, len(data)-100):])
						}
						chunk := gjson.Parse(data)
						gotReasoning.WriteString(chunk.Get("choices.0.delta.reasoning_content").String())
						gotContent.WriteString(chunk.Get("choices.0.delta.content").String())
						if chunk.Get("error").Exists() {
							failures++
						}
					case strings.TrimSpace(line) != "":
						t.Fatalf("unexpected SSE line %d: bytes=%d", index, len(line))
					}
				}
				if gotReasoning.String() != reasoning+"补充" || heartbeats == 0 {
					t.Fatalf("reasoning bytes=%d want=%d heartbeats=%d", gotReasoning.Len(), len(reasoning+"补充"), heartbeats)
				}
				if complete && (gotContent.String() != "完成" || done != 1 || failures != 0) {
					t.Fatalf("completed stream: content=%q done=%d errors=%d", gotContent.String(), done, failures)
				}
				if !complete && (gotContent.Len() != 0 || done != 0 || failures != 1) {
					t.Fatalf("interrupted stream: content=%q done=%d errors=%d", gotContent.String(), done, failures)
				}
				if calls.Load() != 1 {
					t.Fatalf("visible reasoning must not restart generation: calls=%d", calls.Load())
				}
			})
		}
	}
}

// 首次思考到达后，即使正文晚于首字超时，Chat 仍应继续等待并正常结束。
func TestTraeCNChatReasoningDisarmsFirstTokenTimeout(t *testing.T) {
	previous := CurrentRuntimeSettings()
	settings := DefaultRuntimeSettings()
	settings.FirstTokenTimeoutSec = 1
	ApplyRuntimeSettings(settings)
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	reasoningReceived := make(chan struct{})
	server, calls := newTraeCNChatStreamServer(t, false, func(w http.ResponseWriter, r *http.Request) {
		writeTraeCNChatTestOutput(w, "reasoning_content", "开始思考")
		select {
		case <-reasoningReceived:
		case <-r.Context().Done():
			return
		}
		select {
		case <-time.After(1200 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		writeTraeCNChatTestOutput(w, "content", "完成")
		_, _ = io.WriteString(w, "event: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
	})
	resp := openTraeCNChatTestStream(t, server)
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reasoning did not arrive before completion: %v", err)
		}
		if strings.Contains(line, `"reasoning_content":"开始思考"`) {
			close(reasoningReceived)
			break
		}
	}
	body, err := io.ReadAll(reader)
	if err != nil || !strings.Contains(string(body), `"content":"完成"`) || !strings.Contains(string(body), "data: [DONE]") || calls.Load() != 1 {
		t.Fatalf("reasoning was interrupted: err=%v calls=%d body=%s", err, calls.Load(), body)
	}
}

func TestTraeCNChatDisconnectCancelsUpstream(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	server, calls := newTraeCNChatStreamServer(t, false, func(w http.ResponseWriter, r *http.Request) {
		writeTraeCNChatTestOutput(w, "reasoning_content", "开始思考")
		<-r.Context().Done()
		close(upstreamCanceled)
	})
	resp := openTraeCNChatTestStream(t, server)
	reader := bufio.NewReader(resp.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reasoning did not arrive: %v", err)
		}
		if strings.Contains(line, `"reasoning_content":"开始思考"`) {
			break
		}
	}
	_ = resp.Body.Close()
	select {
	case <-upstreamCanceled:
	case <-time.After(upstreamDrainTimeout + 2*time.Second):
		t.Fatal("Chat disconnect did not cancel the TRAE request after the usage drain window")
	}
	if calls.Load() != 1 {
		t.Fatalf("disconnected Chat request retried upstream: calls=%d", calls.Load())
	}
}
