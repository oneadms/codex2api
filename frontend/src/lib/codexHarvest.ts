export interface HarvestSpeed {
  round_interval_seconds: number
  probe_interval_seconds: number
  attempt_timeout_seconds: number
  cooldown_seconds: number
  max_requests_per_round: number
  max_node_attempts: number
  refresh_before_seconds: number
}
export interface HarvestControls { version: number; node_memory_enabled: boolean; speed: HarvestSpeed }
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
  account_id: number; models: string[]; probe_interval_seconds: number; rate_limit_cooldown_seconds: number
  max_attempts: number; node_switch_rule: 'every_request' | '312_or_2fail' | '312_only' | 'never'; stop_on_success: boolean
}
export interface HarvestJob { id: string; request: HarvestManualRequest; running: boolean; cancelled: boolean; attempts: number; tickets_stored: number; last_event?: HarvestEvent; started_at: string; finished_at?: string }
export interface HarvestSnapshot { controls: HarvestControlSnapshot; scope: HarvestScope; accounts: HarvestAccount[]; jobs: HarvestJob[] }
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

export const harvestSpeedFields: (keyof HarvestSpeed)[] = ['round_interval_seconds', 'probe_interval_seconds', 'attempt_timeout_seconds', 'cooldown_seconds', 'max_requests_per_round', 'max_node_attempts', 'refresh_before_seconds']

export function splitHarvestList(value: string): string[] {
  return [...new Set(value.split(/[\s,，]+/).map(item => item.trim()).filter(Boolean))]
}
