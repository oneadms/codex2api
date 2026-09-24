package proxy

import (
	"context"
	"net/http"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/harvest"
	"github.com/codex2api/internal/mihomo"
)

type harvestSelection struct {
	sidecar    *mihomo.DirectedSidecar
	node       mihomo.HarvestNode
	scope      harvest.CodexHarvestNodeScope
	generation int64
}

func (m *CodexHarvestManager) selectNode(ctx context.Context, account *auth.Account, model, proxyURL string, controls harvest.CodexHarvestControls, tried map[string]bool) (*harvestSelection, string) {
	if !controls.NodeMemoryEnabled {
		return nil, "rotation"
	}
	sidecar, err := mihomo.LoadDirectedSidecar(m.DataDir, proxyURL)
	if err != nil {
		return nil, "rotation_unavailable"
	}
	nodes, err := sidecar.Directory(ctx)
	if err != nil {
		return nil, "rotation_unavailable"
	}
	account.Mu().RLock()
	email := account.Email
	account.Mu().RUnlock()
	scope := harvest.CodexHarvestNodeScope{PoolID: sidecar.PoolID, AccountID: account.ID(), Identity: auth.CodexTicketIdentity(account.EffectiveAccountID(), email), Model: model, Blocks: auth.CodexTicketExpectedBlocks(account.GetPlanType())}
	generation, records, err := m.Repository.Snapshot(ctx, scope)
	if err != nil {
		return nil, "learning_store_unavailable"
	}
	ranked := m.Controls.Rank(nodes, records, tried)
	if len(ranked) == 0 {
		return nil, "nodes_cooling_down"
	}
	selected := ranked[0]
	reason := "explore"
	for _, record := range records {
		if record.NodeID == selected.ID && record.LastSuccess != nil {
			reason = "recent_success"
			break
		}
	}
	if ticket := account.CodexTicketForModel(model, time.Now(), 0); ticket != nil && ticket.HarvestNodeID != "" {
		for _, node := range ranked {
			if node.ID == ticket.HarvestNodeID {
				selected = node
				reason = "ticket_sticky"
				break
			}
		}
	}
	return &harvestSelection{sidecar: sidecar, node: selected, scope: scope, generation: generation}, reason
}

