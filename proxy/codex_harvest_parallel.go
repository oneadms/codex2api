package proxy

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/harvest"
	"github.com/codex2api/internal/mihomo"
)

// 所有通道和模型共用实际请求预算，续接和失败请求同样计数。
func reserveHarvestRequest(used *atomic.Int64, limit int) bool {
	return reserveHarvestBudget(used, limit, 1)
}

func reserveHarvestBudget(used *atomic.Int64, limit, cost int) bool {
	for {
		n := used.Load()
		if n+int64(cost) > int64(limit) {
			return false
		}
		if used.CompareAndSwap(n, n+int64(cost)) {
			return true
		}
	}
}

func (m *CodexHarvestManager) runParallel(ctx context.Context, account *auth.Account, job harvest.ManualJob) {
	r := job.Request
	emit := func(event harvest.Event) {
		m.appendEvent(event)
		m.jobsMu.Lock()
		m.jobs[job.ID].LastEvent = &event
		m.jobsMu.Unlock()
	}
	baseEvent := func(model string) harvest.Event {
		return harvest.Event{AccountID: account.ID(), Model: model, Source: "manual", JobID: job.ID, Stage: "probe", ExpectedLength: auth.CodexTicketTargetLength(account.GetPlanType()), CreatedAt: time.Now().UTC()}
	}
	if err := m.Store.EnsureCodexTicketAccessToken(ctx, account, false); err != nil {
		event := baseEvent(r.Models[0])
		event.Result, event.Message = "account_error", "账号凭据不可用"
		emit(event)
		return
	}
	cost := 1
	if auth.CodexTicketTargetLength(account.GetPlanType()) == 780 {
		cost = 2
	}
	collection, err := mihomo.BeginCollection(ctx, auth.ConfiguredCodexTicketSettings().HarvestProxyURL, min(r.CollectLanes, max(1, r.MaxAttempts/cost)))
	if err != nil {
		event := baseEvent(r.Models[0])
		event.Result, event.Message = "network_error", err.Error()
		emit(event)
		return
	}
	defer collection.Close()
	var used atomic.Int64
	var reserved atomic.Int64
	for _, model := range r.Models {
		if ctx.Err() != nil || used.Load() >= int64(r.MaxAttempts) || !auth.CodexTicketGateEnabled() {
			return
		}
		controls, _, _ := m.Controls.Controls(ctx)
		modelCtx, cancel := context.WithCancel(ctx)
		type outcome struct {
			event  harvest.Event
			result codexTicketHarvestResult
			node   mihomo.HarvestNode
		}
		results := make(chan outcome, collection.LaneCount())
		var workers sync.WaitGroup
		for lane := 0; lane < collection.LaneCount(); lane++ {
			workers.Add(1)
			go func(lane int) {
				defer workers.Done()
				var last time.Time
				for modelCtx.Err() == nil && used.Load() < int64(r.MaxAttempts) {
					if account.GetActiveRequests() > 0 {
						if !harvestWait(modelCtx, time.Second) {
							return
						}
						continue
					}
					scope, err := m.Scope(modelCtx)
					if err != nil || !harvestAccountEligible(account, scope, true) || !auth.CodexTicketGateEnabled() {
						return
					}
					// 为完整候选先预留两次额度，避免所有通道只拿到首请求而无法续接。
					if !reserveHarvestBudget(&reserved, r.MaxAttempts, cost) {
						return
					}
					node, proxyURL, err := collection.Next(modelCtx, lane)
					if err != nil || node.ID == "" {
						reserved.Add(-int64(cost))
						if err != nil && modelCtx.Err() == nil {
							event := baseEvent(model)
							event.Result, event.Message = "network_error", "采票节点分配失败"
							results <- outcome{event: event}
						}
						return
					}
					sent := 0
					started := time.Now()
					attemptCtx := context.WithValue(modelCtx, harvestProtocolKey{}, controls.Transport)
					attemptCtx = context.WithValue(attemptCtx, harvestEdgeKey{}, controls.EdgeIP)
					attemptCtx = context.WithValue(attemptCtx, harvestReserveKey{}, func(wait context.Context) error {
						if remaining := time.Until(last.Add(time.Duration(r.ProbeIntervalSeconds) * time.Second)); remaining > 0 && !harvestWait(wait, remaining) {
							return wait.Err()
						}
						if wait.Err() != nil {
							return wait.Err()
						}
						currentScope, err := m.Scope(wait)
						if err != nil || !auth.CodexTicketGateEnabled() || !harvestAccountEligible(account, currentScope, true) {
							return fmt.Errorf("采票范围变化、账号忙碌或采票已关闭")
						}
						if sent >= cost {
							return fmt.Errorf("采票请求预算已用完")
						}
						sent++
						used.Add(1)
						last = time.Now()
						m.jobsMu.Lock()
						m.jobs[job.ID].Attempts = int(used.Load())
						m.jobsMu.Unlock()
						return nil
					})
					result := fireCodexTicketHarvest(attemptCtx, account, model, proxyURL, time.Duration(controls.Speed.AttemptTimeoutSeconds)*time.Second)
					reserved.Add(-int64(cost - sent))
					event := baseEvent(model)
					event.NodeID, event.NodeName, event.Attempt = node.ID, node.Name, int(used.Load())
					event.Result, event.HTTPStatus, event.Length = result.Result, result.HTTPStatus, max(len(result.State), result.ObservedLength)
					event.LatencyMS = time.Since(started).Milliseconds()
					if result.HTTPStatus == 401 || result.HTTPStatus == 403 || result.HTTPStatus == 429 {
						event.Result = "account_error"
					}
					if modelCtx.Err() != nil {
						event.Result = "cancelled"
					}
					results <- outcome{event, result, node}
					if event.Result == "success" || event.Result == "account_error" {
						return
					}
				}
			}(lane)
		}
		go func() { workers.Wait(); close(results) }()
		won, stop := false, false
		for outcome := range results {
			event, result := outcome.event, outcome.result
			if event.Result == "success" {
				if won || stop || ctx.Err() != nil {
					event.Result = "cancelled"
				} else {
					result.Binding.HarvestNodeID, result.Binding.HarvestNodeName = outcome.node.ID, outcome.node.Name
					result.Binding.HarvestPoolID, result.Binding.ManagedNode = collection.PoolID(), true
					if publishCodexTicket(m.DB, m.Store, account, model, result.State, codexTicketIssuedAt(result.State), result.Cookies, result.Binding) {
						won = true
						event.Stage, event.CookieCount = "stored", (&auth.CodexTicket{Cookie: result.Cookies.Header}).CookieCount()
						m.jobsMu.Lock()
						m.jobs[job.ID].TicketsStored++
						m.jobsMu.Unlock()
					} else {
						event.Result, stop = "persistence_error", true
					}
					cancel()
				}
			}
			if event.Result == "account_error" {
				stop = true
				cancel()
			}
			// 迟到的取消或失败结果不能覆盖已经入库的成功状态。
			if event.Result != "cancelled" && (!won || event.Result == "success") {
				summary := &auth.CodexTicketProbeSummary{Result: event.Result, HTTPStatus: event.HTTPStatus, CheckedAt: time.Now()}
				if event.Result != "success" {
					cooldown := controls.Speed.CooldownSeconds
					if event.Result == "account_error" {
						cooldown = r.RateLimitCooldownSeconds
					}
					next := time.Now().Add(time.Duration(cooldown) * time.Second)
					summary.NextProbeAt = &next
				}
				m.Store.PublishCodexTicketProbe(account.ID(), model, summary)
				write, done := context.WithTimeout(context.Background(), 3*time.Second)
				_ = m.DB.UpdateCredentials(write, account.ID(), map[string]interface{}{auth.CodexTicketProbeCredentialKey(model): summary})
				done()
			}
			event.Message = harvestResultMessage(event.Result)
			emit(event)
		}
		cancel()
		if stop || (won && r.StopOnSuccess) {
			return
		}
	}
}
