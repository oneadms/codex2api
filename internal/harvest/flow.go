package harvest

import (
	"fmt"
	"time"
)

const ScopeKey = "codex_harvest_scope_v1"

// Scope 只限制采票范围，不改变账号的业务调度资格。
type Scope struct {
	Mode              string  `json:"mode"`
	GroupIDs          []int64 `json:"group_ids"`
	AccountPolicy     string  `json:"account_policy"`
	SkippedAccountIDs []int64 `json:"skipped_account_ids"`
}

func DefaultScope() Scope {
	return Scope{Mode: "all", AccountPolicy: "schedulable_only", GroupIDs: []int64{}, SkippedAccountIDs: []int64{}}
}

func (s Scope) Validate() error {
	if s.Mode != "all" && s.Mode != "selected" {
		return fmt.Errorf("采票范围必须是 all 或 selected")
	}
	if s.AccountPolicy != "schedulable_only" && s.AccountPolicy != "prioritize_schedulable" {
		return fmt.Errorf("采票账号策略无效")
	}
	for _, ids := range [][]int64{s.GroupIDs, s.SkippedAccountIDs} {
		if len(ids) > 1000 {
			return fmt.Errorf("采票范围不能超过 1000 项")
		}
		seen := map[int64]bool{}
		for _, id := range ids {
			if id <= 0 || seen[id] {
				return fmt.Errorf("采票范围包含无效或重复 ID")
			}
			seen[id] = true
		}
	}
	return nil
}

// Event 只保存可诊断的摘要，禁止写入票据、Cookie、代理凭据和上游正文。
type Event struct {
	ID              int64     `json:"id"`
	JobID           string    `json:"job_id,omitempty"`
	AccountID       int64     `json:"account_id"`
	Model           string    `json:"model"`
	Source          string    `json:"source"`
	Stage           string    `json:"stage"`
	NodeID          string    `json:"node_id,omitempty"`
	NodeName        string    `json:"node_name,omitempty"`
	SelectionReason string    `json:"selection_reason,omitempty"`
	Result          string    `json:"result"`
	Message         string    `json:"message"`
	HTTPStatus      int       `json:"http_status"`
	Length          int       `json:"length"`
	ExpectedLength  int       `json:"expected_length"`
	CookieCount     int       `json:"cookie_count"`
	LatencyMS       int64     `json:"latency_ms"`
	Attempt         int       `json:"attempt"`
	CreatedAt       time.Time `json:"created_at"`
}

type EventPage struct {
	Items []Event `json:"items"`
	Total int64   `json:"total"`
}

type ManualRequest struct {
	AccountID                int64    `json:"account_id"`
	Models                   []string `json:"models"`
	ProbeIntervalSeconds     int      `json:"probe_interval_seconds"`
	RateLimitCooldownSeconds int      `json:"rate_limit_cooldown_seconds"`
	MaxAttempts              int      `json:"max_attempts"`
	NodeSwitchRule           string   `json:"node_switch_rule"`
	StopOnSuccess            bool     `json:"stop_on_success"`
}

func (r ManualRequest) Validate() error {
	if r.AccountID <= 0 || len(r.Models) == 0 || len(r.Models) > 64 {
		return fmt.Errorf("请选择账号和模型")
	}
	if r.MaxAttempts < 1 || r.MaxAttempts > 100 {
		return fmt.Errorf("手动采票次数必须为 1–100")
	}
	if r.ProbeIntervalSeconds < 0 || r.ProbeIntervalSeconds > 60 || r.RateLimitCooldownSeconds < 1 || r.RateLimitCooldownSeconds > 3600 {
		return fmt.Errorf("手动采票间隔或冷却超出范围")
	}
	switch r.NodeSwitchRule {
	case "every_request", "312_or_2fail", "312_only", "never":
	default:
		return fmt.Errorf("节点切换规则无效")
	}
	return nil
}

type ManualJob struct {
	ID            string        `json:"id"`
	Request       ManualRequest `json:"request"`
	Running       bool          `json:"running"`
	Cancelled     bool          `json:"cancelled"`
	Attempts      int           `json:"attempts"`
	TicketsStored int           `json:"tickets_stored"`
	LastEvent     *Event        `json:"last_event,omitempty"`
	StartedAt     time.Time     `json:"started_at"`
	FinishedAt    *time.Time    `json:"finished_at,omitempty"`
}
