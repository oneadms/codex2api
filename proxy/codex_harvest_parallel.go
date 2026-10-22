package proxy

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/harvest"
)

// All lanes/models share the actual request budget, including continuations.
func reserveHarvestRequest(used *atomic.Int64, limit int) bool {
	return reserveHarvestBudget(used, limit, 1)
}

func reserveHarvestBudget(used *atomic.Int64, limit, cost int) bool {
	if cost <= 0 || limit < cost {
		return false
	}
	for {
		n := used.Load()
		if n > int64(limit-cost) {
			return false
		}
		if used.CompareAndSwap(n, n+int64(cost)) {
			return true
		}
	}
}

func (m *CodexHarvestManager) runParallel(ctx context.Context, account *auth.Account, job harvest.ManualJob) {
	r := job.Request
	identity := account.CodexTicketIdentitySnapshot()
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
		controls, _, _ := m.Controls.Controls(ctx)
		m.recordHarvestProbe(account, event.Model, event, max(controls.Speed.CooldownSeconds, r.RateLimitCooldownSeconds), 0)
		emit(event)
		return
	}
	cost := 1
	if auth.CodexTicketTargetLength(account.GetPlanType()) == 780 {
		cost = 2
	}
	if r.MaxAttempts < cost {
		return
	}
	controls, _, _ := m.Controls.Controls(ctx)
	collection, err := m.beginHarvestCollection(ctx, controls, min(r.CollectLanes, r.MaxAttempts/cost))
	if err != nil {
		event := baseEvent(r.Models[0])
		event.Result, event.Message = "network_error", "采票出口分配失败"
		emit(event)
		return
	}
	defer func() {
		if err := collection.Close(); err != nil {
			event := baseEvent(r.Models[0])
			event.Result, event.Message = "network_error", "采票通道清理失败，托管代理已停止"
			emit(event)
		}
	}()
	var used atomic.Int64
	var reserved atomic.Int64
	for _, model := range r.Models {
		if ctx.Err() != nil || int64(r.MaxAttempts)-used.Load() < int64(cost) || !m.harvestAdmitted(ctx, account, identity, model) {
			return
		}
		controls, _, _ := m.Controls.Controls(ctx)
		modelCtx, cancel := context.WithCancel(ctx)
		type outcome struct {
			event  harvest.Event
			result codexTicketHarvestResult
			exit   harvestCollectionExit
		}
		lanes := min(collection.lanes, (r.MaxAttempts-int(used.Load()))/cost)
		results := make(chan outcome, lanes)
		var workers sync.WaitGroup
		for lane := 0; lane < lanes; lane++ {
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
					if !m.harvestAdmitted(modelCtx, account, identity, model) {
						return
					}
					// Reserve a complete candidate before provisioning an exit. Useful
					// lanes are bounded by nodes/proxies AND remaining request budget.
					if !reserveHarvestBudget(&reserved, r.MaxAttempts, cost) {
						return
					}
					exit, err := collection.Next(modelCtx, lane)
					if err != nil || exit.node.ID == "" {
						reserved.Add(-int64(cost))
						if err != nil && modelCtx.Err() == nil {
							event := baseEvent(model)
							event.Result, event.Message = "network_error", "采票节点分配失败"
							results <- outcome{event: event}
						}
						return
					}
					sent, denied := 0, false
					started := time.Now()
					attemptCtx := context.WithValue(modelCtx, harvestProtocolKey{}, controls.Transport)
					attemptCtx = context.WithValue(attemptCtx, harvestEdgeKey{}, controls.EdgeIP)
					attemptCtx = context.WithValue(attemptCtx, harvestReserveKey{}, func(wait context.Context) error {
						if remaining := time.Until(last.Add(time.Duration(r.ProbeIntervalSeconds) * time.Second)); remaining > 0 && !harvestWait(wait, remaining) {
							return wait.Err()
						}
						if !m.harvestAdmitted(wait, account, identity, model) || (exit.proxyID > 0 && !m.harvestProxyCurrent(wait, exit.proxyID, exit.proxyURL)) {
							denied = true
							return fmt.Errorf("账号身份、采票范围或代理配置已变化")
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
					result := fireCodexTicketHarvest(attemptCtx, account, model, exit.proxyURL, time.Duration(controls.Speed.AttemptTimeoutSeconds)*time.Second)
					reserved.Add(-int64(cost - sent))
					event := baseEvent(model)
					event.NodeID, event.NodeName, event.Attempt = exit.node.ID, exit.node.Name, int(used.Load())
					event.Result, event.HTTPStatus, event.Length = result.Result, result.HTTPStatus, max(len(result.State), result.ObservedLength)
					event.RetryAfterSeconds = harvestRetryAfterSeconds(result.RetryAfter)
					event.LatencyMS = time.Since(started).Milliseconds()
					if result.Terminal {
						event.Result = "terminal_error"
					}
					if result.HTTPStatus == 401 || result.HTTPStatus == 403 || result.HTTPStatus == 429 || result.Result == "token_error" {
						event.Result = "account_error"
					}
					if modelCtx.Err() != nil || denied {
						event.Result = "cancelled"
					}
					results <- outcome{event, result, exit}
					if event.Result == "success" || event.Result == "account_error" || event.Result == "cancelled" || result.Terminal || result.RetryAfter > 0 {
						return
					}
				}
			}(lane)
		}
		go func() { workers.Wait(); close(results) }()
		won, stop, rejected := false, false, false
		for out := range results {
			event, result := out.event, out.result
			if event.Result == "success" {
				if won || stop || rejected || !m.harvestAdmitted(ctx, account, identity, model) || (out.exit.proxyID > 0 && !m.harvestProxyCurrent(ctx, out.exit.proxyID, out.exit.proxyURL)) {
					event.Result = "cancelled"
				} else {
					collection.bind(result.Binding, out.exit)
					result.Binding.Identity = identity
					if publishCodexTicket(m.DB, m.Store, account, model, result.State, codexTicketIssuedAt(result.State), result.Cookies, result.Binding) {
						won = true
						event.Stage, event.CookieCount = "stored", (&auth.CodexTicket{Cookie: result.Cookies.Header}).CookieCount()
						m.jobsMu.Lock()
						m.jobs[job.ID].TicketsStored++
						m.jobsMu.Unlock()
					} else {
						event.Result, stop = "persistence_error", true
					}
				}
				cancel()
			}
			if event.Result == "account_error" {
				stop = true
				cancel()
			}
			if result.Terminal {
				rejected = true
				cancel()
			}
			if result.RetryAfter > 0 {
				cancel()
			}
			// Only the coordinator publishes. Late failures cannot replace a
			// persisted winner; repeated failures retain the longest cooldown.
			if !won || event.Result == "success" {
				m.recordHarvestProbe(account, model, event, max(controls.Speed.CooldownSeconds, r.RateLimitCooldownSeconds), result.RetryAfter)
			}
			event.Message = harvestResultMessage(event.Result)
			if won && event.Result != "success" {
				m.appendEvent(event)
			} else {
				emit(event)
			}
		}
		cancel()
		if stop || (won && r.StopOnSuccess) {
			return
		}
	}
}
