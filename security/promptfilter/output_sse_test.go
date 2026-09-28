package promptfilter

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func newSSEOutputTestScanner() *OutputScanner {
	cfg := testConfig(ModeBlock)
	cfg.StrictTerminalEnabled = true
	cfg.Advanced.Output = OutputConfig{Enabled: true, BufferBytes: 512, OverlapBytes: 64, StrictOnly: true}
	return NewOutputScanner(cfg)
}

func outputTestSSEFrame(text, newline string) []byte {
	payload, _ := json.Marshal(map[string]string{"type": "response.output_text.delta", "delta": text})
	return []byte("data: " + string(payload) + newline + newline)
}

// 过滤窗口仍按字节保留，但每次释放的数据必须停在完整 SSE 事件的边界。
func TestOutputScannerPreservesSSEFrames(t *testing.T) {
	for _, newline := range []string{"\n", "\r\n"} {
		t.Run(newline, func(t *testing.T) {
			scanner := newSSEOutputTestScanner()
			first := outputTestSSEFrame(strings.Repeat("甲", 400), newline)
			second := outputTestSSEFrame(strings.Repeat("乙", 400), newline)
			var received bytes.Buffer
			// 也覆盖 data: 字段和 UTF-8 字符被底层读取拆开的情况。
			for _, part := range [][]byte{first[:2], first[2:100], first[100:]} {
				out, err := scanner.Push(part)
				if err != nil || len(out) != 0 {
					t.Fatalf("incomplete safety window released a frame prefix: bytes=%d err=%v", len(out), err)
				}
			}
			out, err := scanner.Push(second)
			if err != nil || !bytes.Equal(out, first) {
				t.Fatalf("expected exactly the first frame: bytes=%d want=%d err=%v", len(out), len(first), err)
			}
			received.Write(out)
			terminal := []byte("data: [DONE]" + newline + newline)
			out, err = scanner.Push(terminal)
			if err != nil {
				t.Fatal(err)
			}
			received.Write(out)
			want := bytes.Join([][]byte{first, second, terminal}, nil)
			if !bytes.Equal(received.Bytes(), want) {
				t.Fatalf("frames changed during filtering: bytes=%d want=%d", received.Len(), len(want))
			}
		})
	}
}

func TestOutputScannerSSEStillBlocksAcrossEvents(t *testing.T) {
	cfg := testConfig(ModeBlock)
	cfg.StrictTerminalEnabled = true
	cfg.Advanced.Output = OutputConfig{Enabled: true, BufferBytes: 512, OverlapBytes: 64, StrictOnly: true}
	cfg.CustomPatterns = []PatternConfig{{Name: "cross_event", Pattern: `blocked-marker`, Weight: 100, Strict: true}}
	scanner := NewOutputScanner(cfg)
	first := outputTestSSEFrame(strings.Repeat("safe ", 160)+"blocked-", "\n")
	if out, err := scanner.Push(first); err != nil || bytes.Contains(out, []byte("blocked-")) {
		t.Fatalf("unsafe suffix escaped the window: bytes=%d err=%v", len(out), err)
	}
	if out, err := scanner.Push(outputTestSSEFrame("marker", "\n")); !errors.Is(err, ErrOutputBlocked) || len(out) != 0 {
		t.Fatalf("cross-event match was not blocked: bytes=%d err=%v", len(out), err)
	}
}

func TestOutputScannerChatErrorReleasesPendingFrames(t *testing.T) {
	scanner := newSSEOutputTestScanner()
	first := outputTestSSEFrame("safe", "\n")
	if out, err := scanner.Push(first); err != nil || len(out) != 0 {
		t.Fatalf("unexpected initial output: %q err=%v", out, err)
	}
	terminal := []byte("data: {\"error\":{\"code\":\"upstream_stream_break\",\"message\":\"upstream ended\"}}\n\n")
	out, err := scanner.Push(terminal)
	if err != nil || !bytes.Equal(out, bytes.Join([][]byte{first, terminal}, nil)) {
		t.Fatalf("terminal Chat error was retained: %q err=%v", out, err)
	}
	if bytes.Contains(out, []byte("[DONE]")) {
		t.Fatal("failed Chat stream must not become a successful terminal")
	}
}

func TestOutputScannerLiteralErrorDoesNotReleaseWindow(t *testing.T) {
	for _, data := range [][]byte{
		[]byte(`{"error":{"message":"ordinary JSON"}}`),
		outputTestSSEFrame(`{"error":{"message":"ordinary text"}}`, "\n"),
	} {
		scanner := newSSEOutputTestScanner()
		if out, err := scanner.Push(data); err != nil || len(out) != 0 {
			t.Fatalf("literal error released the safety window: %q err=%v", out, err)
		}
	}
}
