package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func TestTraeCNResumeUpstreamPreservesRequestAndCanonicalState(t *testing.T) {
	var calls atomic.Int32
	var originalBody, requestID, traeID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			originalBody, requestID, traeID = string(body), r.Header.Get("X-Request-ID"), r.Header.Get("X-Trae-Request-ID")
			if r.Header.Get("Last-Event-ID") != "" {
				t.Error("initial request has a cursor")
			}
			_, _ = io.WriteString(w, "id: 37\nevent: output\ndata: {\"type\":\"text\",\"content\":\"ABC\"}\n\nid: 38\nevent: output\ndata: {\"content\":\"partial")
			return
		}
		if string(body) != originalBody || r.Header.Get("X-Request-ID") != requestID || r.Header.Get("X-Trae-Request-ID") != traeID {
			t.Error("resume changed original request identity/body")
		}
		if r.Header.Get("Last-Event-ID") != "37" {
			t.Errorf("cursor=%q, want 37", r.Header.Get("Last-Event-ID"))
		}
		_, _ = io.WriteString(w, "id: 38\nevent: output\ndata: {\"type\":\"text\",\"content\":\"DEF\"}\n\nid: 39\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
	}))
	defer server.Close()
	account := &auth.Account{UpstreamType: auth.UpstreamTraeCN, AccessToken: "AT", RefreshToken: "RT", ExpiresAt: time.Now().Add(time.Hour), TraeCNHost: server.URL}
	ctx := context.WithValue(t.Context(), traeCNResumeUpstreamContextKey{}, true)
	response, err := ExecuteTraeCNRequest(ctx, account, GrokProtocolResponses, []byte(traeCNResumeTestBody), []byte(traeCNResumeTestBody), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || requestID == "" || traeID == "" {
		t.Fatalf("upstream calls=%d, missing IDs=%t", calls.Load(), requestID == "" || traeID == "")
	}
	events := canonicalSSEEvents(t, payload)
	created, ok := findCanonicalEvent(events, "response.created")
	if !ok {
		t.Fatal("missing response.created")
	}
	completed, ok := findCanonicalEvent(events, "response.completed")
	if !ok {
		t.Fatalf("missing completion: %s", payload)
	}
	if created.Get("response.id").String() != completed.Get("response.id").String() {
		t.Fatal("resume changed response ID")
	}
	if completed.Get("response.output.0.content.0.text").String() != "ABCDEF" {
		t.Fatalf("resume lost state or consumed partial event: %s", payload)
	}
	var messageID string
	for _, event := range events {
		if event.Get("type").String() == "response.output_text.delta" {
			id := event.Get("item_id").String()
			if messageID != "" && messageID != id {
				t.Fatal("resume changed output item ID")
			}
			messageID = id
		}
	}
}

func TestTraeCNResumeUpstreamRejectsMissingOrIgnoredCursor(t *testing.T) {
	for _, tc := range []struct {
		name, initial, resumed, want string
		calls                        int
	}{
		{"missing", "event: output\ndata: {\"content\":\"ABC\"}\n\n", "", "without an event cursor", 0},
		{"ignored", "id: 1\nevent: output\ndata: {\"content\":\"ABC\"}\n\nid: 2\nevent: output\ndata: {\"content\":\"DEF\"}\n\n", "id: 1\nevent: output\ndata: {\"content\":\"ABC\"}\n\n", "repeated or conflicting", 1},
		{"changed", "id: 1\nevent: output\ndata: {\"content\":\"ABC\"}\n\n", "id: 1\nevent: output\ndata: {\"content\":\"DIFFERENT\"}\n\n", "repeated or conflicting", 1},
		{"no_resume_ids", "id: 1\nevent: output\ndata: {\"content\":\"ABC\"}\n\n", "event: output\ndata: {\"content\":\"DEF\"}\n\n", "did not return event cursors", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			source := newTraeCNResumableSource(t.Context(), io.NopCloser(strings.NewReader(tc.initial)), func(string) (io.ReadCloser, error) { calls++; return io.NopCloser(strings.NewReader(tc.resumed)), nil })
			defer source.Close()
			payload, err := io.ReadAll(source)
			if err == nil || !strings.Contains(err.Error(), tc.want) || calls != tc.calls {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
			if string(payload) != tc.initial {
				t.Fatalf("forwarded restarted or conflicting output: %q", payload)
			}
		})
	}
}

func TestTraeCNResumeUpstreamReplaysLastEventOnceAndStopsAtTerminal(t *testing.T) {
	initial := "id: 37\nevent: output\ndata: {\"content\":\"ABC\"}\n\n"
	calls := 0
	source := newTraeCNResumableSource(t.Context(), io.NopCloser(strings.NewReader(initial)), func(cursor string) (io.ReadCloser, error) {
		calls++
		if cursor != "37" {
			t.Errorf("cursor=%q", cursor)
		}
		return io.NopCloser(strings.NewReader(initial + "id: 38\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n")), nil
	})
	payload, err := io.ReadAll(traeCNCanonicalStream(source, "model"))
	if err != nil {
		t.Fatal(err)
	}
	completed, ok := findCanonicalEvent(canonicalSSEEvents(t, payload), "response.completed")
	if !ok || completed.Get("response.output.0.content.0.text").String() != "ABC" || calls != 1 {
		t.Fatalf("duplicate replay or resumed completed task: calls=%d %s", calls, payload)
	}
}

func TestTraeCNResumeUpstreamCancellationInterruptsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := newTraeCNResumableSource(ctx, io.NopCloser(strings.NewReader("id: 1\ndata: {}\n\n")), func(string) (io.ReadCloser, error) {
		t.Error("reconnected after cancellation")
		return nil, context.Canceled
	})
	defer source.Close()
	buffer := make([]byte, 100)
	if _, err := source.Read(buffer); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := source.Read(buffer); err != context.Canceled {
		t.Fatalf("error=%v", err)
	}
}

func TestTraeCNResumeUpstreamLongLinesKeepEventBoundary(t *testing.T) {
	// 跨越 bufio 边界且末尾换行单独落在一次 ReadSlice 中。
	line := "data: " + strings.Repeat("x", 4096-len("data: ")) + "\n"
	frame := "id: 1\n" + line + "data: final\n\n"
	source := newTraeCNResumableSource(t.Context(), io.NopCloser(strings.NewReader(frame)), nil)
	defer source.Close()
	got, err := source.frame()
	if err != nil || string(got) != frame {
		t.Fatalf("event split at long line: size=%d err=%v", len(got), err)
	}
}
