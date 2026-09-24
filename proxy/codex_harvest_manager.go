package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/harvest"
	"github.com/codex2api/internal/mihomo"
)

// CodexHarvestManager 把采票控制、节点学习及任务生命周期接入现有账号仓库。
type CodexHarvestManager struct {
	DB          *database.DB
	Store       *auth.Store
	Repository  *database.HarvestRepository
	Controls    *harvest.CodexHarvestService
	DataDir     string
	working     sync.Map
	jobsMu      sync.Mutex
	jobs        map[string]*harvest.ManualJob
	cancels     map[string]context.CancelFunc
	roundMu     sync.Mutex
	cursor      uint64
	paceMu      sync.Mutex
	lastRequest time.Time
}

var codexHarvester atomic.Pointer[CodexHarvestManager]

func ConfigureCodexHarvest(db *database.DB, store *auth.Store, dataDir string) *CodexHarvestManager {
	repo := db.HarvestRepository()
	settings := auth.ConfiguredCodexTicketSettings()
	speed := harvest.CodexHarvestSpeed{RoundIntervalSeconds: settings.ProbeIntervalSeconds, ProbeIntervalSeconds: 2,
		AttemptTimeoutSeconds: settings.ProbeTimeoutSeconds, CooldownSeconds: settings.CooldownSeconds,
		MaxRequestsPerRound: settings.MaxProbesPerRound, MaxNodeAttempts: 3, RefreshBeforeSeconds: settings.RefreshBeforeSeconds}
	m := &CodexHarvestManager{DB: db, Store: store, Repository: repo, Controls: harvest.NewCodexHarvestService(repo, repo, &speed), DataDir: dataDir,
		jobs: map[string]*harvest.ManualJob{}, cancels: map[string]context.CancelFunc{}}
	codexHarvester.Store(m)
	registerCodexTicketFeedback(db, store)
	return m
}

func (m *CodexHarvestManager) Scope(ctx context.Context) (harvest.Scope, error) {
	scope := harvest.DefaultScope()
	raw, err := m.Repository.GetValue(ctx, harvest.ScopeKey)
	if errors.Is(err, harvest.ErrSettingNotFound) {
		return scope, nil
	}
	if err != nil {
		return scope, err
	}
	if err = json.Unmarshal([]byte(raw), &scope); err != nil {
		return scope, err
	}
	return scope, scope.Validate()
}

func (m *CodexHarvestManager) SaveScope(ctx context.Context, scope harvest.Scope) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(scope)
	if err != nil {
		return err
	}
	if err = m.Repository.Set(ctx, harvest.ScopeKey, string(raw)); err == nil {
		KickCodexTicketHarvest()
	}
	return err
}

func harvestAccountEligible(account *auth.Account, scope harvest.Scope, idle bool) bool {
	if account == nil || !account.CanHarvestCodexTicket(time.Now()) {
		return false
	}
	if idle && account.GetActiveRequests() > 0 {
		return false
	}
	for _, id := range scope.SkippedAccountIDs {
		if id == account.ID() {
			return false
		}
	}
	if scope.Mode == "selected" {
		groups := map[int64]struct{}{}
		for _, id := range scope.GroupIDs {
			groups[id] = struct{}{}
		}
		if !account.InAnyGroup(groups) {
			return false
		}
	}
	return scope.AccountPolicy != "schedulable_only" || account.IsAvailable()
}

func (m *CodexHarvestManager) ControlSnapshot(ctx context.Context) harvest.CodexHarvestControlSnapshot {
	out := m.Controls.Snapshot(ctx)
	sidecar, err := mihomo.LoadDirectedSidecar(m.DataDir, auth.ConfiguredCodexTicketSettings().HarvestProxyURL)
	if err == nil {
		query, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		_, err = sidecar.Directory(query)
	}
	out.Available = err == nil
	if err != nil {
		out.AvailabilityReason = "定向学习需要外部 Mihomo 的专用 Selector 和监听端口；当前按代理轮换，不能保证固定出口 IP"
	}
	return out
}

