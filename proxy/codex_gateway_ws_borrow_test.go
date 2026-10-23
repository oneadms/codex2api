package proxy

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/harvest"
)

func TestGatewayWSStoredBorrowRequirements(t *testing.T) {
	m, source, target, settings := newGatewayRegressionFixture(t)
	policy := auth.ConfiguredCodexTicketSettings()
	policy.TargetLength = 780
	auth.SetConfiguredCodexTicketSettings(policy)
	controls, _, _ := m.Controls.Controls(t.Context())
	controls.Transport = "websocket"
	if err := m.Controls.SaveControls(t.Context(), controls); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, now.Add(time.Hour).Unix())))
	state := testTicketState(now, 33)
	base := auth.CodexTicket{Model: gatewayModel, State: state, Length: len(state),
		Cookie: "__cflb=source; __oailb=h." + claims + ".s", CookieCapturedAt: now,
		IssuedAt: now, CapturedAt: now, ExpiresAt: now.Add(2 * time.Minute),
		Identity: gatewayIdentity(source), ProbeVerified: true, Transport: "websocket", HarvestProxyURL: "http://127.0.0.1:1"}
	for _, model := range []string{gatewayModel, "gpt-6.1-sol", "gpt-6-sol"} {
		// 单独配置其他模型，避免仍然依赖 Astra 才能进入统一借票路径。
		policy.Models = []string{model}
		auth.SetConfiguredCodexTicketSettings(policy)
		base.Model = model
		body := []byte(fmt.Sprintf(`{"model":%q}`, model))
		for _, tc := range []struct {
			name   string
			change func(*auth.CodexTicket, *harvest.GatewaySettings)
			want   bool
		}{
			{"valid", func(*auth.CodexTicket, *harvest.GatewaySettings) {}, true},
			{"minimum_cookie_ttl", func(_ *auth.CodexTicket, v *harvest.GatewaySettings) { v.CookiePool.TTLSeconds = 30 }, true},
			{"sse_source", func(t *auth.CodexTicket, _ *harvest.GatewaySettings) { t.Transport = "sse" }, false},
			{"other_model", func(t *auth.CodexTicket, _ *harvest.GatewaySettings) { t.Model = "another-model" }, false},
			{"unverified", func(t *auth.CodexTicket, _ *harvest.GatewaySettings) { t.ProbeVerified = false }, false},
			{"expired", func(t *auth.CodexTicket, _ *harvest.GatewaySettings) { t.ExpiresAt = now.Add(-time.Second) }, false},
			{"expiry_margin", func(t *auth.CodexTicket, _ *harvest.GatewaySettings) { t.ExpiresAt = now.Add(20 * time.Second) }, false},
			{"old_cookie", func(t *auth.CodexTicket, _ *harvest.GatewaySettings) {
				t.CookieCapturedAt = now.Add(-241 * time.Second)
			}, false},
			{"cookie_ttl", func(t *auth.CodexTicket, v *harvest.GatewaySettings) {
				t.CookieCapturedAt = now.Add(-61 * time.Second)
				v.CookiePool.TTLSeconds = 60
			}, false},
			{"identity_changed", func(t *auth.CodexTicket, _ *harvest.GatewaySettings) { t.Identity = "old-workspace" }, false},
			{"missing_exit", func(t *auth.CodexTicket, _ *harvest.GatewaySettings) { t.HarvestProxyURL = "" }, false},
			{"missing_node", func(t *auth.CodexTicket, _ *harvest.GatewaySettings) {
				t.HarvestNodeID, t.HarvestPoolID = "removed", "removed"
			}, false},
			{"disabled", func(_ *auth.CodexTicket, v *harvest.GatewaySettings) { v.CookiePool.Enabled = false }, false},
			{"unselected_target", func(_ *auth.CodexTicket, v *harvest.GatewaySettings) {
				v.CookiePool.TargetAccountIDs = []int64{target.ID() + 100}
			}, false},
		} {
			t.Run(model+"/"+tc.name, func(t *testing.T) {
				ticket, v := base, settings
				tc.change(&ticket, &v)
				source.Mu().Lock()
				source.CodexTickets = map[string]*auth.CodexTicket{model: &ticket}
				source.Mu().Unlock()
				seedGatewayRegressionSettings(t, m, v)
				ctx, _, release, err := PrepareCodexGatewayWebsocket(t.Context(), target, body, "http://target.invalid:1")
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				headers := make(http.Header)
				ApplyCodexGatewayCookie(ctx, headers)
				_, blocked := CodexTicketGateBlocked(ctx, target, "", model)
				if blocked == tc.want || (headers.Get("Cookie") != "") != tc.want {
					t.Fatalf("借用判定错误: blocked=%t cookie=%t", blocked, headers.Get("Cookie") != "")
				}
				if codexTicketFromContext(ctx) != nil || CodexTurnStateInjectionFromContext(ctx) != "" {
					t.Fatal("来源完整票泄漏到目标上下文")
				}
				if !tc.want {
					return
				}
				// 已作出不注入决定时，来源账号不能借用目标专属的路由豁免。
				prepared := withCodexTurnStateInjection(ctx, "")
				if _, blocked := CodexTicketGateBlocked(prepared, source, "", model); !blocked {
					t.Fatal("路由豁免错误地跨账号生效")
				}
				otherPolicy := policy
				otherPolicy.Models = []string{model, "another-model"}
				auth.SetConfiguredCodexTicketSettings(otherPolicy)
				_, blocked = CodexTicketGateBlocked(prepared, target, "", "another-model")
				auth.SetConfiguredCodexTicketSettings(policy)
				if !blocked {
					t.Fatal("路由豁免错误地跨模型生效")
				}
				result, err := m.TestGateway(t.Context(), "prepare", 0)
				if err != nil || !result.Success {
					t.Fatalf("显式准备未识别已验收 WS 来源票: %+v %v", result, err)
				}
				httpCtx, _, finish, err := prepareCodexGatewayHTTP(t.Context(), target, body, "")
				defer finish()
				if err != nil {
					t.Fatal(err)
				}
				if _, blocked := CodexTicketGateBlocked(httpCtx, target, "", model); !blocked {
					t.Fatal("WS 借路由不应放宽 SSE 的完整票要求")
				}
			})
		}
	}
}

