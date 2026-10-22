package wsrelay

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/harvest"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

// gatewayWSServer 模拟上游：记录每轮的路由 Cookie、turn state 与续接 ID，
// 返回 response.completed，模拟固定会话依赖的原连接续接。
type gatewayWSServer struct {
	server          *httptest.Server
	requests        chan string
	dials           atomic.Int64
	closeAfterReply atomic.Bool
	sequence        atomic.Int64
	mu              sync.Mutex
	connections     map[*websocket.Conn]bool
}

func (s *gatewayWSServer) closeConnections() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for conn := range s.connections {
		_ = conn.Close()
	}
}

func newGatewayWSServer(t *testing.T) *gatewayWSServer {
	t.Helper()
	s := &gatewayWSServer{requests: make(chan string, 64), connections: make(map[*websocket.Conn]bool)}
	up := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		s.dials.Add(1)
		s.mu.Lock()
		s.connections[conn] = true
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			delete(s.connections, conn)
			s.mu.Unlock()
			_ = conn.Close()
		}()
		responses := map[string]bool{}
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			id := fmt.Sprintf("resp_%d", s.sequence.Add(1))
			cookie := r.Header.Get("Cookie")
			state := gjson.GetBytes(payload, "client_metadata.x-codex-turn-state").String()
			previous := gjson.GetBytes(payload, "previous_response_id").String()
			s.requests <- strings.TrimSpace(cookie) + "|" + strings.TrimSpace(state) + "|" + previous
			if previous != "" && !responses[previous] {
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"error","error":{"code":"previous_response_not_found"}}`))
				continue
			}
			responses[id] = true
			frame := fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"status":"completed","model":"gpt-6-astra","error":null,"output":[{"content":[{"type":"output_text","text":"OK"}]}]}}`, id)
			if err := conn.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
				return
			}
			if s.closeAfterReply.Load() {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	s.server = server
	return s
}

// newGatewayManager 构造带真实数据库与账号的采票管理器，并注册为进程级实例。
// 返回 (manager, sourceID, targetID)：来源与目标使用两个互不重叠的 Codex 账号。
func newGatewayManager(t *testing.T) (*proxy.CodexHarvestManager, int64, int64) {
	t.Helper()
	previousSettings := auth.ConfiguredCodexTicketSettings()
	previousManager, previousSink := loadActiveCodexHarvestManager(), proxy.ActiveCodexTicketFeedbackForTest()
	t.Cleanup(func() {
		auth.SetConfiguredCodexTicketSettings(previousSettings)
		restoreActiveCodexHarvestManager(previousManager)
		proxy.RestoreCodexTicketFeedbackForTest(previousSink)
	})
	db, err := database.New("sqlite", t.TempDir()+"/ws-gateway.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	id, err := db.InsertAccountWithCredentials(t.Context(), "ws-gateway-source", map[string]any{"upstream_type": "codex", "access_token": "ws-token", "plan_type": "plus"}, "")
	if err != nil {
		t.Fatal(err)
	}
	targetID, err := db.InsertAccountWithCredentials(t.Context(), "ws-gateway-target", map[string]any{"upstream_type": "codex", "access_token": "ws-token-2", "plan_type": "plus"}, "")
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 4})
	t.Cleanup(store.Stop)
	if err = store.LoadAccountByID(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err = store.LoadAccountByID(t.Context(), targetID); err != nil {
		t.Fatal(err)
	}
	settings := previousSettings
	settings.Enabled = true
	settings.HarvestProxyURL = "socks5h://127.0.0.1:1080"
	settings.Models = []string{"gpt-6-astra"}
	settings.TargetLength = 780
	settings.FailClosed = true
	auth.SetConfiguredCodexTicketSettings(settings)
	return proxy.ConfigureCodexHarvest(db, store, t.TempDir()), id, targetID
}

