export interface HarvestSpeed {
  round_interval_seconds: number
  probe_interval_seconds: number
  attempt_timeout_seconds: number
  cooldown_seconds: number
  max_requests_per_round: number
  max_node_attempts: number
  refresh_before_seconds: number
}
/** 对齐 internal/harvest/controls.go：strategy 取代旧的 reuse_source_account_id，不迁移旧值。 */
export interface HarvestControls { version: number; strategy: 'refresh' | 'fixed'; proxy_ids: number[]; edge_ip: string; transport: 'sse' | 'websocket'; node_memory_enabled: boolean; speed: HarvestSpeed }
export interface HarvestScope { mode: 'all' | 'selected'; group_ids: number[]; account_policy: 'schedulable_only' | 'prioritize_schedulable'; skipped_account_ids: number[] }
export interface HarvestControlSnapshot {
  settings: HarvestControls
  configured: boolean
  settings_error: string
  defaults: HarvestControls
  presets: Record<string, HarvestSpeed>
  bounds: Record<keyof HarvestSpeed, { min: number; max: number }>
  available: boolean
  availability_reason: string
  runtime: { running: boolean; next_round_at: string | null; requests_used: number; request_budget: number; current_node: string; selection_reason: string; degraded_reason: string }
}
export interface HarvestTicket {
	probe_verified: boolean; transport?: 'sse' | 'websocket'
	acquisition?: 'self' | 'reuse'; source_account_id?: number
  model: string; ready: boolean; revoked: boolean; length: number; remaining_seconds: number
  cookie_count: number; cookie_remaining_seconds: number; cookie_expired: boolean
  egress_bound: boolean; session_bound: boolean; standby_ready: boolean
  probe?: { result: string; http_status: number; checked_at: string; next_probe_at?: string }
}
export interface HarvestAccount { id: number; email: string; group_ids: number[]; eligible: boolean; skipped: boolean; busy: boolean; tickets: HarvestTicket[] | null }
export interface HarvestEvent {
  id: number; job_id?: string; account_id: number; model: string; source: string; stage: string
  node_id?: string; node_name?: string; selection_reason?: string; result: string; message: string
  http_status: number; length: number; expected_length: number; cookie_count: number; latency_ms: number; attempt: number; created_at: string
}
export interface HarvestManualRequest {
	collect_lanes: number
  account_id: number; models: string[]; probe_interval_seconds: number; rate_limit_cooldown_seconds: number
  max_attempts: number; node_switch_rule: 'every_request' | '312_or_2fail' | '312_only' | 'never'; stop_on_success: boolean
}
export interface HarvestJob { id: string; request: HarvestManualRequest; running: boolean; cancelled: boolean; attempts: number; tickets_stored: number; last_event?: HarvestEvent; started_at: string; finished_at?: string }
/** coverage.go：snapshot 顶层新增 coverage，按账号采样 ready/missing 与平均覆盖率。 */
export interface HarvestAccountCoverage { started_at: string; updated_at: string; ready_seconds: number; missing_seconds: number; percent: number | null }
export interface HarvestCoverageSnapshot {
  started_at?: string | null; updated_at?: string | null; account_count: number
  average_ready_seconds: number; average_missing_seconds: number; average_percent: number | null
  sample_interval_seconds: number; accounts: Record<number, HarvestAccountCoverage>
}
export interface HarvestSnapshot { controls: HarvestControlSnapshot; scope: HarvestScope; accounts: HarvestAccount[]; jobs: HarvestJob[]; parallel_available: boolean; coverage?: HarvestCoverageSnapshot }
export interface HarvestNodeRecord {
  id: number; pool_id: string; account_id: number; model: string; blocks: number; node_id: string; node_name: string; provider: string
  successes: number; misses: number; network_errors: number; account_errors: number; consecutive_failures: number
  last_success: string | null; cooldown_until: string | null; latency_ms: number; last_result: string; updated_at: string
}
export interface HarvestPage<T> { items: T[]; total: number }
export interface MihomoCountryFilter { mode: 'off' | 'exclude' | 'include'; codes: string[]; allow_unknown: boolean }
export interface MihomoNode { name: string; display_name?: string; state: string; country_code?: string; country_checked_at?: string; country_error?: string; country_blocked: boolean }
export interface MihomoStatus {
  country_filter: MihomoCountryFilter; country_codes: string[]; eligible_nodes: number; country_excluded: number; unknown_countries: number
  use_once: boolean; installed: boolean; running: boolean; busy: boolean; phase: string; error?: string
  subscriptions: number; nodes: number; endpoint: string; supported: boolean; node_states: MihomoNode[] | null
}
export interface MihomoAction { action: string; subscriptions?: string[]; append?: boolean; country_filter?: MihomoCountryFilter }