func TestGatewayWSBorrowSeparatesModelSessions(t *testing.T) {
	m, source, target, settings := newGatewayRegressionFixture(t)
	policy := auth.ConfiguredCodexTicketSettings()
	policy.TargetLength, policy.Models = 780, []string{gatewayModel, "gpt-6.1-sol"}
	auth.SetConfiguredCodexTicketSettings(policy)
	controls, _, _ := m.Controls.Controls(t.Context())
	controls.Transport = "websocket"
	if err := m.Controls.SaveControls(t.Context(), controls); err != nil {
		t.Fatal(err)
	}
	settings.WSSession.Enabled, settings.WSSession.AccountIDs = true, []int64{target.ID()}
	seedGatewayRegressionSettings(t, m, settings)
	now := time.Now()
	claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, now.Add(time.Hour).Unix())))
	state := testTicketState(now, 33)
	identity := gatewayIdentity(source)
	source.Mu().Lock()
	source.CodexTickets = make(map[string]*auth.CodexTicket)
	for i, model := range policy.Models {
		source.CodexTickets[model] = &auth.CodexTicket{Model: model, State: state, Length: len(state),
			Cookie: fmt.Sprintf("__cflb=source; __oailb=h.%s.s%d", claims, i), CookieCapturedAt: now,
			IssuedAt: now, CapturedAt: now, ExpiresAt: now.Add(2 * time.Minute), Identity: identity,
			ProbeVerified: true, Transport: "websocket", HarvestProxyURL: "http://127.0.0.1:1"}
	}
	source.Mu().Unlock()
	if result, err := m.TestGateway(t.Context(), "prepare", 0); err != nil || !result.Success {
		t.Fatalf("多模型来源已具备有效票，准备仍失败: %+v %v", result, err)
	}
	if runtime := m.GatewaySnapshot(t.Context()); runtime.Mode != "unified_ticket" || runtime.Sources[0].State != "candidate" {
		t.Fatalf("来源汇总未识别多模型票: %+v", runtime.Sources)
	}
	headers := http.Header{"Session-Id": {"same-client-session"}}
	turns := make([]*GatewayWSTurn, 0, len(policy.Models))
	for i, model := range policy.Models {
		body := []byte(fmt.Sprintf(`{"model":%q}`, model))
		turn, err := BeginCodexGatewayWSTurn(t.Context(), target, body, headers, "same-key", "")
		if err != nil || turn == nil {
			t.Fatalf("%s 未建立固定会话: %v", model, err)
		}
		t.Cleanup(func() { turn.discardSession() })
		turns = append(turns, turn)
		if turn.anchor.cookie != fmt.Sprintf("h.%s.s%d", claims, i) {
			t.Fatal("固定会话借用了其他模型的 Cookie")
		}
		if _, blocked := CodexTicketGateBlocked(turn.Context(t.Context()), target, "", model); blocked {
			t.Fatalf("%s 已借到同模型路由，仍被缺票拦截", model)
		}
		if err = turn.BindConnection(model); err != nil {
			t.Fatal(err)
		}
		turn.Observe([]byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"model":%q,"status":"completed"}}`, "response-"+model, model)))
		if !turn.Finish(false) {
			t.Fatalf("%s 成功响应未保留会话", model)
		}
	}
	if turns[0].PoolKey() == turns[1].PoolKey() {
		t.Fatal("不同模型共享了固定连接键")
	}
	wrong := []byte(fmt.Sprintf(`{"model":"gpt-6.1-sol","previous_response_id":%q}`, "response-"+gatewayModel))
	if turn, err := BeginCodexGatewayWSTurn(t.Context(), target, wrong, headers, "same-key", ""); err == nil || turn != nil {
		t.Fatal("允许跨模型续接响应")
	}
	for _, model := range policy.Models {
		body := []byte(fmt.Sprintf(`{"model":%q,"previous_response_id":%q}`, model, "response-"+model))
		turn, err := BeginCodexGatewayWSTurn(t.Context(), target, body, headers, "same-key", "")
		if err != nil || turn == nil {
			t.Fatalf("切换模型后无法回到 %s 的原会话: %v", model, err)
		}
		if err = turn.BindConnection(model); err != nil {
			t.Fatal(err)
		}
		turn.Observe([]byte(`{"type":"response.completed","response":{"id":"wrong-model","model":"another-model","status":"completed"}}`))
		if turn.CanRetain() {
			t.Fatal("另一个模型的响应被错误验收")
		}
		turn.Finish(true)
	}
	source.Mu().Lock()
	delete(source.CodexTickets, "gpt-6.1-sol")
	source.Mu().Unlock()
	if runtime := m.GatewaySnapshot(t.Context()); runtime.Sources[0].State != "waiting" {
		t.Fatal("缺少一个模型的来源票时，汇总仍显示全部就绪")
	}
}
