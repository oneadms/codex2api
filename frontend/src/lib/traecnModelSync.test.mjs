import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import {
  buildTraeCNModelSyncPatch,
  buildTraeCNModelSyncScope,
  DEFAULT_TRAECN_MODEL_SYNC_SETTINGS,
  isValidModelSyncTime,
  normalizeTraeCNModelSyncSettings,
} from './traecnModelSync.ts'
import { collectAccountOperationResult, snapshotAccountOperationResults, summarizeAccountOperationResults } from './accountOperationResults.ts'

const page = readFileSync(new URL('../pages/TraeCNAccounts.tsx', import.meta.url), 'utf8')
const settingsPage = readFileSync(new URL('../pages/Settings.tsx', import.meta.url), 'utf8')

test('daily sync defaults to disabled at Shanghai midnight and preserves configured values', () => {
  assert.deepEqual(normalizeTraeCNModelSyncSettings({}), { model_sync_enabled: false, model_sync_time: '00:00', model_sync_timezone: 'Asia/Shanghai' })
  assert.deepEqual(normalizeTraeCNModelSyncSettings({ model_sync_enabled: true, model_sync_time: '23:59', model_sync_timezone: 'UTC' }), { model_sync_enabled: true, model_sync_time: '23:59', model_sync_timezone: 'UTC' })
})

test('daily times accept midnight and require strict 24-hour HH:mm', () => {
  for (const valid of ['00:00', '09:05', '12:30', '23:59']) assert.equal(isValidModelSyncTime(valid), true, valid)
  for (const invalid of ['', '0:00', '24:00', '23:60', '12:3', '12:30:00', ' 12:30', '12:30\n', 'ab:cd']) assert.equal(isValidModelSyncTime(invalid), false, invalid)
})

test('saving schedules sends only changed schedule fields, never unrelated settings', () => {
  const saved = { ...DEFAULT_TRAECN_MODEL_SYNC_SETTINGS, model_mapping: { alias: 'old-model' }, preflight_sse_passthrough: true, models: ['old-model'] }
  assert.deepEqual(buildTraeCNModelSyncPatch(saved, saved), {})
  assert.deepEqual(buildTraeCNModelSyncPatch({ ...saved, model_sync_enabled: true, model_mapping: { changed: 'new-model' } }, saved), { model_sync_enabled: true })
  assert.deepEqual(buildTraeCNModelSyncPatch({ ...saved, model_sync_time: '04:15', model_sync_timezone: 'UTC' }, saved), { model_sync_time: '04:15', model_sync_timezone: 'UTC' })
  assert.deepEqual(buildTraeCNModelSyncPatch({ ...saved, model_sync_enabled: false }, { ...saved, model_sync_enabled: true }), { model_sync_enabled: false })
})

test('selected scope wins over filters and includes off-page account IDs', () => {
  const scope = buildTraeCNModelSyncScope([91, 2, 91], { channel: 'traecn', search: 'query', status: 'normal' }, 400)
  assert.deepEqual(scope, { scope: 'selected', count: 2, body: { ids: [91, 2] } })
})

test('filtered and all scopes use explicit TRAECN selectors rather than visible-page IDs', () => {
  assert.deepEqual(buildTraeCNModelSyncScope([], { channel: 'traecn', search: 'alice', status: 'disabled' }, 75), {
    scope: 'filtered', count: 75, body: { selector: { channel: 'traecn', search: 'alice', status: 'disabled' } },
  })
  assert.deepEqual(buildTraeCNModelSyncScope([], { channel: 'codex', status: 'all' }, 200), {
    scope: 'all', count: 200, body: { selector: { channel: 'traecn', search: undefined, status: undefined } },
  })
  assert.equal(buildTraeCNModelSyncScope([], { channel: 'traecn', search: 'query' }, 10).scope, 'filtered')
  assert.equal(buildTraeCNModelSyncScope([], { channel: 'traecn', status: 'normal' }, 10).scope, 'filtered')
})