/** admin/codex_gateway.go + harvest/gateway.go 的只读契约。配置仅含 ID 与数字，不含 Cookie 等敏感值。 */
export interface GatewayCookiePool {
  enabled: boolean; source_account_ids: number[]; target_account_ids: number[]
  ip_affinity: boolean; rotate_nodes: boolean; max_node_attempts: number; node_cooldown_seconds: number; ttl_seconds: number
}
export interface GatewayWSSession { enabled: boolean; account_ids: number[]; ttl_seconds: number }
export interface GatewaySettings {
  cookie_pool: GatewayCookiePool; ws_session: GatewayWSSession
  ticket_pool_scheduling: boolean; scheduling_mode: 'account' | 'model' | 'groups'; scheduling_group_ids: number[]; revision: string; route_revision: string
}
export interface GatewayRouteStatus {
  account_id: number; state: string; reason: string; gateway?: string; proxy_node?: string
  active: boolean; checked_at?: string; expires_at?: string; remaining_seconds: number
}
export interface GatewayWSStatus { account_id: number; active_sessions: number; ready: boolean; reason: string }
export interface GatewaySetup { state: string; reason: string; account_id: number }
export interface GatewayRuntime {
  revision: string; generated_at: string; mode: 'unified_ticket' | 'cookie_route' | 'disabled'
  preparing: boolean; ready_routes: number; sources: GatewayRouteStatus[]; targets: GatewayRouteStatus[]; ws: GatewayWSStatus[]; setup: GatewaySetup; last_test?: GatewayTestResult
}
export interface GatewayTestResult { action: 'prepare' | 'verify' | 'ws'; account_id: number; success: boolean; reason: string; duration_ms: number; checked_at: string }
export interface GatewayHistoryRecord {
  id: number; source_account_id: number; target_account_id: number; gateway: string; action: string
  success: boolean; reason: string; checked_at: string
}

export const harvestSpeedFields: (keyof HarvestSpeed)[] = ['round_interval_seconds', 'probe_interval_seconds', 'attempt_timeout_seconds', 'cooldown_seconds', 'max_requests_per_round', 'max_node_attempts', 'refresh_before_seconds']

export function splitHarvestList(value: string): string[] {
  return [...new Set(value.split(/[\s,，]+/).map(item => item.trim()).filter(Boolean))]
}

