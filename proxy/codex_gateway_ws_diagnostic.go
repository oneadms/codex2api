package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type gatewayWSDiagnosticKey struct{}
type gatewayWSDiagnostic struct {
	mu    sync.Mutex
	turns []*GatewayWSTurn
}

func recordGatewayWSDiagnosticTurn(ctx context.Context, turn *GatewayWSTurn) {
	if trace, _ := ctx.Value(gatewayWSDiagnosticKey{}).(*gatewayWSDiagnostic); trace != nil {
		trace.mu.Lock()
		trace.turns = append(trace.turns, turn)
		trace.mu.Unlock()
	}
}

func (t *GatewayWSTurn) discardSession() {
	t.mu.Lock()
	t.g.mu.Lock()
	if t.g.anchors[t.key] == t.anchor {
		delete(t.g.anchors, t.key)
	}
	discard := t.anchor.discard
	t.g.mu.Unlock()
	t.mu.Unlock()
	if discard != nil {
		discard()
	}
}

// TestGatewayWS validates the same ExecuteRequest -> registered WS executor
// used by business traffic. It requires two successful turns on one connection,
// passes previous_response_id explicitly, and removes its private test session.
// TestGateway's action == "ws" branch should call this, not the HTTP route probe.
func (m *CodexHarvestManager) TestGatewayWS(ctx context.Context, accountID int64) error {
	a := m.Store.FindByID(accountID)
	if !gatewayAccountEligible(a) {
		return errors.New("account_unavailable")
	}
	models := []string{gatewayModel}
	settings := auth.ConfiguredCodexTicketSettings()
	if auth.CodexTicketGateEnabled() && settings.TargetLength == 780 {
		models = settings.Models
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	for _, model := range models {
		if err := m.testGatewayWSModel(ctx, a, model); err != nil {
			return err
		}
	}
	return nil
}

// 每个配置模型独立验收两轮原连接续聊，不能用另一个模型的成功结果代替。
func (m *CodexHarvestManager) testGatewayWSModel(ctx context.Context, a *auth.Account, model string) error {
	enabled, err := CodexGatewayWSRequired(ctx, a, model)
	if err != nil {
		return err
	}
	if !enabled || WebsocketExecuteFunc == nil {
		return errors.New("websocket_disabled")
	}
	trace := &gatewayWSDiagnostic{}
	ctx = context.WithValue(ctx, gatewayWSDiagnosticKey{}, trace)
	defer func() {
		trace.mu.Lock()
		turns := append([]*GatewayWSTurn(nil), trace.turns...)
		trace.mu.Unlock()
		for _, turn := range turns {
			turn.discardSession()
		}
	}()
	session := "gateway-ws-test-" + NewUpstreamSessionUUID()
	headers := http.Header{"Session-Id": []string{session}}
	previous, connectionID := "", ""
	for round := 0; round < 2; round++ {
		body := codexTicketHarvestProbeBody(model)
		if previous != "" {
			body, _ = sjson.SetBytes(body, "previous_response_id", previous)
		}
		response, err := ExecuteRequest(ctx, a, body, session, "", session, nil, headers, true)
		if err != nil {
			return errors.New("target_probe_failed")
		}
		if response == nil || response.Body == nil {
			return errors.New("target_probe_failed")
		}
		const limit = 4 << 20
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, limit+1))
		_ = response.Body.Close()
		if readErr != nil || len(raw) > limit || response.StatusCode != http.StatusOK {
			return errors.New("target_probe_failed")
		}
		trace.mu.Lock()
		var turn *GatewayWSTurn
		if len(trace.turns) == round+1 {
			turn = trace.turns[round]
		}
		trace.mu.Unlock()
		if turn == nil {
			return errors.New("target_probe_failed") // A stub/HTTP fallback is not WS evidence.
		}
		turn.mu.Lock()
		qualified, id, connID := turn.done && turn.success, turn.responseID, turn.connectionID
		turn.mu.Unlock()
		if !qualified || id == "" || connID == "" || (round > 0 && connID != connectionID) {
			return errors.New("target_probe_failed")
		}
		text := ""
		_ = ReadSSEStream(strings.NewReader(string(raw)), func(frame []byte) bool {
			event := gjson.ParseBytes(frame)
			if event.Get("type").String() == "response.output_text.delta" {
				text += event.Get("delta").String()
			}
			if text == "" {
				for _, item := range event.Get("response.output").Array() {
					for _, content := range item.Get("content").Array() {
						if content.Get("type").String() == "output_text" {
							text += content.Get("text").String()
						}
					}
				}
			}
			return true
		})
		if strings.TrimSpace(text) == "" {
			return errors.New("target_probe_failed")
		}
		previous, connectionID = id, connID
	}
	return nil
}
