package proxy

import (
	"io"
	"strings"
	"testing"
	"time"
)

// 上游静默时下游必须持续收到 SSE 注释保活：链路上的反代（Cloudflare / New API）
// 空闲读超时通常 60~125s，长思考期间没有字节就会被掐断，客户端表现为
// "正在重新连接 N/5"并整轮重试。
func TestTraeCNStreamEmitsKeepaliveWhileUpstreamIsSilent(t *testing.T) {
	t.Setenv("TRAECN_KEEPALIVE_SECONDS", "1")
	source, sourceWriter := io.Pipe()
	defer sourceWriter.Close()
	stream := traeCNCanonicalStreamForTools(source, "DeepSeek-V4-Pro", nil, nil)
	defer stream.Close()

	buffer := make([]byte, 4096)
	read := func() string {
		t.Helper()
		done := make(chan string, 1)
		go func() {
			n, _ := stream.Read(buffer)
			done <- string(buffer[:n])
		}()
		select {
		case chunk := <-done:
			return chunk
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for stream data")
			return ""
		}
	}

	first := read()
	if !strings.Contains(first, "response.created") {
		t.Fatalf("first chunk = %q, want response.created", first)
	}
	keepalive := read()
	if !strings.Contains(keepalive, ": keepalive") {
		t.Fatalf("chunk = %q, want an SSE keepalive comment while the upstream is silent", keepalive)
	}

	// 保活之后正常事件仍然完整可解析（不会与注释交错）。
	go func() {
		io.WriteString(sourceWriter, "event: output\ndata: {\"response\":\"ok\"}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
	}()
	deadline := time.Now().Add(3 * time.Second)
	var events strings.Builder
	for time.Now().Before(deadline) {
		events.WriteString(read())
		if strings.Contains(events.String(), "response.completed") {
			break
		}
	}
	if !strings.Contains(events.String(), `"delta":"ok"`) || !strings.Contains(events.String(), "response.completed") {
		t.Fatalf("stream events = %q, want the text delta and completion", events.String())
	}
}

func TestTraeCNKeepaliveCanBeDisabled(t *testing.T) {
	t.Setenv("TRAECN_KEEPALIVE_SECONDS", "0")
	source, sourceWriter := io.Pipe()
	defer sourceWriter.Close()
	stream := traeCNCanonicalStreamForTools(source, "DeepSeek-V4-Pro", nil, nil)
	defer stream.Close()

	buffer := make([]byte, 4096)
	n, _ := stream.Read(buffer)
	if got := string(buffer[:n]); !strings.Contains(got, "response.created") {
		t.Fatalf("first chunk = %q", got)
	}
	done := make(chan string, 1)
	go func() {
		n, _ := stream.Read(buffer)
		done <- string(buffer[:n])
	}()
	select {
	case chunk := <-done:
		if strings.Contains(chunk, ": keepalive") {
			t.Fatalf("keepalive must be off when disabled: %q", chunk)
		}
	case <-time.After(2500 * time.Millisecond):
		// 关闭后静默是对的。
	}
}
