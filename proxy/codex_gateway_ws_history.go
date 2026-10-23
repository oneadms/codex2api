package proxy

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/harvest"
	"github.com/tidwall/gjson"
)

type gatewayWSObservationKey struct{}

type gatewayWSObservation struct {
	mu                                    sync.Mutex
	owner                                 *CodexHarvestManager
	started                               time.Time
	record                                harvest.GatewayHistoryRecord
	connected, completed, continued, done bool
}

// BeginCodexGatewayWSObservation 为一次目标请求建立记录，公共入口与 WS 执行器共用同一份。
func BeginCodexGatewayWSObservation(ctx context.Context, account *auth.Account, model string) context.Context {
	if account == nil {
		return ctx
	}
	model = auth.NormalizeCodexTicketModel(model)
	if current := gatewayWSObservationFromContext(ctx); current != nil && current.record.TargetAccountID == account.ID() && current.record.Model == model {
		return ctx
	}
	m := codexHarvester.Load()
	if m == nil || m.Repository == nil || !account.SupportsCodexTickets() {
		return ctx
	}
	v, err := m.GatewaySettings(ctx)
	if err != nil || !v.CookiePool.Enabled || !slices.Contains(v.CookiePool.TargetAccountIDs, account.ID()) || (model != gatewayModel && !auth.CodexTicketModelGated(model)) {
		return ctx
	}
	action := "ws_request"
	if codexSyntheticProbeFromContext(ctx) || ctx.Value(gatewayWSDiagnosticKey{}) != nil {
		action = "ws_test"
	}
	observation := &gatewayWSObservation{owner: m, started: time.Now(), record: harvest.GatewayHistoryRecord{
		TargetAccountID: account.ID(), Model: model, Transport: "websocket", Action: action,
	}}
	return context.WithValue(ctx, gatewayWSObservationKey{}, observation)
}

func gatewayWSObservationFromContext(ctx context.Context) *gatewayWSObservation {
	if ctx == nil {
		return nil
	}
	observation, _ := ctx.Value(gatewayWSObservationKey{}).(*gatewayWSObservation)
	return observation
}

// 握手成功后才记录 Cookie 已发送；固定会话续聊只标记沿用原连接。
func ObserveCodexGatewayWSHandshake(ctx context.Context, headers http.Header, continued bool) {
	o := gatewayWSObservationFromContext(ctx)
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done {
		return
	}
	o.connected, o.continued = true, continued
	bound, ok := ctx.Value(gatewayRequestKey{}).(gatewayRequest)
	if !ok || bound.accountID != o.record.TargetAccountID || bound.model != o.record.Model || CodexTurnStateInjectionFromContext(ctx) != "" {
		return
	}
	cookie, err := (&http.Request{Header: headers}).Cookie("__oailb")
	o.record.BorrowedCookieSent = !continued && err == nil && cookie.Value == bound.route.cookie && cookie.Value != ""
}

func ObserveCodexGatewayWSFrame(ctx context.Context, frame []byte) {
	o := gatewayWSObservationFromContext(ctx)
	if o == nil {
		return
	}
	event := gjson.ParseBytes(frame)
	if event.Get("type").String() != "response.completed" && event.Get("type").String() != "response.done" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.done {
		o.completed = event.Get("response.status").String() == "completed" && auth.NormalizeCodexTicketModel(event.Get("response.model").String()) == o.record.Model && !event.Get("response.error").IsObject()
	}
}

// FinishCodexGatewayWSObservation 只保存账号、模型与状态，不保存 Cookie、凭据或上游错误正文。
func FinishCodexGatewayWSObservation(ctx context.Context, failed bool) {
	o := gatewayWSObservationFromContext(ctx)
	if o == nil {
		return
	}
	o.mu.Lock()
	if o.done {
		o.mu.Unlock()
		return
	}
	o.done = true
	record := o.record
	record.CheckedAt, record.DurationMS = time.Now(), time.Since(o.started).Milliseconds()
	record.Success = o.connected && o.completed && !failed
	record.BorrowStatus = "unavailable"
	if codexTicketFromContext(ctx) != nil {
		record.BorrowStatus = "own_ticket"
	} else if CodexTurnStateInjectionFromContext(ctx) != "" {
		record.BorrowStatus = "manual_ticket"
	} else if bound, ok := ctx.Value(gatewayRequestKey{}).(gatewayRequest); ok && bound.accountID == record.TargetAccountID && bound.model == record.Model && bound.route.cookie != "" {
		record.SourceAccountID, record.Gateway, record.ProxyNode = bound.route.sourceID, bound.route.gateway, bound.route.nodeName
		record.BorrowStatus = "selected"
		if record.BorrowedCookieSent {
			record.BorrowStatus = "borrowed"
		} else if o.connected && o.continued {
			record.BorrowStatus = "continued"
		}
	}
	record.Reason = "request_failed"
	if record.Success {
		record.Reason = "success"
	}
	o.mu.Unlock()
	// 客户端取消也需要留下结果；短超时仅约束记录写入，不影响请求判定。
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	_ = o.owner.Repository.AppendGatewayHistory(writeCtx, record)
}
