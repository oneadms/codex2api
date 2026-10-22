import test from 'node:test'
import assert from 'node:assert/strict'
import { normalizeGatewaySettings, toggleGatewayAccount, validateGatewaySettings } from './codexHarvest.ts'
import { harvestEn, harvestZh } from '../locales/harvest.ts'

test('gateway settings normalize to the backend defaults without legacy fields', () => {
  const settings = normalizeGatewaySettings(null)
  assert.deepEqual(settings.cookie_pool, {
    enabled: false, source_account_ids: [], target_account_ids: [],
    ip_affinity: false, rotate_nodes: false,
    max_node_attempts: 3, node_cooldown_seconds: 3600, ttl_seconds: 230,
  })
  assert.deepEqual(settings.ws_session, { enabled: false, account_ids: [], ttl_seconds: 3600 })
  assert.equal(settings.ticket_pool_scheduling, false)
  assert.equal(settings.scheduling_mode, 'account')
  assert.deepEqual(settings.scheduling_group_ids, [])
  assert.equal(settings.revision, '')
  // 多来源列表取代旧单来源字段：即使只选一个账号也只走列表，不存在单独的兼容字段。
  assert.equal('reuse_source_account_id' in settings, false)
  assert.equal('reuse_source_account_id' in settings.cookie_pool, false)
  const single = normalizeGatewaySettings({ cookie_pool: { enabled: true, source_account_ids: [7], target_account_ids: [8] } })
  assert.deepEqual(single.cookie_pool.source_account_ids, [7])
  assert.equal(validateGatewaySettings(single), '')
})

test('gateway settings clamp numbers and keep dependent switches coherent', () => {
  const clamped = normalizeGatewaySettings({
    cookie_pool: { max_node_attempts: 99, node_cooldown_seconds: 1, ttl_seconds: 999 },
    ws_session: { ttl_seconds: 30 },
    scheduling_mode: 'invalid',
  })
  assert.equal(clamped.cookie_pool.max_node_attempts, 10)
  assert.equal(clamped.cookie_pool.node_cooldown_seconds, 60)
  assert.equal(clamped.cookie_pool.ttl_seconds, 240)
  assert.equal(clamped.ws_session.ttl_seconds, 60)
  assert.equal(clamped.scheduling_mode, 'account')

  const rotated = normalizeGatewaySettings({ cookie_pool: { rotate_nodes: true, ip_affinity: false } })
  assert.equal(rotated.cookie_pool.ip_affinity, true)

  // 固定会话依赖路由池，并把未出现在来源/目标里的 WS 账号补进目标。
  const ws = normalizeGatewaySettings({ ws_session: { enabled: true, account_ids: [5, 6] }, cookie_pool: { source_account_ids: [5], target_account_ids: [9] } })
  assert.equal(ws.cookie_pool.enabled, true)
  assert.deepEqual(ws.cookie_pool.target_account_ids, [9, 6])
})

test('gateway validation rejects overlap and incomplete selections', () => {
  const base = normalizeGatewaySettings({
    cookie_pool: { enabled: true, source_account_ids: [1], target_account_ids: [2] },
    ws_session: { enabled: true, account_ids: [2], ttl_seconds: 600 },
  })
  assert.equal(validateGatewaySettings(base), '')
  assert.equal(validateGatewaySettings({ ...base, cookie_pool: { ...base.cookie_pool, target_account_ids: [1] } }), 'overlap')
  assert.equal(validateGatewaySettings({ ...base, ws_session: { ...base.ws_session, account_ids: [1] } }), 'overlap')
  assert.equal(validateGatewaySettings({ ...base, cookie_pool: { ...base.cookie_pool, source_account_ids: [] } }), 'chooseBoth')
  assert.equal(validateGatewaySettings({ ...base, cookie_pool: { ...base.cookie_pool, target_account_ids: [] }, ws_session: { ...base.ws_session, enabled: false } }), 'chooseBoth')
  assert.equal(validateGatewaySettings({ ...base, ws_session: { ...base.ws_session, account_ids: [] } }), 'chooseWS')
  assert.equal(validateGatewaySettings({ ...base, ticket_pool_scheduling: true, scheduling_mode: 'groups', scheduling_group_ids: [] }), 'chooseGroups')
  assert.equal(validateGatewaySettings({ ...base, scheduling_group_ids: Array.from({ length: 101 }, (_, index) => index + 1) }), 'groupLimit')
})

test('gateway account toggles stay mutually exclusive and bounded', () => {
  assert.deepEqual(toggleGatewayAccount([], 3), [3])
  assert.deepEqual(toggleGatewayAccount([3], 3), [])
  // 已在对侧列表中的账号不能被加入。
  assert.deepEqual(toggleGatewayAccount([], 3, [3]), [])
  assert.deepEqual(toggleGatewayAccount([], 3, [4]), [3])
  assert.equal(toggleGatewayAccount(Array.from({ length: 64 }, (_, index) => index + 1), 65).length, 64)
})

test('every gateway runtime code the backend can emit has a zh and en label', () => {
  const families = {
    routeStates: ['ready', 'candidate', 'waiting', 'disabled', 'failed', 'expired', 'rejected'],
    setupStates: ['disabled', 'idle', 'running', 'queued', 'failed', 'ok', 'ready'],
    gatewayReasons: [
      'not_tested', 'account_unavailable', 'target_probe_passed', 'source_passed_pending_target',
      'disabled', 'settings_unavailable', 'timeout', 'request_failed', 'cookie_pool_disabled',
      'configuration_changed', 'source_probe_cooldown', 'no_qualified_source_route',
      'target_probe_failed', 'target_route_changed', 'target_probe_degraded',
      'target_validation_in_progress', 'source_busy', 'routing_cookie_missing',
      'upstream_http_error', 'response_not_qualified', 'node_unavailable', 'node_cooling_down',
      'rotation_use_once', 'websocket_ticket_unavailable', 'websocket_disabled',
      'route_expired', 'success', 'cancelled', 'waiting', 'ready',
    ],
    historyActions: ['source', 'prepare', 'verify', 'ws'],
    results: [
      'success', 'invalid_state', 'account_error', 'network_error', 'persistence_error',
      'response_incomplete_or_error', 'skipped', 'cancelled', 'error', 'terminal_error', 'token_error',
    ],
  }
  for (const [family, codes] of Object.entries(families)) {
    for (const code of codes) {
      assert.ok(harvestZh[family][code], `zh ${family}.${code}`)
      assert.ok(harvestEn[family][code], `en ${family}.${code}`)
    }
  }
  for (const key of ['enabled', 'disabled', 'action', 'target', 'gatewayHost', 'scheduling', 'sources', 'targets']) {
    assert.ok(harvestZh[key] && harvestEn[key], key)
  }
})

