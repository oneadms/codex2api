package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func TestBuildReverseProxyURL(t *testing.T) {
	// 保存并恢复原始配置
	old := resinCfg.Load()
	defer func() { resinCfg.Store(old) }()

	SetResinConfig(&ResinConfig{
		BaseURL:      "http://127.0.0.1:2260/my-token",
		PlatformName: "codex2api",
	})

	tests := []struct {
		name      string
		targetURL string
		want      string
	}{
		{
			name:      "HTTPS codex responses",
			targetURL: "https://chatgpt.com/backend-api/codex/responses",
			want:      "http://127.0.0.1:2260/my-token/codex2api/https/chatgpt.com/backend-api/codex/responses",
		},
		{
			name:      "HTTPS codex responses compact",
			targetURL: "https://chatgpt.com/backend-api/codex/responses/compact",
			want:      "http://127.0.0.1:2260/my-token/codex2api/https/chatgpt.com/backend-api/codex/responses/compact",
		},
		{
			name:      "HTTPS auth token URL",
			targetURL: "https://auth.openai.com/oauth/token",
			want:      "http://127.0.0.1:2260/my-token/codex2api/https/auth.openai.com/oauth/token",
		},
		{
			name:      "URL with query params",
			targetURL: "https://api.example.com/healthz?foo=bar",
			want:      "http://127.0.0.1:2260/my-token/codex2api/https/api.example.com/healthz?foo=bar",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildReverseProxyURL(tt.targetURL)
			if got != tt.want {
				t.Fatalf("BuildReverseProxyURL(%q)\n  got:  %q\n  want: %q", tt.targetURL, got, tt.want)
			}
		})
	}
}

func TestBuildWebSocketURL(t *testing.T) {
	old := resinCfg.Load()
	defer func() { resinCfg.Store(old) }()

	SetResinConfig(&ResinConfig{
		BaseURL:      "http://127.0.0.1:2260/my-token",
		PlatformName: "codex2api",
	})

	tests := []struct {
		name      string
		targetURL string
		want      string
	}{
		{
			name:      "WSS codex responses",
			targetURL: "wss://chatgpt.com/backend-api/codex/responses",
			want:      "ws://127.0.0.1:2260/my-token/codex2api/https/chatgpt.com/backend-api/codex/responses",
		},
		{
			name:      "WS target",
			targetURL: "ws://local.dev/ws",
			want:      "ws://127.0.0.1:2260/my-token/codex2api/http/local.dev/ws",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildWebSocketURL(tt.targetURL)
			if got != tt.want {
				t.Fatalf("BuildWebSocketURL(%q)\n  got:  %q\n  want: %q", tt.targetURL, got, tt.want)
			}
		})
	}
}

func TestIsResinEnabled(t *testing.T) {
	old := resinCfg.Load()
	defer func() { resinCfg.Store(old) }()

	// 禁用状态
	SetResinConfig(nil)
	if IsResinEnabled() {
		t.Fatal("expected Resin disabled when config is nil")
	}

	// 空 URL
	SetResinConfig(&ResinConfig{BaseURL: "", PlatformName: "test"})
	if IsResinEnabled() {
		t.Fatal("expected Resin disabled when BaseURL is empty")
	}

	// 启用状态
	SetResinConfig(&ResinConfig{BaseURL: "http://localhost:2260/tk", PlatformName: "test"})
	if !IsResinEnabled() {
		t.Fatal("expected Resin enabled")
	}

	// A separator-only list has no usable platform and must disable Resin just
	// like an empty field.
	SetResinConfig(&ResinConfig{BaseURL: "http://localhost:2260/tk", PlatformName: " , ; \n "})
	if IsResinEnabled() {
		t.Fatal("expected Resin disabled when platform list is empty after normalization")
	}
}

func TestSetResinConfigStoresNormalizedSnapshot(t *testing.T) {
	old := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(old) })

	input := &ResinConfig{
		BaseURL:      "  http://localhost:2260/tk/  ",
		PlatformName: " p1, p2, p1 ",
	}
	SetResinConfig(input)
	input.BaseURL = "http://mutated.example"
	input.PlatformName = "mutated"

	cfg := GetResinConfig()
	if cfg == nil {
		t.Fatal("normalized Resin config is nil")
	}
	if cfg.BaseURL != "http://localhost:2260/tk/" {
		t.Fatalf("snapshot BaseURL = %q", cfg.BaseURL)
	}
	if got := cfg.platformList(); !reflect.DeepEqual(got, []string{"p1", "p2"}) {
		t.Fatalf("snapshot platforms = %#v", got)
	}
}

func TestBuildReverseProxyURL_Disabled(t *testing.T) {
	old := resinCfg.Load()
	defer func() { resinCfg.Store(old) }()

	SetResinConfig(nil)

	target := "https://chatgpt.com/backend-api/codex/responses"
	got := BuildReverseProxyURL(target)
	if got != target {
		t.Fatalf("expected passthrough when disabled, got %q", got)
	}
}

