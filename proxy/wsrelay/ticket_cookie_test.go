package wsrelay

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestCodexTicketCookieWebsocketReuse(t *testing.T) {
	previousSettings := auth.ConfiguredCodexTicketSettings()
	t.Cleanup(func() { auth.SetConfiguredCodexTicketSettings(previousSettings) })
	auth.SetConfiguredCodexTicketSettings(auth.CodexTicketSettings{
		Enabled: true, HarvestProxyURL: "socks5h://127.0.0.1:1080", Models: []string{"gpt-5.5"}, FailClosed: true,
	})
	t.Setenv("CODEX_WS_STATELESS_ONESHOT", "0")
	for _, route := range []string{"session", "stateless", "previous_response_id"} {
		t.Run(route, func(t *testing.T) {
			type frame struct {
				cookie, state, responseID string
			}
			frames := make(chan frame, 4)
			upgrader := websocket.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				for {
					_, payload, err := conn.ReadMessage()
					if err != nil {
						return
					}
					id := "resp_" + gjson.GetBytes(payload, "input").String()
					frames <- frame{r.Header.Get("Cookie"), gjson.GetBytes(payload, "client_metadata.x-codex-turn-state").String(), id}
					if err := conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"status":"completed","output":[]}}`, id))); err != nil {
						return
					}
				}
			}))
			t.Cleanup(server.Close)
			manager := NewManager()
			t.Cleanup(manager.Stop)
			executor := NewExecutorWithManager(manager)
			previousExecute := proxy.WebsocketExecuteFunc
			t.Cleanup(func() { proxy.WebsocketExecuteFunc = previousExecute })
			var actualConnection *WsConnection
			proxy.WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, body []byte, session, proxyURL, key string, cfg *proxy.DeviceProfileConfig, headers http.Header, poolKey string) (*http.Response, error) {
				response, err := executor.ExecuteRequestViaWebsocket(ctx, account, body, session, proxyURL, key, cfg, headers, poolKey)
				if err != nil {
					return nil, err
				}
				actualConnection = response.conn
				return websocketResponseToHTTP(ctx, response, http.StatusOK, nil), nil
			}
			account := &auth.Account{DBID: 720, AccessToken: "test-token", PlanType: "plus", DynamicConcurrencyLimit: 1, CustomHeaders: map[string]string{"Cookie": "custom=wrong"}}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ctx = proxy.WithResinConfig(ctx, &proxy.ResinConfig{BaseURL: server.URL, PlatformName: "cookie-test"})
			var previousConnection *WsConnection
			previousID := ""
			for i, cookie := range []string{"ticket=first", "ticket=first", "ticket=second"} {
				issued := time.Now().Add(time.Duration(i) * time.Second)
				raw := make([]byte, auth.CodexTicketEnvelopeHeaderBytes+auth.CodexTicketBlockBytes*auth.CodexTicketPersonalBlocks)
				raw[0] = 0x80
				binary.BigEndian.PutUint64(raw[1:9], uint64(issued.Unix()))
				state := base64.URLEncoding.EncodeToString(raw)
				account.Mu().Lock()
				account.CodexTickets = map[string]*auth.CodexTicket{"gpt-5.5": {Model: "gpt-5.5", State: state, Cookie: cookie, Length: len(state), ExpiresAt: issued.Add(time.Minute)}}
				account.Mu().Unlock()
				session := ""
				if route == "session" {
					session = "cookie-session"
				}
				body := []byte(fmt.Sprintf(`{"model":"gpt-5.5","input":"%d","previous_response_id":%q}`, i, previousID))
				response, err := proxy.ExecuteRequest(ctx, account, body, session, "", "cookie-key", nil, nil, true)
				if err != nil {
					t.Fatal(err)
				}
				_, readErr := io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if readErr != nil {
					t.Fatal(readErr)
				}
				select {
				case got := <-frames:
					if got.cookie != cookie || got.state != state {
						t.Fatal("WebSocket handshake cookie and per-turn state lost their pairing")
					}
					if route == "previous_response_id" {
						previousID = got.responseID
					}
				case <-ctx.Done():
					t.Fatal("upstream did not receive the frame")
				}
				if i > 0 && (actualConnection == previousConnection) != (i == 1) {
					t.Fatal("same cookies should reuse connections; rotated cookies must reconnect")
				}
				if strings.Contains(actualConnection.PoolKey, cookie) {
					t.Fatal("connection pool keys must not contain raw cookies")
				}
				previousConnection = actualConnection
			}
		})
	}
}