func (m *CodexHarvestManager) attempt(ctx context.Context, account *auth.Account, model, proxyURL string, controls harvest.CodexHarvestControls, budget *int, tried map[string]bool, keep **harvestSelection, source, jobID string, number int) (event harvest.Event) {
	started := time.Now()
	event = harvest.Event{AccountID: account.ID(), Model: model, Source: source, JobID: jobID, Attempt: number, Stage: "probe", ExpectedLength: auth.CodexTicketExpectedLength(auth.CodexTicketExpectedBlocks(account.GetPlanType())), CreatedAt: started.UTC()}
	defer func() { event.LatencyMS = time.Since(started).Milliseconds(); m.appendEvent(event) }()
	scope, err := m.Scope(ctx)
	if err != nil || !auth.CodexTicketGateEnabled() || !harvestAccountEligible(account, scope, true) {
		event.Result = "skipped"
		event.Message = "账号忙碌、采票范围变化或采票已关闭"
		return
	}
	selection, reason := (*harvestSelection)(nil), "rotation"
	if keep != nil && *keep != nil {
		selection = *keep
		reason = "manual_sticky"
	} else {
		selection, reason = m.selectNode(ctx, account, model, proxyURL, controls, tried)
	}
	event.SelectionReason = reason
	m.Controls.SetRuntime(func(r *harvest.CodexHarvestRuntime) {
		r.SelectionReason = reason
		if reason == "rotation_unavailable" {
			r.DegradedReason = "定向节点不可用，已按代理轮换；出口 IP 不保证固定"
		}
	})
	if reason == "nodes_cooling_down" || reason == "learning_store_unavailable" {
		event.Result = "skipped"
		event.Message = "节点处于冷却或学习记录暂不可用"
		return
	}
	if selection != nil {
		event.NodeID, event.NodeName = selection.node.ID, selection.node.Name
		m.Controls.SetRuntime(func(r *harvest.CodexHarvestRuntime) { r.CurrentNode = selection.node.Name })
		release, acquireErr := selection.sidecar.Acquire(ctx, selection.node)
		if acquireErr != nil {
			event.Result = "network_error"
			event.Message = "定向节点锁定失败"
			return
		}
		defer release()
		proxyURL = selection.sidecar.ProxyURL
		tried[selection.node.ID] = true
		if keep != nil {
			*keep = selection
		}
	}
	ctx = context.WithValue(ctx, harvestPacerKey{}, func(wait context.Context) error {
		m.paceMu.Lock()
		defer m.paceMu.Unlock()
		remaining := time.Until(m.lastRequest.Add(time.Duration(controls.Speed.ProbeIntervalSeconds) * time.Second))
		if remaining > 0 && !harvestWait(wait, remaining) {
			return wait.Err()
		}
		if wait.Err() != nil {
			return wait.Err()
		}
		m.lastRequest = time.Now()
		return nil
	})
	res := probeCodexTicketWithRefresh(ctx, m.Store, account, model, proxyURL, time.Duration(controls.Speed.AttemptTimeoutSeconds)*time.Second, budget)
	event.HTTPStatus, event.Length = res.HTTPStatus, len(res.State)
	if res.ObservedLength > 0 {
		event.Length = res.ObservedLength
	}
	event.Result = res.Result
	if ctx.Err() != nil {
		event.Result = "cancelled"
		event.Message = "任务已停止"
		return
	}
	if res.Result == "success" {
		if selection != nil {
			if err = selection.sidecar.Confirm(ctx, selection.node); err != nil {
				res.Result = "error"
				event.Result = "network_error"
			} else {
				res.Binding.HarvestNodeID, res.Binding.HarvestNodeName, res.Binding.HarvestPoolID = selection.node.ID, selection.node.Name, selection.sidecar.PoolID
			}
		}
		if res.Result == "success" && !publishCodexTicket(m.DB, m.Store, account, model, res.State, codexTicketIssuedAt(res.State), res.Cookies, res.Binding) {
			event.Result = "persistence_error"
		}
	}
	if res.HTTPStatus == http.StatusUnauthorized || res.HTTPStatus == http.StatusForbidden || res.HTTPStatus == http.StatusTooManyRequests || res.Result == "token_error" {
		event.Result = "account_error"
	}
	if res.HTTPStatus == 0 && event.Result == "error" {
		event.Result = "network_error"
	}
	event.Message = harvestResultMessage(event.Result)
	if event.Result == "success" {
		event.Stage = "stored"
		event.CookieCount = (&auth.CodexTicket{Cookie: res.Cookies.Header}).CookieCount()
	}
	summary := &auth.CodexTicketProbeSummary{Result: event.Result, HTTPStatus: res.HTTPStatus, CheckedAt: started}
	if event.Result != "success" {
		next := time.Now().Add(time.Duration(controls.Speed.CooldownSeconds) * time.Second)
		summary.NextProbeAt = &next
	}
	m.Store.PublishCodexTicketProbe(account.ID(), model, summary)
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 3*time.Second)
	_ = m.DB.UpdateCredentials(probeCtx, account.ID(), map[string]interface{}{auth.CodexTicketProbeCredentialKey(model): summary})
	probeCancel()
	if selection != nil && event.Result != "persistence_error" {
		query, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = m.Repository.Record(query, harvest.CodexHarvestNodeFeedback{Scope: selection.scope, Node: selection.node, Generation: selection.generation, Result: event.Result, LatencyMS: time.Since(started).Milliseconds(), CooldownSeconds: controls.Speed.CooldownSeconds})
	}
	return
}

type harvestPacerKey struct{}

func waitCodexHarvestRequest(ctx context.Context) error {
	if pace, ok := ctx.Value(harvestPacerKey{}).(func(context.Context) error); ok {
		return pace(ctx)
	}
	return ctx.Err()
}

func harvestResultMessage(result string) string {
	switch result {
	case "success":
		return "合格票据及 Cookie 已持久化"
	case "invalid_state":
		return "探测回答（Gemini 版本号须大于 2.5）、票据形状、时效或 Cookie 不合格"
	case "account_error":
		return "上游鉴权或限流，账号进入采票冷却"
	case "network_error":
		return "代理、节点或 WebSocket 连接失败"
	case "persistence_error":
		return "票据落库失败，未发布"
	case "response_incomplete_or_error":
		return "响应未达到成功终态"
	default:
		return "采票未成功"
	}
}