func TestParseResinPlatforms(t *testing.T) {
	want := []string{"p1", "p2", "p3"}
	if got := parseResinPlatforms(" p1, p2;;p1\np3, "); !reflect.DeepEqual(got, want) {
		t.Fatalf("parseResinPlatforms() = %#v, want %#v", got, want)
	}
	if got := parseResinPlatforms(" , ; \n "); len(got) != 0 {
		t.Fatalf("parseResinPlatforms(empty) = %#v, want empty", got)
	}
}

func TestResinPlatformForSession(t *testing.T) {
	old := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(old) })
	SetResinConfig(&ResinConfig{BaseURL: "http://127.0.0.1:2260/token", PlatformName: "p1,p2,p3"})

	first := ResinPlatformForSession("session-1")
	if first == "" {
		t.Fatal("ResinPlatformForSession returned empty platform")
	}
	if got := ResinPlatformForSession("session-1"); got != first {
		t.Fatalf("same session mapped to different platforms: %q vs %q", first, got)
	}
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		seen[ResinPlatformForSession(fmt.Sprintf("session-%d", i))] = true
	}
	if len(seen) != 3 {
		t.Fatalf("hash did not cover all configured platforms: %#v", seen)
	}
	if got := ResinPlatformForSession(""); got != "p1" {
		t.Fatalf("empty session key = %q, want default p1", got)
	}

	SetResinConfig(&ResinConfig{BaseURL: "http://127.0.0.1:2260/token", PlatformName: "only"})
	for _, key := range []string{"", "a", "b", "c"} {
		if got := ResinPlatformForSession(key); got != "only" {
			t.Fatalf("single platform key %q = %q, want only", key, got)
		}
	}
}

func TestResinPlatformForSessionConcurrent(t *testing.T) {
	old := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(old) })
	SetResinConfig(&ResinConfig{BaseURL: "http://127.0.0.1:2260/token", PlatformName: "p1,p2,p3"})

	want := make(map[string]string)
	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("session-%d", i)
		want[key] = ResinPlatformForSession(key)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for key, expected := range want {
				if got := ResinPlatformForSession(key); got != expected {
					t.Errorf("session %q mapped to %q, want %q", key, got, expected)
				}
			}
		}()
	}
	wg.Wait()
}

func TestBuildResinURLForPlatform(t *testing.T) {
	old := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(old) })
	SetResinConfig(&ResinConfig{
		BaseURL:      "http://127.0.0.1:2260/my-token",
		PlatformName: "p1,p2,p3",
	})

	target := "https://chatgpt.com/backend-api/codex/responses?x=1"
	wantHTTP := "http://127.0.0.1:2260/my-token/p2/https/chatgpt.com/backend-api/codex/responses?x=1"
	if got := BuildReverseProxyURLForPlatform(target, "p2"); got != wantHTTP {
		t.Fatalf("BuildReverseProxyURLForPlatform() = %q, want %q", got, wantHTTP)
	}
	wantWS := "ws://127.0.0.1:2260/my-token/p3/https/chatgpt.com/backend-api/codex/responses?x=1"
	if got := BuildWebSocketURLForPlatform("wss://chatgpt.com/backend-api/codex/responses?x=1", "p3"); got != wantWS {
		t.Fatalf("BuildWebSocketURLForPlatform() = %q, want %q", got, wantWS)
	}
	// Legacy builders remain deterministic and use the first platform when no
	// request-scoped selection is supplied.
	if got := BuildReverseProxyURL(target); got != "http://127.0.0.1:2260/my-token/p1/https/chatgpt.com/backend-api/codex/responses?x=1" {
		t.Fatalf("legacy BuildReverseProxyURL() = %q", got)
	}
}

func TestResinPlatformContext(t *testing.T) {
	ctx := WithResinPlatform(nil, " p2 ")
	if got := ResinPlatformFromContext(ctx); got != "p2" {
		t.Fatalf("ResinPlatformFromContext() = %q, want p2", got)
	}
	if got := ResinPlatformFromContext(nil); got != "" {
		t.Fatalf("ResinPlatformFromContext(nil) = %q, want empty", got)
	}
}

func TestResinPlatformForSessionIdentityUsesSchedulerAffinityKey(t *testing.T) {
	old := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(old) })
	SetResinConfig(&ResinConfig{BaseURL: "http://127.0.0.1:2260/token", PlatformName: "p1,p2,p3"})

	identity := requestSessionIdentity{affinityID: "session-42", hasStableAffinity: true}
	for _, apiKeyID := range []int64{0, 17} {
		want := ResinPlatformForSession(sessionAffinityKey(identity.affinityID, apiKeyID))
		if got := resinPlatformForSessionIdentity(identity, apiKeyID); got != want {
			t.Fatalf("apiKeyID=%d platform=%q, want %q", apiKeyID, got, want)
		}
	}
	if got := resinPlatformForSessionIdentity(requestSessionIdentity{affinityID: "random"}, 17); got != "p1" {
		t.Fatalf("unstable identity platform=%q, want default p1", got)
	}
}

