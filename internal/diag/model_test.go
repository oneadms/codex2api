package diag

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func modelEnvelope(content, finish string) string {
	data, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"finish_reason": finish, "message": map[string]string{"content": content}}}})
	return string(data)
}

func TestChatAnalyzerRequestsAndStrictResponses(t *testing.T) {
	responses := []string{
		modelEnvelope(`{"files":[{"path":"counter.go","query":"Step"}]}`, "stop"),
		modelEnvelope(`{"title":"Fix counter","root_cause":"Step decrements instead of incrementing","confidence":0.92,"can_fix":true,"edits":[{"path":"counter.go","old":"x - 1","new":"x + 1"}]}`, "stop"),
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("missing model auth or method")
		}
		var payload struct {
			Model    string                           `json:"model"`
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload.Model != "test-model" || len(payload.Messages) != 2 || !strings.Contains(payload.Messages[0].Content, "UNTRUSTED") {
			t.Error("model request missing constraints")
		}
		if calls >= len(responses) {
			t.Error("unexpected extra LLM request")
			w.WriteHeader(500)
			return
		}
		_, _ = w.Write([]byte(responses[calls]))
		calls++
	}))
	defer srv.Close()
	a := &ChatAnalyzer{URL: srv.URL, APIKey: "test-key", Model: "test-model"}
	selected, err := a.Select(context.Background(), Group{}, []string{"counter.go"})
	if err != nil || len(selected) != 1 {
		t.Fatalf("select: %v %v", selected, err)
	}
	d, err := a.Diagnose(context.Background(), Group{}, []Source{{Path: "counter.go", Content: "func Step(x int) int { return x - 1 }"}})
	if err != nil || !d.CanFix || len(d.Edits) != 1 {
		t.Fatalf("diagnosis: %+v %v", d, err)
	}
}

func TestChatAnalyzerRejectsIncompleteAndMalformedOutput(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
	}{
		{"truncated", modelEnvelope(`{"files":[]}`, "length"), 200},
		{"unknown-fields", modelEnvelope(`{"files":[],"command":"rm -rf /"}`, "stop"), 200},
		{"trailing", modelEnvelope(`{"files":[]} {"files":[]}`, "stop"), 200},
		{"fenced", modelEnvelope("```json\n{\"files\":[]}\n```", "stop"), 200},
		{"oversized", strings.Repeat("x", (256<<10)+1), 200},
		{"server-error", "secret-upstream-body", 503},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer srv.Close()
			a := &ChatAnalyzer{URL: srv.URL, Model: "model"}
			if _, err := a.Select(context.Background(), Group{}, nil); err == nil || strings.Contains(err.Error(), "secret-upstream-body") {
				t.Fatalf("unsafe or missing failure: %v", err)
			}
		})
	}
}

func TestChatAnalyzerRejectsRedirectAndRemotePlaintext(t *testing.T) {
	if err := (&ChatAnalyzer{URL: "http://example.com/v1/chat/completions", Model: "model"}).Validate(); err == nil {
		t.Fatal("remote plaintext endpoint accepted")
	}
	targetCalls := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls++ }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	a := &ChatAnalyzer{URL: srv.URL, Model: "model", APIKey: "secret"}
	if _, err := a.Select(context.Background(), Group{}, nil); err == nil || targetCalls != 0 {
		t.Fatalf("redirect followed: %d %v", targetCalls, err)
	}
}