/** 归一化输入：空值回默认、clamp 到 Go 校验范围、WS 依赖联动，与 gateway.go Normalize 一致。 */
export function normalizeGatewaySettings(value: Partial<GatewaySettings> | null | undefined): GatewaySettings {
  const cookie = value?.cookie_pool ?? {} as Partial<GatewayCookiePool>
  const ws = value?.ws_session ?? {} as Partial<GatewayWSSession>
  const out: GatewaySettings = {
    cookie_pool: {
      enabled: cookie.enabled ?? false,
      source_account_ids: [...(cookie.source_account_ids ?? [])],
      target_account_ids: [...(cookie.target_account_ids ?? [])],
      ip_affinity: cookie.ip_affinity ?? false,
      rotate_nodes: cookie.rotate_nodes ?? false,
      max_node_attempts: clampInt(cookie.max_node_attempts, 3, 1, 10),
      node_cooldown_seconds: clampInt(cookie.node_cooldown_seconds, 3600, 60, 86400),
      ttl_seconds: clampInt(cookie.ttl_seconds, 230, 30, 240),
    },
    ws_session: {
      enabled: ws.enabled ?? false,
      account_ids: [...(ws.account_ids ?? [])],
      ttl_seconds: clampInt(ws.ttl_seconds, 3600, 60, 3600),
    },
    ticket_pool_scheduling: value?.ticket_pool_scheduling ?? false,
    scheduling_mode: (['account', 'model', 'groups'] as const).includes(value?.scheduling_mode as never) ? value!.scheduling_mode! : 'account',
    scheduling_group_ids: [...(value?.scheduling_group_ids ?? [])],
    revision: value?.revision ?? '',
    route_revision: value?.route_revision ?? '',
  }
  if (out.cookie_pool.rotate_nodes) out.cookie_pool.ip_affinity = true
  if (out.ws_session.enabled) {
    out.cookie_pool.enabled = true
    for (const id of out.ws_session.account_ids) {
      if (!out.cookie_pool.source_account_ids.includes(id) && !out.cookie_pool.target_account_ids.includes(id)) {
        out.cookie_pool.target_account_ids.push(id)
      }
    }
  }
  return out
}

function clampInt(value: number | undefined, fallback: number, min: number, max: number): number {
  if (value === undefined || value === null || !Number.isFinite(value)) return fallback
  const rounded = Math.trunc(value)
  return Math.min(max, Math.max(min, rounded))
}

/** 前端预校验。返回 i18n key；空字符串表示可通过 PUT 提交。 */
export function validateGatewaySettings(value: GatewaySettings): string {
  const overlap = value.cookie_pool.source_account_ids.some(id => value.cookie_pool.target_account_ids.includes(id) || value.ws_session.account_ids.includes(id))
  if (overlap) return 'overlap'
  if (value.cookie_pool.enabled && (value.cookie_pool.source_account_ids.length === 0 || value.cookie_pool.target_account_ids.length === 0)) return 'chooseBoth'
  if (value.ws_session.enabled && value.ws_session.account_ids.length === 0) return 'chooseWS'
  if (value.ticket_pool_scheduling && value.scheduling_mode === 'groups' && value.scheduling_group_ids.length === 0) return 'chooseGroups'
  if (value.cookie_pool.source_account_ids.length > 64 || value.cookie_pool.target_account_ids.length > 64 || value.ws_session.account_ids.length > 64) return 'accountLimit'
  if (value.scheduling_group_ids.length > 100) return 'groupLimit'
  return ''
}

/** 目标/来源/WS 互斥多选：一侧选中的账号从对侧移除，避免 409/400 之外的组合错误。 */
export function toggleGatewayAccount(list: number[], id: number, opposite: number[] = []): number[] {
  if (list.includes(id)) return list.filter(item => item !== id)
  if (opposite.includes(id)) return list
  if (list.length >= 64) return list
  return [...list, id]
}

/** runtime.mode 展示文案：只有 780 且门控开启时才是 unified_ticket，前端不得声称 SSE 完整票可用于 WS。 */
export function gatewayModeLabelKey(mode: GatewayRuntime['mode']): string {
  if (mode === 'unified_ticket') return 'harvest.modeUnified'
  if (mode === 'cookie_route') return 'harvest.modeCookieRoute'
  return 'harvest.modeDisabled'
}

/** GET /codex-harvest/gateway/status 仅读取缓存路由，不发起探测；结果由显式 prepare/verify/ws 产生。 */
export function gatewayProbePolicyKey(): string { return 'harvest.probeHint' }
