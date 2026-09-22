package proxy

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/harvest"
)

func (m *CodexHarvestManager) StartManual(ctx context.Context, request harvest.ManualRequest) (harvest.ManualJob, error) {
	account, err := m.validateManual(ctx, &request)
	if err != nil {
		return harvest.ManualJob{}, err
	}
	if _, busy := m.working.LoadOrStore(account.ID(), true); busy {
		return harvest.ManualJob{}, fmt.Errorf("该账号已有采票任务")
	}
	job := harvest.ManualJob{ID: NewUpstreamSessionUUID(), Request: request, Running: true, StartedAt: time.Now().UTC()}
	jobCtx, cancel := context.WithCancel(context.Background())
	m.jobsMu.Lock()
	// 只保留最近任务的内存快照；详细事件已经持久化。
	for id, old := range m.jobs {
		if !old.Running && time.Since(old.StartedAt) > time.Hour {
			delete(m.jobs, id)
		}
	}
	if len(m.jobs) >= 100 {
		m.jobsMu.Unlock()
		cancel()
		m.working.Delete(account.ID())
		return harvest.ManualJob{}, fmt.Errorf("手动任务数量已达上限，请稍后重试")
	}
	copy := job
	m.jobs[job.ID] = &copy
	m.cancels[job.ID] = cancel
	m.jobsMu.Unlock()
	if !m.DB.RunBackgroundTask(func(lifecycle context.Context) {
		stop := context.AfterFunc(lifecycle, cancel)
		defer stop()
		defer cancel()
		defer m.working.Delete(account.ID())
		m.runManual(jobCtx, account, job)
	}) {
		cancel()
		m.working.Delete(account.ID())
		m.jobsMu.Lock()
		delete(m.jobs, job.ID)
		delete(m.cancels, job.ID)
		m.jobsMu.Unlock()
		return harvest.ManualJob{}, fmt.Errorf("服务正在关闭")
	}
	return job, nil
}

func (m *CodexHarvestManager) runManual(ctx context.Context, account *auth.Account, job harvest.ManualJob) {
	defer func() {
		m.jobsMu.Lock()
		defer m.jobsMu.Unlock()
		now := time.Now().UTC()
		current := m.jobs[job.ID]
		current.Running = false
		current.Cancelled = ctx.Err() != nil
		current.FinishedAt = &now
		delete(m.cancels, job.ID)
	}()
	r := job.Request
	budget := r.MaxAttempts
	successes := map[string]bool{}
	tried := map[string]bool{}
	var keep *harvestSelection
	failures := 0
	modelIndex := 0
	for budget > 0 && ctx.Err() == nil {
		if !auth.CodexTicketGateEnabled() {
			return
		}
		scope, err := m.Scope(ctx)
		if err != nil || !harvestAccountEligible(account, scope, false) {
			return
		}
		if account.GetActiveRequests() > 0 {
			if !harvestWait(ctx, time.Second) {
				return
			}
			continue
		}
		controls, _, _ := m.Controls.Controls(ctx)
		controls.Speed.ProbeIntervalSeconds = r.ProbeIntervalSeconds
		model := r.Models[modelIndex%len(r.Models)]
		modelIndex++
		if r.StopOnSuccess && successes[model] {
			if len(successes) == len(r.Models) {
				return
			}
			continue
		}
		before := budget
		event := m.attempt(ctx, account, model, auth.ConfiguredCodexTicketSettings().HarvestProxyURL, controls, &budget, tried, &keep, "manual", job.ID, r.MaxAttempts-budget+1)
		m.jobsMu.Lock()
		current := m.jobs[job.ID]
		current.Attempts = r.MaxAttempts - budget
		current.LastEvent = &event
		if event.Result == "success" {
			current.TicketsStored++
			successes[model] = true
			failures = 0
		} else {
			failures++
		}
		m.jobsMu.Unlock()
		if r.StopOnSuccess && len(successes) == len(r.Models) {
			return
		}
		if event.Result == "cancelled" || before == budget {
			return
		}
		change := r.NodeSwitchRule == "every_request" || (r.NodeSwitchRule == "312_only" && event.Length == 312) || (r.NodeSwitchRule == "312_or_2fail" && (event.Length == 312 || failures >= 2))
		if change {
			keep = nil
			failures = 0
		}
		// 换模型后重新计算账号、模型维度的学习记录。
		if len(r.Models) > 1 {
			keep = nil
		}
		wait := time.Duration(r.ProbeIntervalSeconds) * time.Second
		if event.HTTPStatus == 429 {
			wait = time.Duration(r.RateLimitCooldownSeconds) * time.Second
		}
		if event.HTTPStatus == 401 || event.HTTPStatus == 403 || event.Result == "persistence_error" {
			return
		}
		if budget > 0 && !harvestWait(ctx, wait) {
			return
		}
	}
}

func (m *CodexHarvestManager) Jobs() []harvest.ManualJob {
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	items := make([]harvest.ManualJob, 0, len(m.jobs))
	for _, job := range m.jobs {
		copy := *job
		copy.Request.Models = append([]string{}, job.Request.Models...)
		if job.LastEvent != nil {
			event := *job.LastEvent
			copy.LastEvent = &event
		}
		items = append(items, copy)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].StartedAt.After(items[j].StartedAt) })
	return items
}

func (m *CodexHarvestManager) CancelJob(id string) bool {
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	if cancel := m.cancels[id]; cancel != nil {
		cancel()
		return true
	}
	return false
}

// Close 在服务关闭时停止所有手动任务，数据库排空负责等待事件落库。
func (m *CodexHarvestManager) Close() {
	m.jobsMu.Lock()
	defer m.jobsMu.Unlock()
	for _, cancel := range m.cancels {
		cancel()
	}
}
