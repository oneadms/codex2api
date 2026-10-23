package wsrelay

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/harvest"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestGatewayWSBorrowCookieWithMissingTargetTicket(t *testing.T) {
	for _, model := range []string{"gpt-6-astra", "gpt-6.1-sol", "gpt-6-sol"} {
		for _, mode := range []string{"ordinary", "fixed", "node-fixed", "connection-test", "connection-test-skipped", "verify-target", "diagnostic", "upstream-error"} {
			t.Run(model+"/"+mode, func(t *testing.T) {
				manager, sourceID, targetID := newGatewayManager(t)
				policy := auth.ConfiguredCodexTicketSettings()
				policy.Models = []string{model}
				auth.SetConfiguredCodexTicketSettings(policy)
				requestBody := func(input, previous string) []byte {
					body, _ := json.Marshal(map[string]any{"model": model, "stream": true, "input": input, "previous_response_id": previous})
					return body
				}
				source, target := manager.Store.FindByID(sourceID), manager.Store.FindByID(targetID)
				target.Mu().Lock()
				target.PlanType, target.AccountID = "team5x", "target-workspace"
				target.Mu().Unlock()
				if mode == "connection-test-skipped" {
					scope := harvest.DefaultScope()
					scope.SkippedAccountIDs = []int64{targetID}
					if err := manager.SaveScope(t.Context(), scope); err != nil {
						t.Fatal(err)
					}
				}
				seedGatewayWSTicket(t, source)
				cookie, _ := (&http.Request{Header: http.Header{"Cookie": {source.CodexTicketSnapshot("gpt-6-astra").Cookie}}}).Cookie("__oailb")
				var expectedCookie atomic.Value
				expectedCookie.Store(cookie.Value)
				var dials, requests atomic.Int32
				upgrader := websocket.Upgrader{}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer ws-token-2" || r.Header.Get("Chatgpt-Account-Id") != "target-workspace" {
						t.Error("借路由必须保留目标账号自己的鉴权和工作区")
					}
					if r.Header.Get("Cookie") != "__oailb="+expectedCookie.Load().(string) || r.Header.Get("X-Codex-Turn-State") != "" {
						t.Error("握手只能借来源路由 Cookie，不能携带来源完整票或其他 Cookie")
					}
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer conn.Close()
					dials.Add(1)
					previous := ""
					for {
						_, body, err := conn.ReadMessage()
						if err != nil {
							return
						}
						if gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String() != "" {
							t.Error("来源 turn state 不能写入目标请求帧")
						}
						if gjson.GetBytes(body, "model").String() != model {
							t.Error("借路由不能修改目标请求模型")
						}
						if got := gjson.GetBytes(body, "previous_response_id").String(); got != "" && got != previous {
							t.Error("续聊没有回到原连接")
						}
						previous = fmt.Sprintf("borrow_%d", requests.Add(1))
						if mode == "upstream-error" {
							_ = conn.WriteJSON(map[string]any{"type": "response.failed", "response": map[string]any{"model": model, "status": "failed", "error": map[string]any{"code": "server_error", "message": "private-upstream-error"}}})
							continue
						}
						_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{
							"id": previous, "model": model, "status": "completed", "error": nil,
							"output": []any{map[string]any{"content": []any{map[string]any{"type": "output_text", "text": "OK"}}}},
						}})
					}
				}))
				t.Cleanup(server.Close)
				localProxy := gatewayOfflineProxy(t, server.Listener.Addr().String())
				source.Mu().Lock()
				source.CodexTickets["gpt-6-astra"].HarvestProxyURL = localProxy.URL
				source.Mu().Unlock()
				var nodeSelections *atomic.Int32
				if mode == "node-fixed" {
					nodeSelections = bindOfflineNodeTicket(t, manager, source, localProxy.URL)
				}
				source.Mu().Lock()
				ticket := source.CodexTickets["gpt-6-astra"]
				ticket.Model = model
				source.CodexTickets = map[string]*auth.CodexTicket{model: ticket}
				source.Mu().Unlock()
				v := harvest.DefaultGatewaySettings()
				v.CookiePool.Enabled = true
				v.CookiePool.SourceAccountIDs, v.CookiePool.TargetAccountIDs = []int64{sourceID}, []int64{targetID}
				v.WSSession.Enabled = mode != "ordinary" && mode != "verify-target" && mode != "upstream-error"
				v.WSSession.AccountIDs = []int64{targetID}
				v.Revision, v.RouteRevision = "borrow-test", "borrow-test"
				raw, _ := json.Marshal(v)
				if err := manager.Repository.Set(t.Context(), harvest.GatewaySettingsKey, string(raw)); err != nil {
					t.Fatal(err)
				}
				controls, _, err := manager.Controls.Controls(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				controls.Transport = "websocket"
				if err = manager.Controls.SaveControls(t.Context(), controls); err != nil {
					t.Fatal(err)
				}
				executor := NewExecutorWithManager(NewManager())
				t.Cleanup(executor.manager.Stop)
				expected, _ := url.Parse(localProxy.URL)
				executor.manager.dialer.NetDialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
					if address != expected.Host {
						return nil, fmt.Errorf("未沿用来源出口: %s", address)
					}
					return (&net.Dialer{}).DialContext(ctx, network, address)
				}
				previousExecute := proxy.WebsocketExecuteFunc
				t.Cleanup(func() { proxy.WebsocketExecuteFunc = previousExecute })
				proxy.WebsocketExecuteFunc = func(ctx context.Context, a *auth.Account, body []byte, session, proxyURL, key string, cfg *proxy.DeviceProfileConfig, headers http.Header, poolKey string) (*http.Response, error) {
					response, err := executor.ExecuteRequestViaWebsocket(ctx, a, body, session, proxyURL, key, cfg, headers, poolKey)
					if err != nil {
						return nil, err
					}
					return websocketResponseToHTTP(ctx, response, 200, nil), nil
				}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				ctx = proxy.WithResinConfig(ctx, &proxy.ResinConfig{BaseURL: "http://127.0.0.1:1", PlatformName: "must-not-override-source"})
				// 逐次核对历史，握手借用与上游请求结果必须分别保留。
				assertHistory := func(statuses ...string) {
					t.Helper()
					page, err := manager.Repository.GatewayHistory(t.Context(), 0, 25)
					if err != nil {
						t.Fatal(err)
					}
					var records []harvest.GatewayHistoryRecord
					for _, record := range page.Items {
						if record.Action == "ws_request" || record.Action == "ws_test" {
							records = append(records, record)
						}
					}
					if len(records) != len(statuses) {
						t.Fatalf("记录数不符: got %d want %d: %+v", len(records), len(statuses), records)
					}
					for i, status := range statuses {
						record := records[i]
						if record.BorrowStatus != status || record.Model != model || record.Transport != "websocket" || record.TargetAccountID != targetID || record.BorrowedCookieSent != (status == "borrowed") {
							t.Fatalf("借用记录与真实请求不符: %+v", record)
						}
						if status != "unavailable" && record.SourceAccountID != sourceID {
							t.Fatalf("来源账号不符: %+v", record)
						}
						if record.Success != (status != "unavailable" && mode != "upstream-error") {
							t.Fatalf("请求结果不符: %+v", record)
						}
						if strings.HasPrefix(mode, "connection-test") || mode == "diagnostic" || mode == "verify-target" {
							if record.Action != "ws_test" {
								t.Fatalf("测试入口未标记为 WS 测试: %+v", record)
							}
						}
					}
					raw, _ := json.Marshal(page)
					for _, secret := range []string{cookie.Value, "ws-token", "private-upstream-error"} {
						if strings.Contains(string(raw), secret) {
							t.Fatal("历史不得保存 Cookie 值、登录凭据或上游错误正文")
						}
					}
				}
				if mode == "verify-target" {
					result, err := manager.TestGateway(ctx, "verify", targetID)
					if err != nil || !result.Success || dials.Load() != 1 || requests.Load() != 1 {
						t.Fatalf("不开固定会话也应完成 WS 目标验证: %+v %v", result, err)
					}
					assertHistory("borrowed")
					return
				}
				if mode == "diagnostic" {
					if err := manager.TestGatewayWS(ctx, target.ID()); err != nil {
						t.Fatalf("%s 借路由诊断失败: %v", model, err)
					}
					if dials.Load() != 1 || requests.Load() != 2 || target.CodexTicketSnapshot(model) != nil {
						t.Fatal("诊断必须借路由在同一连接完成两轮，不生成目标完整票")
					}
					assertHistory("continued", "borrowed")
					return
				}
				headers, key := http.Header{"Session-Id": {"borrow-session"}}, "borrow-key"
				if strings.HasPrefix(mode, "connection-test") {
					ctx, headers, key = proxy.WithCodexSyntheticProbe(ctx), nil, ""
				}
				first, err := proxy.ExecuteRequest(ctx, target, requestBody("first", ""), "", "", key, nil, headers, true)
				if err != nil {
					t.Fatalf("目标无票时未借用 WS 路由: %v", err)
				}
				body, err := readAllHTTPBody(first)
				if mode == "upstream-error" {
					assertHistory("borrowed")
					return
				}
				if err != nil || gatewayWSResponseID(body) == "" {
					t.Fatalf("借路由响应失败: %v %s", err, body)
				}
				if mode == "fixed" || mode == "node-fixed" {
					// 来源票消失后，已建立的固定会话仍须在原连接续聊，不能改借另一条路由。
					source.Mu().Lock()
					source.CodexTickets = nil
					source.Mu().Unlock()
					second, err := proxy.ExecuteRequest(ctx, target, requestBody("next", gatewayWSResponseID(body)), "", "", key, nil, headers, true)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = readAllHTTPBody(second); err != nil || dials.Load() != 1 || requests.Load() != 2 {
						t.Fatalf("借路由固定会话续聊失败: %v, dials=%d", err, dials.Load())
					}
					if nodeSelections != nil && nodeSelections.Load() != 1 {
						t.Fatalf("建立连接时未锁定来源节点: %d", nodeSelections.Load())
					}
					assertHistory("continued", "borrowed")
				} else {
					assertHistory("borrowed")
				}
				if target.CodexTicketSnapshot(model) != nil {
					t.Fatal("借路由不能把来源完整票保存成目标票")
				}
				if mode == "ordinary" {
					// 来源路由变化后必须重新握手，不能复用冻结了旧 Cookie 的池化连接。
					nextCookie := "next." + strings.SplitN(cookie.Value, ".", 2)[1]
					expectedCookie.Store(nextCookie)
					source.Mu().Lock()
					source.CodexTickets[model].Cookie = "__cflb=next; __oailb=" + nextCookie
					source.Mu().Unlock()
					second, err := proxy.ExecuteRequest(ctx, target, requestBody("new-route", ""), "", "", key, nil, headers, true)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = readAllHTTPBody(second); err != nil || dials.Load() != 2 || requests.Load() != 2 {
						t.Fatalf("普通 WS 没有更新握手 Cookie: %v, dials=%d", err, dials.Load())
					}
					// 来源票被撤销后，新请求必须恢复缺票拦截。
					source.Mu().Lock()
					source.CodexTickets = nil
					source.Mu().Unlock()
					_, err = proxy.ExecuteRequest(ctx, target, requestBody("missing", ""), "", "", key, nil, headers, true)
					if err == nil || !strings.Contains(err.Error(), "codex_ticket_unavailable") || requests.Load() != 2 {
						t.Fatalf("没有来源时应继续缺票拦截: %v", err)
					}
					assertHistory("unavailable", "borrowed", "borrowed")
				}
			})
		}
	}
}