func TestResinPlatformForExecutorFallbacks(t *testing.T) {
	old := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(old) })
	SetResinConfig(&ResinConfig{BaseURL: "http://127.0.0.1:2260/token", PlatformName: "p1,p2,p3"})

	headers := http.Header{}
	headers.Set("Authorization", "Bearer key-without-explicit-session")
	body := []byte(`{"model":"gpt-5.4"}`)
	identity := resolveRequestSessionIdentity(headers, body)
	if !identity.hasStableAffinity {
		t.Fatal("API key fallback should be marked stable")
	}
	want := ResinPlatformForSession(identity.affinityID)
	if got := resinPlatformForExecutor(context.Background(), "", headers, body); got != want {
		t.Fatalf("API-key fallback platform = %q, want %q", got, want)
	}

	// No explicit session, content seed, or API key: use the default platform
	// rather than hashing the final random identity.
	if got := resinPlatformForExecutor(context.Background(), "", nil, []byte(`{"model":"gpt-5.4"}`)); got != "p1" {
		t.Fatalf("anonymous fallback platform = %q, want p1", got)
	}
	if got := resinPlatformForExecutor(context.Background(), "", nil, nil, "embedded-key"); got != ResinPlatformForCredential("embedded-key") {
		t.Fatalf("embedded credential fallback platform = %q, want %q", got, ResinPlatformForCredential("embedded-key"))
	}
	if got := resinPlatformForExecutor(WithResinPlatform(context.Background(), "p3"), "", nil, nil); got != "p3" {
		t.Fatalf("pinned platform = %q, want p3", got)
	}

	// Realtime clients can authenticate through the WebSocket subprotocol and
	// still need a deterministic platform when they omit a session header.
	wsHeaders := http.Header{}
	wsHeaders.Set("Sec-WebSocket-Protocol", "realtime, openai-insecure-api-key.ws-key")
	wsIdentity := resolveRequestSessionIdentity(wsHeaders, []byte(`{"model":"gpt-5.4"}`))
	if !wsIdentity.hasStableAffinity {
		t.Fatal("WebSocket subprotocol credential should be marked stable")
	}
	if got, expected := resinPlatformForExecutor(context.Background(), "", wsHeaders, []byte(`{"model":"gpt-5.4"}`)), ResinPlatformForSession(wsIdentity.affinityID); got != expected {
		t.Fatalf("WebSocket subprotocol platform = %q, want %q", got, expected)
	}
	for _, headerName := range []string{"X-API-Key", "Anthropic-Auth-Token"} {
		aliasHeaders := http.Header{}
		aliasHeaders.Set(headerName, "alias-key")
		aliasIdentity := resolveRequestSessionIdentity(aliasHeaders, []byte(`{"model":"gpt-5.4"}`))
		if !aliasIdentity.hasStableAffinity {
			t.Fatalf("%s credential should be marked stable", headerName)
		}
		if got, expected := resinPlatformForExecutor(context.Background(), "", aliasHeaders, []byte(`{"model":"gpt-5.4"}`)), ResinPlatformForSession(aliasIdentity.affinityID); got != expected {
			t.Fatalf("%s platform = %q, want %q", headerName, got, expected)
		}
	}
}

func TestExecuteRequestUsesSessionPlatform(t *testing.T) {
	old := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(old) })

	type captured struct {
		path    string
		account string
	}
	received := make(chan captured, 1)
	resin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- captured{path: r.URL.Path, account: r.Header.Get("X-Resin-Account")}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resin-platform-test"}`)
	}))
	t.Cleanup(resin.Close)
	SetResinConfig(&ResinConfig{BaseURL: resin.URL + "/token", PlatformName: "p1,p2,p3"})

	const sessionKey = "session-platform-route"
	platform := ResinPlatformForSession(sessionKey)
	headers := http.Header{}
	headers.Set("Session-Id", sessionKey)
	account := &auth.Account{DBID: 42, AccessToken: "access-token"}
	resp, err := ExecuteRequest(context.Background(), account, []byte(`{"model":"gpt-5.4","input":"hello"}`), "", "", "sk-test", nil, headers, false)
	if err != nil {
		t.Fatalf("ExecuteRequest() error = %v", err)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	select {
	case got := <-received:
		wantPath := "/token/" + platform + "/https/chatgpt.com/backend-api/codex/responses"
		if got.path != wantPath {
			t.Fatalf("Resin path = %q, want %q", got.path, wantPath)
		}
		if got.account != "42" {
			t.Fatalf("X-Resin-Account = %q, want 42", got.account)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Resin request")
	}
}
