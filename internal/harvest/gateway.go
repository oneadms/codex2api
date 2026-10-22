package harvest

import (
	"errors"
	"slices"
	"time"
)

const GatewaySettingsKey = "codex_gateway_routing_v1"

// GatewaySettings contains configuration only. Cookies and credentials never
// enter this DTO or the settings/history tables.
type GatewaySettings struct {
	CookiePool           GatewayCookiePool `json:"cookie_pool"`
	WSSession            GatewayWSSession  `json:"ws_session"`
	TicketPoolScheduling bool              `json:"ticket_pool_scheduling"`
	SchedulingMode       string            `json:"scheduling_mode"`
	SchedulingGroupIDs   []int64           `json:"scheduling_group_ids"`
	Revision             string            `json:"revision"`
}
type GatewayCookiePool struct {
	Enabled             bool    `json:"enabled"`
	SourceAccountIDs    []int64 `json:"source_account_ids"`
	TargetAccountIDs    []int64 `json:"target_account_ids"`
	IPAffinity          bool    `json:"ip_affinity"`
	RotateNodes         bool    `json:"rotate_nodes"`
	MaxNodeAttempts     int     `json:"max_node_attempts"`
	NodeCooldownSeconds int     `json:"node_cooldown_seconds"`
	TTLSeconds          int     `json:"ttl_seconds"`
}
type GatewayWSSession struct {
	Enabled    bool    `json:"enabled"`
	AccountIDs []int64 `json:"account_ids"`
	TTLSeconds int     `json:"ttl_seconds"`
}

func DefaultGatewaySettings() GatewaySettings {
	return GatewaySettings{CookiePool: GatewayCookiePool{SourceAccountIDs: []int64{}, TargetAccountIDs: []int64{}, MaxNodeAttempts: 3, NodeCooldownSeconds: 3600, TTLSeconds: 230}, WSSession: GatewayWSSession{AccountIDs: []int64{}, TTLSeconds: 3600}, SchedulingMode: "account", SchedulingGroupIDs: []int64{}}
}
func (v *GatewaySettings) Normalize() {
	if v.CookiePool.TTLSeconds == 0 {
		v.CookiePool.TTLSeconds = 230
	}
	if v.CookiePool.MaxNodeAttempts == 0 {
		v.CookiePool.MaxNodeAttempts = 3
	}
	if v.CookiePool.NodeCooldownSeconds == 0 {
		v.CookiePool.NodeCooldownSeconds = 3600
	}
	if v.WSSession.TTLSeconds == 0 {
		v.WSSession.TTLSeconds = 3600
	}
	if v.SchedulingMode == "" {
		v.SchedulingMode = "account"
	}
	if v.CookiePool.RotateNodes {
		v.CookiePool.IPAffinity = true
	}
	if v.WSSession.Enabled {
		v.CookiePool.Enabled = true
		for _, id := range v.WSSession.AccountIDs {
			if !slices.Contains(v.CookiePool.SourceAccountIDs, id) && !slices.Contains(v.CookiePool.TargetAccountIDs, id) {
				v.CookiePool.TargetAccountIDs = append(v.CookiePool.TargetAccountIDs, id)
			}
		}
	}
}
func (v GatewaySettings) Validate() error {
	if v.CookiePool.TTLSeconds < 30 || v.CookiePool.TTLSeconds > 240 {
		return errors.New("路由 Cookie 有效期必须为 30–240 秒")
	}
	if v.CookiePool.MaxNodeAttempts < 1 || v.CookiePool.MaxNodeAttempts > 10 {
		return errors.New("节点尝试数必须为 1–10")
	}
	if v.CookiePool.NodeCooldownSeconds < 60 || v.CookiePool.NodeCooldownSeconds > 86400 {
		return errors.New("节点冷却必须为 60–86400 秒")
	}
	if v.WSSession.TTLSeconds < 60 || v.WSSession.TTLSeconds > 3600 {
		return errors.New("WebSocket 固定会话有效期必须为 60–3600 秒")
	}
	validateIDs := func(ids []int64, limit int) bool {
		seen := map[int64]bool{}
		if len(ids) > limit {
			return false
		}
		for _, id := range ids {
			if id <= 0 || seen[id] {
				return false
			}
			seen[id] = true
		}
		return true
	}
	if !validateIDs(v.CookiePool.SourceAccountIDs, 64) || !validateIDs(v.CookiePool.TargetAccountIDs, 64) || !validateIDs(v.WSSession.AccountIDs, 64) || !validateIDs(v.SchedulingGroupIDs, 100) {
		return errors.New("账号和分组 ID 必须为正数且不能重复，来源和目标各最多 64 个")
	}
	for _, id := range v.CookiePool.SourceAccountIDs {
		if slices.Contains(v.CookiePool.TargetAccountIDs, id) || slices.Contains(v.WSSession.AccountIDs, id) {
			return errors.New("来源账号与目标账号不能重叠")
		}
	}
	if v.CookiePool.Enabled && (len(v.CookiePool.SourceAccountIDs) == 0 || len(v.CookiePool.TargetAccountIDs) == 0) {
		return errors.New("启用网关借票前请选择来源账号和目标账号")
	}
	if v.WSSession.Enabled && len(v.WSSession.AccountIDs) == 0 {
		return errors.New("请选择 WebSocket 固定会话账号")
	}
	if v.SchedulingMode != "account" && v.SchedulingMode != "model" && v.SchedulingMode != "groups" {
		return errors.New("调度模式必须为 account、model 或 groups")
	}
	if v.TicketPoolScheduling && v.SchedulingMode == "groups" && len(v.SchedulingGroupIDs) == 0 {
		return errors.New("请选择调度分组")
	}
	return nil
}