func (m *CodexHarvestManager) RunRound(ctx context.Context) {
	if !m.roundMu.TryLock() {
		return
	}
	defer m.roundMu.Unlock()
	if !auth.CodexTicketGateEnabled() {
		return
	}
	controls, _, err := m.Controls.Controls(ctx)
	if err != nil {
		log.Printf("[codex-harvest] 读取控制配置失败，使用最近有效配置")
	}
	scope, err := m.Scope(ctx)
	if err != nil {
		log.Printf("[codex-harvest] 读取采票范围失败，本轮跳过")
		return
	}
	settings := auth.ConfiguredCodexTicketSettings()
	budget := controls.Speed.MaxRequestsPerRound
	m.Controls.SetRuntime(func(r *harvest.CodexHarvestRuntime) {
		*r = harvest.CodexHarvestRuntime{Running: true, RequestBudget: budget}
	})
	defer m.Controls.SetRuntime(func(r *harvest.CodexHarvestRuntime) { r.Running = false; r.CurrentNode = "" })
	accounts := m.Store.Accounts()
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID() < accounts[j].ID() })
	// 轮次游标保证请求预算较小时后面的账号仍有机会补票。
	if len(accounts) > 0 {
		start := int(m.cursor % uint64(len(accounts)))
		m.cursor++
		accounts = append(append([]*auth.Account{}, accounts[start:]...), accounts[:start]...)
	}
	if scope.AccountPolicy == "prioritize_schedulable" {
		sort.SliceStable(accounts, func(i, j int) bool { return accounts[i].IsAvailable() && !accounts[j].IsAvailable() })
	}
	for _, account := range accounts {
		if ctx.Err() != nil || budget <= 0 || !auth.CodexTicketGateEnabled() {
			break
		}
		if !harvestAccountEligible(account, scope, true) {
			continue
		}
		if _, busy := m.working.LoadOrStore(account.ID(), true); busy {
			continue
		}
		func() {
			defer m.working.Delete(account.ID())
			for _, model := range settings.Models {
				if ctx.Err() != nil || budget <= 0 {
					return
				}
				currentScope, scopeErr := m.Scope(ctx)
				if scopeErr != nil || !harvestAccountEligible(account, currentScope, true) {
					return
				}
				now := time.Now()
				if ticket := account.CodexTicketForModel(model, now, 0); ticket != nil {
					auth.PublishCodexTicketToSharedPool(account, ticket)
					refresh := time.Duration(controls.Speed.RefreshBeforeSeconds) * time.Second
					// Cookie 的有效窗口比上游预设短，至少保留一半窗口用于正常业务。
					refresh = min(refresh, auth.CodexTicketCookieFreshness/2)
					if !ticket.NeedsRefresh(now, refresh) && ticket.Standby.Valid(now, ticket.Length) {
						continue
					}
				}
				if account.CodexTicketProbeCoolingDown(model, now) {
					continue
				}
				tried := map[string]bool{}
				for attempt := 0; attempt < controls.Speed.MaxNodeAttempts && budget > 0; attempt++ {
					event := m.attempt(ctx, account, model, settings.HarvestProxyURL, controls, &budget, tried, nil, "auto", "", attempt+1)
					m.Controls.SetRuntime(func(r *harvest.CodexHarvestRuntime) { r.RequestsUsed = r.RequestBudget - budget })
					if event.Result == "success" || event.Result == "account_error" || event.Result == "skipped" || event.Result == "cancelled" {
						break
					}
					if !harvestWait(ctx, time.Duration(controls.Speed.ProbeIntervalSeconds)*time.Second) {
						return
					}
				}
			}
		}()
	}
	query, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err = m.Repository.PruneEvents(query); err != nil && ctx.Err() == nil {
		log.Printf("[codex-harvest] 采票历史清理失败")
	}
}

func harvestWait(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (m *CodexHarvestManager) appendEvent(event harvest.Event) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := m.Repository.AppendEvent(ctx, event); err != nil {
		log.Printf("[codex-harvest] 采票事件落库失败: account=%d", event.AccountID)
	}
}

type CodexHarvestAccount struct {
	ID       int64                    `json:"id"`
	Email    string                   `json:"email"`
	GroupIDs []int64                  `json:"group_ids"`
	Eligible bool                     `json:"eligible"`
	Skipped  bool                     `json:"skipped"`
	Busy     bool                     `json:"busy"`
	Tickets  []auth.CodexTicketStatus `json:"tickets"`
}

func (m *CodexHarvestManager) Accounts(ctx context.Context) ([]CodexHarvestAccount, error) {
	scope, err := m.Scope(ctx)
	if err != nil {
		return nil, err
	}
	models := auth.ConfiguredCodexTicketSettings().Models
	items := []CodexHarvestAccount{}
	for _, a := range m.Store.Accounts() {
		if !a.SupportsCodexTickets() {
			continue
		}
		a.Mu().RLock()
		email := a.Email
		a.Mu().RUnlock()
		_, busy := m.working.Load(a.ID())
		row := CodexHarvestAccount{ID: a.ID(), Email: email, GroupIDs: a.GroupIDSnapshot(), Eligible: harvestAccountEligible(a, scope, false), Busy: busy,
			Tickets: a.CodexTicketStatuses(models, time.Now(), 0)}
		for _, id := range scope.SkippedAccountIDs {
			if id == a.ID() {
				row.Skipped = true
				break
			}
		}
		items = append(items, row)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, nil
}

func (m *CodexHarvestManager) validateManual(ctx context.Context, r *harvest.ManualRequest) (*auth.Account, error) {
	models, err := auth.NormalizeCodexTicketModels(r.Models)
	if err != nil {
		return nil, err
	}
	r.Models = models
	if err = r.Validate(); err != nil {
		return nil, err
	}
	if !auth.CodexTicketGateEnabled() {
		return nil, fmt.Errorf("请先启用采票并配置代理和模型")
	}
	for _, model := range models {
		if !auth.CodexTicketModelGated(model) {
			return nil, fmt.Errorf("模型 %s 未加入采票名单", model)
		}
	}
	scope, err := m.Scope(ctx)
	if err != nil {
		return nil, err
	}
	account := m.Store.FindByID(r.AccountID)
	if !harvestAccountEligible(account, scope, false) {
		return nil, fmt.Errorf("账号不在采票范围或当前不可用")
	}
	return account, nil
}