test('model sync results preserve account identity and every failure before progress throttling', () => {
  const results = new Map([[99, { accountId: 99, status: 'failed', message: 'stale' }]])
  collectAccountOperationResult(results, { action: 'traecn_batch_sync_models', type: 'start' })
  collectAccountOperationResult(results, { action: 'traecn_batch_sync_models', type: 'progress', account_id: 8, account_name: 'Successful account', status: 'success', message: 'Updated 12 models' })
  collectAccountOperationResult(results, { action: 'traecn_batch_sync_models', type: 'progress', account_id: 27, account_name: 'Off-page account', account_email: 'offpage@example.test', status: 'failed', http_status: 502, error: 'Upstream catalog unavailable', message: 'Ignored in favor of error' })
  collectAccountOperationResult(results, { action: 'traecn_batch_sync_models', type: 'complete' })
  const snapshot = snapshotAccountOperationResults(results)
  assert.deepEqual(snapshot.map(({ accountId, status }) => [accountId, status]), [[27, 'failed'], [8, 'success']])
  assert.equal(snapshot[0].message, 'Upstream catalog unavailable')
  assert.equal(snapshot[0].accountName, 'Off-page account')
  assert.equal(snapshot[0].accountEmail, 'offpage@example.test')
  assert.equal(snapshot[0].httpStatus, 502)
  assert.deepEqual(summarizeAccountOperationResults(snapshot), { total: 2, success: 1, failed: 1, banned: 0, rateLimited: 0 })
})

test('check-in results remain supported by the shared result collector', () => {
  const results = new Map()
  collectAccountOperationResult(results, { action: 'traecn_batch_checkin', type: 'progress', account_id: 7, error: 'check-in failed' })
  assert.equal(snapshotAccountOperationResults(results)[0].message, 'check-in failed')
})

test('both batch entries share a guarded stream, refresh accounts and models, and link to daily settings', () => {
  assert.match(page, /\/accounts\/traecn\/batch-sync-models\?stream=true/)
  assert.match(page, /onClick=\{\(\) => void handleBatchSyncModels\(\)\}/)
  assert.match(page, /onClick=\{\(\) => void handleBatchSyncModels\(Array\.from\(selectedIDs\)\)\}/)
  assert.match(page, /if \(!confirmed \|\| batchOperationRef\.current\) return/)
  assert.match(page, /await Promise\.all\(\[reload\(true\), reloadModels\(\)\]\)/)
  assert.match(page, /to="\/settings\?tab=traecn"/)
  assert.match(settingsPage, /<TraeCNModelSyncSettings \/>/)
})

test('all three locales explain scope, midnight, timezone and failure behavior', () => {
  for (const language of ['en', 'zh', 'zh-TW']) {
    const locale = JSON.parse(readFileSync(new URL(`../locales/${language}.json`, import.meta.url), 'utf8'))
    for (const key of ['batchSyncModels', 'batchSyncModelsRunning', 'batchSyncModelsProgressTitle', 'batchSyncModelsResultsTitle', 'batchSyncModelsDone', 'batchSyncModelsFailed', 'batchSyncModelsReloadFailed', 'modelSyncSettings']) assert.equal(typeof locale.traecn[key], 'string', `${language}: ${key}`)
    for (const scope of ['selected', 'filtered', 'all']) {
      assert.equal(typeof locale.traecn.batchSyncModelsConfirm[`${scope}Title`], 'string')
      assert.match(locale.traecn.batchSyncModelsConfirm[`${scope}Description`], /\{\{count\}\}/)
    }
    const settings = locale.settings.traecnModelSync
    for (const key of ['title', 'description', 'enabled', 'scope', 'time', 'timeHint', 'timezone', 'timezoneHint', 'catchUp', 'failureHint', 'invalidTime', 'invalidTimezone', 'save', 'saved']) assert.equal(typeof settings[key], 'string', `${language}: ${key}`)
    assert.match(settings.scope, /TRAECN/)
    assert.match(settings.timeHint, /00:00/)
    assert.match(settings.timezoneHint, /IANA/)
  }
})