type GatewayRouteStatus struct {
	AccountID        int64      `json:"account_id"`
	State            string     `json:"state"`
	Reason           string     `json:"reason"`
	Gateway          string     `json:"gateway,omitempty"`
	ProxyNode        string     `json:"proxy_node,omitempty"`
	Active           bool       `json:"active"`
	CheckedAt        *time.Time `json:"checked_at,omitempty"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	RemainingSeconds int64      `json:"remaining_seconds"`
}
type GatewayWSStatus struct {
	AccountID      int64  `json:"account_id"`
	ActiveSessions int    `json:"active_sessions"`
	Ready          bool   `json:"ready"`
	Reason         string `json:"reason"`
}
type GatewaySetup struct {
	State     string `json:"state"`
	Reason    string `json:"reason"`
	AccountID int64  `json:"account_id"`
}
type GatewayRuntime struct {
	Revision    string               `json:"revision"`
	GeneratedAt time.Time            `json:"generated_at"`
	Mode        string               `json:"mode"`
	Preparing   bool                 `json:"preparing"`
	ReadyRoutes int                  `json:"ready_routes"`
	Sources     []GatewayRouteStatus `json:"sources"`
	Targets     []GatewayRouteStatus `json:"targets"`
	WS          []GatewayWSStatus    `json:"ws"`
	Setup       GatewaySetup         `json:"setup"`
	LastTest    *GatewayTestResult   `json:"last_test,omitempty"`
}
type GatewayTestResult struct {
	Action     string    `json:"action"`
	AccountID  int64     `json:"account_id"`
	Success    bool      `json:"success"`
	Reason     string    `json:"reason"`
	DurationMS int64     `json:"duration_ms"`
	CheckedAt  time.Time `json:"checked_at"`
}
type GatewayHistoryRecord struct {
	ID              int64     `json:"id"`
	SourceAccountID int64     `json:"source_account_id"`
	TargetAccountID int64     `json:"target_account_id"`
	Gateway         string    `json:"gateway"`
	Action          string    `json:"action"`
	Success         bool      `json:"success"`
	Reason          string    `json:"reason"`
	CheckedAt       time.Time `json:"checked_at"`
}
type GatewayHistoryPage struct {
	Items []GatewayHistoryRecord `json:"items"`
	Total int64                  `json:"total"`
}