func readAllHTTPBody(resp *http.Response) ([]byte, error) {
	if resp == nil || resp.Body == nil {
		return nil, fmt.Errorf("空响应")
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func gatewayWSBody(input, previous string) []byte {
	body := fmt.Sprintf(`{"model":"gpt-6-astra","stream":true,"input":"%s"`, input)
	if previous != "" {
		body += fmt.Sprintf(`,"previous_response_id":%q`, previous)
	}
	return []byte(body + "}")
}

func gatewayWSResponseID(body []byte) string {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		if id := gjson.Get(strings.TrimSpace(strings.TrimPrefix(line, "data:")), "response.id").String(); id != "" {
			return id
		}
	}
	return ""
}

func drainGatewayWSRequests(s *gatewayWSServer, n int) []string {
	out := []string{}
	for {
		select {
		case row := <-s.requests:
			out = append(out, row)
		case <-time.After(2 * time.Second):
			return out
		}
		if len(out) >= n {
			return out
		}
	}
}

// TestCodexGatewayWSFixedSessionSticksToOriginalConnection 验证：
//  1. 固定会话按 API Key + 账号 + 客户端会话 + 配置版本隔离；
//  2. previous_response_id 必须回到原连接；
//  3. 原连接不可用时显式失败，绝不悄悄换连接续接。
func TestCodexGatewayWSFixedSessionSticksToOriginalConnection(t *testing.T) {
	server := newGatewayWSServer(t)
	manager, sourceID, targetID := newGatewayManager(t)
	account := manager.Store.FindByID(targetID)
	if account == nil {
		t.Fatal("目标账号未进入运行时池")
	}
	v := harvest.DefaultGatewaySettings()
	v.CookiePool.SourceAccountIDs = []int64{sourceID}
	v.CookiePool.TargetAccountIDs = []int64{account.ID()}
	v.WSSession.Enabled = true
	v.WSSession.AccountIDs = []int64{account.ID()}
	if _, err := manager.SaveGatewaySettings(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	seedGatewayWSTicket(t, account)

	executor := NewExecutorWithManager(NewManager())
	t.Cleanup(executor.manager.Stop)
	previousExecute := proxy.WebsocketExecuteFunc
	t.Cleanup(func() { proxy.WebsocketExecuteFunc = previousExecute })
	proxy.WebsocketExecuteFunc = func(ctx context.Context, a *auth.Account, body []byte, session, proxyURL, key string, cfg *proxy.DeviceProfileConfig, headers http.Header, poolKey string) (*http.Response, error) {
		response, err := executor.ExecuteRequestViaWebsocket(ctx, a, body, session, proxyURL, key, cfg, headers, poolKey)
		if err != nil {
			return nil, err
		}
		return websocketResponseToHTTP(ctx, response, http.StatusOK, nil), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ctx = proxy.WithResinConfig(ctx, &proxy.ResinConfig{BaseURL: server.server.URL, PlatformName: "gateway-ws"})

	headers := http.Header{}
	headers.Set("Session_id", "ws-fixed-session")
	first, err := proxy.ExecuteRequest(ctx, account, gatewayWSBody("1", ""), "ws-fixed-session", "", "api-key-A", nil, headers, true)
	if err != nil {
		t.Fatalf("固定会话首轮失败: %v", err)
	}
	firstBody, _ := readAllHTTPBody(first)
	firstID := gatewayWSResponseID(firstBody)
	if firstID == "" {
		t.Fatalf("首轮缺少 response_id: %s", string(firstBody))
	}

	// 正确的续接必须成功并回到同一条连接。
	second, err := proxy.ExecuteRequest(ctx, account, gatewayWSBody("2", firstID), "ws-fixed-session", "", "api-key-A", nil, headers, true)
	if err != nil {
		t.Fatalf("固定会话续接失败: %v", err)
	}
	secondBody, readErr := readAllHTTPBody(second)
	if readErr != nil {
		t.Fatal(readErr)
	}
	secondID := gatewayWSResponseID(secondBody)
	if secondID == "" {
		t.Fatalf("续接响应缺少 response_id: %s", string(secondBody))
	}
	if server.dials.Load() != 1 {
		t.Fatalf("续接必须使用原连接，实际拨号 %d 次", server.dials.Load())
	}

	// 不同 API Key 或不同客户端会话不能续接别人的响应。
	for _, tc := range []struct {
		name, apiKey, session, scopeHeader string
		previous                           string
		wantFail                           bool
	}{
		{"other api key", "api-key-B", "ws-fixed-session", "ws-fixed-session", secondID, true},
		{"other client session", "api-key-A", "other-session", "other-session", secondID, true},
	} {
		otherHeaders := http.Header{}
		otherHeaders.Set("Session_id", tc.scopeHeader)
		_, err = proxy.ExecuteRequest(ctx, account, gatewayWSBody("3", tc.previous), tc.session, "", tc.apiKey, nil, otherHeaders, true)
		failed := err != nil
		if failed != tc.wantFail {
			t.Fatalf("%s: 期望失败=%v 实际 err=%v", tc.name, tc.wantFail, err)
		}
	}

	rows := drainGatewayWSRequests(server, 3)
	if len(rows) < 2 {
		t.Fatalf("上游请求数异常: %d", len(rows))
	}
	parts := strings.Split(rows[0], "|")
	continuation := ""
	for _, row := range rows[1:] {
		if strings.HasSuffix(row, "|"+firstID) {
			continuation = row
		}
	}
	if continuation == "" {
		t.Fatal("续接请求未回到原连接")
	}
	continuationParts := strings.Split(continuation, "|")
	if parts[0] != continuationParts[0] {
		t.Fatalf("续接使用了不同的路由 Cookie: %q vs %q", parts[0], continuationParts[0])
	}

	original, _ := executor.manager.lookupResponseConn(secondID, account.ID(), "api-key-A")
	server.closeConnections()
	executor.manager.DiscardConnection(original)
	_, err = proxy.ExecuteRequest(ctx, account, gatewayWSBody("after-close", secondID), "ws-fixed-session", "", "api-key-A", nil, headers, true)
	if err == nil || server.dials.Load() != 1 {
		t.Fatalf("原连接关闭后不得续接或重拨: err=%v dials=%d", err, server.dials.Load())
	}
}

// seedGatewayWSTicket 给目标账号注入一张 780 长度的有效 WebSocket 票。
// 此测试通过业务入口实际注入该完整票，不借来源 Cookie。
func seedGatewayWSTicket(t *testing.T, account *auth.Account) {
	t.Helper()
	now := time.Now()
	// 780 是编码后的目标长度：解码 585 字节 = 57 字节信封头 + 33 个 16 字节块。
	const gatewayWSBlocks = 33
	raw := make([]byte, auth.CodexTicketEnvelopeHeaderBytes+auth.CodexTicketBlockBytes*gatewayWSBlocks)
	raw[0] = 0x80
	binary.BigEndian.PutUint64(raw[1:9], uint64(now.Unix()))
	state := base64.URLEncoding.EncodeToString(raw)
	claims, _ := json.Marshal(map[string]any{"host": "chat.gateway.unified-0.api.openai.com", "exp": now.Add(time.Hour).Unix()})
	signature := base64.RawURLEncoding.EncodeToString([]byte("sig"))
	cookie := "__cflb=cflb-value; __oailb=header." + base64.RawURLEncoding.EncodeToString(claims) + "." + signature
	ticket := &auth.CodexTicket{
		Model: "gpt-6-astra", State: state, Cookie: cookie,
		Length: len(state), IssuedAt: now, CapturedAt: now,
		ExpiresAt: now.Add(30 * time.Minute), Transport: "websocket", ProbeVerified: true,
	}
	account.Mu().Lock()
	account.CodexTickets = map[string]*auth.CodexTicket{"gpt-6-astra": ticket}
	account.Mu().Unlock()
}

// loadActiveCodexHarvestManager 由 proxy 包导出给测试使用，避免测试直接操作进程级指针。
func loadActiveCodexHarvestManager() *proxy.CodexHarvestManager {
	return proxy.ActiveCodexHarvestManagerForTest()
}

func restoreActiveCodexHarvestManager(m *proxy.CodexHarvestManager) {
	proxy.SetActiveCodexHarvestManagerForTest(m)
}
