package wsrelay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
)

// 保存并重新加载配置后，新建 WS 连接仍须发送手动指定的完整 UA。
func TestWebsocketHandshakePreservesRawUserAgentAfterSettingsReload(t *testing.T) {
	t.Setenv("CODEX_WS_SEND_USER_AGENT", "true")
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "ua.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	const wantUA = "codex-tui/0.147.0 (Mac OS 15.4.0; arm64) tmux/3.5a (codex-tui; 0.147.0)"
	if err := db.UpdateSystemSettings(ctx, &database.SystemSettings{
		ClientCompatMode:     proxy.ClientCompatModeForce,
		CodexUserAgentConfig: `{"raw_user_agent":"` + wantUA + `"}`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateCodexSyncedCLIVersion(ctx, "0.153.4"); err != nil {
		t.Fatal(err)
	}
	settings, err := db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings == nil || settings.CodexSyncedCLIVersion != "0.153.4" {
		t.Fatalf("同步版本未持久化: %+v", settings)
	}
	proxy.ApplyRuntimeSettingsFromSystem(settings)

	received := make(chan http.Header, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		received <- r.Header.Clone()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	manager := NewManager()
	t.Cleanup(manager.Stop)
	executor := NewExecutorWithManager(manager)
	account := &auth.Account{DBID: 42, AccountID: "ua-account", DynamicConcurrencyLimit: 1}
	headers := executor.prepareWebsocketHeaders("token", account, account.AccountID, "ua-session", "ua-key", nil, http.Header{}, nil)
	conn, _, err := manager.AcquireConnection(ctx, account, "ws"+strings.TrimPrefix(server.URL, "http"), "ua-session", headers, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { manager.DiscardConnection(conn) })

	select {
	case actual := <-received:
		if got := actual.Get("User-Agent"); got != wantUA {
			t.Fatalf("上游握手 UA = %q，期望 %q", got, wantUA)
		}
		if got := actual.Get("Version"); got != "0.147.0" {
			t.Fatalf("上游 Version = %q，须与手动 UA 版本一致", got)
		}
		if !conn.upstreamUserAgentKnown || conn.upstreamUserAgent != wantUA {
			t.Fatalf("握手 UA 审计值 = %q，期望 %q", conn.upstreamUserAgent, wantUA)
		}
	case <-ctx.Done():
		t.Fatal("等待上游握手超时")
	}
}
