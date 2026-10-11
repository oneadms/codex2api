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

// TestTraeCNKeepaliveCloseReleasesBlockedHeartbeat 钉住转换器收尾顺序的前提：
// 下游停读时心跳会阻塞在 pipe 写里，只有先关掉 pipe writer 才能让心跳收尾。
// 转换器的 defer 必须按"先 writer.Close() 再 keepalive.Close()"执行，否则
// keepalive.Close() 会一直等一个写不出去的心跳，转换 goroutine 与上游请求都收不了尾。
func TestTraeCNKeepaliveCloseReleasesBlockedHeartbeat(t *testing.T) {
	t.Setenv("TRAECN_KEEPALIVE_SECONDS", "1")
	reader, writer := io.Pipe()
	defer reader.Close()
	keepalive := startTraeCNKeepalive(writer)

	// 下游不读：等到心跳真的进入阻塞写（互斥量此时被心跳持有）。
	time.Sleep(1500 * time.Millisecond)

	closed := make(chan struct{})
	go func() {
		_ = keepalive.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("keepalive.Close 在下游停读且 pipe 仍打开时提前返回，说明心跳没有真正写出去")
	case <-time.After(300 * time.Millisecond):
	}

	// 修复点：先关 pipe writer，被阻塞的心跳写立刻返回，Close 才能收尾。
	if err := writer.Close(); err != nil {
		t.Fatalf("closing the pipe writer failed: %v", err)
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("关闭 pipe writer 后 keepalive.Close 仍然阻塞：转换 goroutine 会泄漏")
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
