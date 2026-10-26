import assert from 'node:assert/strict'
import { before, after, test } from 'node:test'
import { createServer } from 'node:http'
import { fileURLToPath } from 'node:url'
import { build } from 'vite'
import tailwindcss from '@tailwindcss/vite'
import { chromium } from 'playwright'
import en from '../src/locales/en.json' with { type: 'json' }

const root = fileURLToPath(new URL('../', import.meta.url))
const schedule = en.settings.traecnModelSync
const action = 'traecn_batch_sync_models'
const batchPath = '/api/admin/accounts/traecn/batch-sync-models'
const interpolate = (text, values) => text.replace(/{{(\w+)}}/g, (_, key) => String(values[key]))
const frame = event => `data: ${JSON.stringify({ action, ...event })}\n\n`
let browser, script, css

before(async () => {
  // Bundle the real components, API client, streaming reader, translations and styles.
  // Only the loopback HTTP service below is mocked; no production account is used.
  const bundle = await build({
    configFile: false, root, logLevel: 'silent',
    resolve: { alias: { '@': `${root}src` } },
    define: { 'process.env.NODE_ENV': '"production"' },
    esbuild: { jsx: 'automatic' },
    plugins: [tailwindcss(), {
      name: 'traecn-model-sync-browser-harness',
      resolveId(id) {
        if (id === 'virtual:traecn-model-sync-harness') return '\0traecn-model-sync-harness'
      },
      load(id) {
        if (id !== '\0traecn-model-sync-harness') return
        return `
          import React from 'react';
          import {createRoot} from 'react-dom/client';
          import {MemoryRouter} from 'react-router-dom';
          import i18next from 'i18next';
          import {initReactI18next} from 'react-i18next';
          import Schedule from '@/components/TraeCNModelSyncSettings.tsx';
          import Accounts from '@/pages/TraeCNAccounts.tsx';
          import {ToastProvider} from '@/components/ToastProvider.tsx';
          import en from '@/locales/en.json';
          import '@/index.css';
          i18next.use(initReactI18next).init({
            lng:'en', fallbackLng:'en', resources:{en:{translation:en}}, interpolation:{escapeValue:false}
          }).then(() => {
            createRoot(document.getElementById('root')).render(
              React.createElement(MemoryRouter, null,
                React.createElement(ToastProvider, {defaultTimeoutMs:30000},
                  React.createElement(location.pathname === '/schedule' ? Schedule : Accounts)))
            );
          });
        `
      },
    }],
    build: {
      write: false, minify: false, emptyOutDir: false, cssCodeSplit: false,
      lib: { entry: 'virtual:traecn-model-sync-harness', formats: ['iife'], name: 'TraeCNModelSyncHarness' },
      rollupOptions: { input: 'virtual:traecn-model-sync-harness' },
    },
  })
  const output = (Array.isArray(bundle) ? bundle[0] : bundle).output
  script = output.find(item => item.type === 'chunk').code
  css = output.filter(item => item.type === 'asset' && item.fileName.endsWith('.css')).map(item => item.source).join('\n')
  assert.ok(css.length > 0, 'the interaction fixture must include the real responsive styles')
  browser = await chromium.launch({ headless: true })
})
after(async () => { await browser?.close() })

function json(res, value, status = 200) {
  res.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' })
  res.end(JSON.stringify(value))
}

async function fixture(t, component = 'accounts', overrides = {}) {
  const state = {
    settings: {
      model_sync_enabled: false, model_sync_time: '00:00', model_sync_timezone: 'Asia/Shanghai',
      model_mapping: { client: 'existing-model' }, preflight_sse_passthrough: true, models: ['existing-model'],
    },
    accounts: Array.from({ length: 42 }, (_, index) => ({
      id: index + 1, name: `account-${index + 1}`, email: `account-${index + 1}@example.test`,
      traecn_api: true, enabled: index % 2 === 1, status: index % 2 === 1 ? 'active' : 'disabled',
      models: index === 2 ? [] : [`old-model-${index + 1}`], group_ids: [],
      created_at: '2026-01-01T00:00:00Z', updated_at: '2026-01-01T00:00:00Z',
    })),
    catalog: ['old-catalog-model'], requests: [], writes: [], batches: [], pendingBatch: null,
    loadError: false, saveError: false, holdSave: false, pendingSave: null,
    batchHTTPError: false, modelReloadError: false,
    ...overrides,
  }
  const errors = []
  const unexpected = []
  const matching = selector => state.accounts.filter(account => {
    if (selector?.search && !account.name.includes(selector.search)) return false
    if (selector?.status === 'disabled' && account.enabled) return false
    return true
  })
  const server = createServer((req, res) => {
    void (async () => {
      const url = new URL(req.url, 'http://127.0.0.1')
      if (url.pathname === '/harness.js') {
        res.writeHead(200, { 'Content-Type': 'text/javascript' }); res.end(script); return
      }
      if (url.pathname === '/harness.css') {
        res.writeHead(200, { 'Content-Type': 'text/css' }); res.end(css); return
      }
      if (url.pathname === '/favicon.ico') { res.writeHead(204); res.end(); return }
      if (url.pathname === '/schedule' || url.pathname === '/accounts') {
        res.writeHead(200, { 'Content-Type': 'text/html' })
        res.end('<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><link rel="stylesheet" href="/harness.css"></head><body><main id="root" style="padding:16px"></main><script src="/harness.js"></script></body></html>')
        return
      }
      const chunks = []
      for await (const chunk of req) chunks.push(chunk)
      const body = chunks.length ? JSON.parse(Buffer.concat(chunks).toString()) : undefined
      state.requests.push({ path: url.pathname, method: req.method, query: Object.fromEntries(url.searchParams), body })
      if (url.pathname === '/api/admin/settings/traecn') {
        if (req.method === 'GET') {
          json(res, state.loadError ? { error: 'settings temporarily unavailable' } : state.settings, state.loadError ? 503 : 200)
        } else if (req.method === 'PUT') {
          state.writes.push(body)
          const finish = () => {
            if (state.saveError) return json(res, { error: 'schedule save failed' }, 503)
            state.settings = { ...state.settings, ...body }
            json(res, state.settings)
          }
          if (state.holdSave) state.pendingSave = finish
          else finish()
        } else throw new Error(`unexpected settings method: ${req.method}`)
        return
      }
      if (url.pathname === '/api/admin/accounts') {
        const all = matching(Object.fromEntries(url.searchParams))
        const page = Number(url.searchParams.get('page')) || 1
        const pageSize = Number(url.searchParams.get('page_size')) || 20
        json(res, {
          accounts: all.slice((page - 1) * pageSize, page * pageSize), total: all.length, page, page_size: pageSize,
          summary: { total: state.accounts.length, normal: 21, active: 21, disabled: 21, rate_limited: 0, error: 0 },
        })
        return
      }
      if (url.pathname === '/api/admin/account-groups') { json(res, { groups: [] }); return }
      if (url.pathname === '/api/admin/settings') { json(res, { traecn_test_model: 'auto' }); return }
      if (url.pathname === '/api/admin/accounts/live') { json(res, { accounts: {} }); return }
      if (/^\/api\/admin\/accounts\/\d+\/traecn\/credits$/.test(url.pathname)) { json(res, {}); return }
      if (url.pathname === '/api/admin/models') {
        json(res, state.modelReloadError ? { error: 'catalog reload failed' } : { traecn_models: state.catalog }, state.modelReloadError ? 503 : 200)
        return
      }
      if (url.pathname === batchPath && req.method === 'POST') {
        assert.equal(url.searchParams.get('stream'), 'true')
        const rows = body.ids ? state.accounts.filter(account => body.ids.includes(account.id)) : matching(body.selector)
        const batch = { body, rows, res, current: 0, success: 0, failed: 0 }
        state.batches.push(batch)
        if (state.batchHTTPError) { json(res, { error: 'sync service unavailable' }, 503); return }
        state.pendingBatch = batch
        res.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-store' })
        res.write(frame({ type: 'start', total: rows.length, current: 0, success: 0, failed: 0 }))
        return
      }
      unexpected.push(`${req.method} ${req.url}`)
      json(res, { error: 'unexpected fixture request' }, 404)
    })().catch(error => { errors.push(error.message); res.destroy() })
  })
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
  const origin = `http://127.0.0.1:${server.address().port}`
  const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } })
  page.setDefaultTimeout(10000)
  page.on('pageerror', error => errors.push(error.message))
  t.after(async () => {
    await page.close()
    server.closeAllConnections()
    await new Promise(resolve => server.close(resolve))
    assert.deepEqual(errors, [], 'browser and fixture should have no uncaught errors')
    assert.deepEqual(unexpected, [], 'all HTTP calls should be explicitly handled')
  })
  await page.goto(`${origin}/${component}`)
  if (component === 'accounts') {
    await page.getByRole('table').getByRole('checkbox', { name: 'Select account account-1', exact: true }).waitFor()
    await page.getByRole('table').getByText('old-catalog-model', { exact: true }).waitFor()
  }
  return { page, state }
}

const syncButton = page => page.getByRole('button', { name: en.traecn.batchSyncModels, exact: true }).first()
const requestCount = (state, path) => state.requests.filter(request => request.path === `/api/admin/${path}` && request.method === 'GET').length
const accountRow = (page, id) => page.getByRole('table').getByRole('row').filter({ has: page.getByRole('checkbox', { name: `Select account account-${id}`, exact: true }) })

async function confirmBatch(page, scope, count) {
  await syncButton(page).click()
  const modal = page.getByRole('dialog', { name: en.traecn.batchSyncModelsConfirm[`${scope}Title`], exact: true })
  await modal.getByText(interpolate(en.traecn.batchSyncModelsConfirm[`${scope}Description`], { count }), { exact: true }).waitFor()
  const started = page.waitForResponse(response => new URL(response.url()).pathname === batchPath)
  await modal.getByRole('button', { name: en.traecn.batchSyncModels, exact: true }).click()
  await started
}

function completeBatch(state, failedIDs = []) {
  const batch = state.pendingBatch
  assert.ok(batch, 'a batch must be running before completing it')
  for (const account of batch.rows) {
    const failed = failedIDs.includes(account.id)
    batch.current++
    batch[failed ? 'failed' : 'success']++
    batch.res.write(frame({
      type: 'progress', total: batch.rows.length, current: batch.current, success: batch.success, failed: batch.failed,
      account_id: account.id, account_name: account.name, account_email: account.email,
      status: failed ? 'failed' : 'success',
      ...(failed ? { error: 'upstream catalog unavailable' } : { message: 'catalog synced' }),
    }))
  }
  batch.res.end(frame({ type: 'complete', current: batch.current, total: batch.rows.length, success: batch.success, failed: batch.failed }))
  state.pendingBatch = null
}

async function waitForEnabled(locator) {
  await locator.waitFor()
  await locator.page().waitForFunction(element => !element.disabled, await locator.elementHandle())
}

async function closeResults(page) {
  const modal = page.getByRole('dialog', { name: en.traecn.batchSyncModelsResultsTitle, exact: false })
  await modal.waitFor()
  await modal.getByRole('button', { name: en.common.close, exact: true }).last().click()
  await modal.waitFor({ state: 'hidden' })
}

async function filterDisabled(page) {
  const searched = page.waitForResponse(response => {
    const url = new URL(response.url())
    return url.pathname === '/api/admin/accounts' && url.searchParams.get('search') === 'account-'
  })
  await page.getByPlaceholder(en.traecn.searchPlaceholder, { exact: true }).fill('account-')
  await searched
  await page.getByRole('button', { name: en.traecn.filterAll, exact: true }).click()
  const filtered = page.waitForResponse(response => {
    const url = new URL(response.url())
    return url.pathname === '/api/admin/accounts' && url.searchParams.get('status') === 'disabled'
  })
  await page.getByRole('option', { name: en.traecn.filterDisabled, exact: true }).click()
  await filtered
  await accountRow(page, 1).waitFor()
  await waitForEnabled(syncButton(page))
}

async function nextPage(page, firstID) {
  await page.getByRole('button', { name: en.common.next, exact: true }).click()
  await accountRow(page, firstID).waitFor()
  await waitForEnabled(syncButton(page))
}

test('schedule changes remain drafts until Save and persist across a reload', async t => {
  const { page, state } = await fixture(t, 'schedule')
  const enabled = page.getByRole('switch', { name: schedule.enabled, exact: true })
  const time = page.getByLabel(schedule.time, { exact: true })
  const save = page.getByRole('button', { name: schedule.save, exact: true })
  await enabled.waitFor()
  assert.equal(await enabled.isChecked(), false)
  assert.equal(await time.inputValue(), '00:00')
  assert.match(await page.getByLabel(schedule.timezone, { exact: true }).textContent(), /Asia\/Shanghai/)
  assert.equal(await save.isDisabled(), true)

  await enabled.click()
  await time.fill('01:45')
  await page.getByLabel(schedule.timezone, { exact: true }).click()
  await page.getByRole('option', { name: /· UTC$/, exact: false }).click()
  // Allow blur/change effects to run, then verify no implicit PUT or persisted mutation.
  await page.getByText(schedule.scope, { exact: true }).click()
  assert.deepEqual(state.writes, [])
  assert.equal(state.settings.model_sync_enabled, false)
  assert.equal(state.settings.model_sync_time, '00:00')
  await save.click()
  await page.getByRole('status').getByText(schedule.saved, { exact: true }).waitFor()
  assert.deepEqual(state.writes, [{ model_sync_enabled: true, model_sync_time: '01:45', model_sync_timezone: 'UTC' }])
  assert.deepEqual(state.settings.model_mapping, { client: 'existing-model' })
  assert.equal(state.settings.preflight_sse_passthrough, true)

  await page.reload()
  await enabled.waitFor()
  assert.equal(await enabled.isChecked(), true)
  assert.equal(await time.inputValue(), '01:45')
  assert.match(await page.getByLabel(schedule.timezone, { exact: true }).textContent(), /· UTC$/)
  await time.fill('02:00')
  await page.getByRole('button', { name: en.settings.traecnMapping.reset, exact: true }).click()
  assert.equal(await time.inputValue(), '01:45')
  assert.equal(state.writes.length, 1)
  assert.equal(await save.isDisabled(), true)
  await enabled.click()
  await save.click()
  await page.getByRole('status').getByText(schedule.saved, { exact: true }).waitFor()
  assert.deepEqual(state.writes[1], { model_sync_enabled: false })
})

test('invalid time blocks Save, failed saves retain the draft, and retry saves only changed fields', async t => {
  const { page, state } = await fixture(t, 'schedule')
  const time = page.getByLabel(schedule.time, { exact: true })
  const save = page.getByRole('button', { name: schedule.save, exact: true })
  await time.fill('')
  await page.getByRole('alert').getByText(schedule.invalidTime, { exact: true }).waitFor()
  assert.equal(await save.isDisabled(), true)
  assert.deepEqual(state.writes, [])
  await time.fill('23:59')
  state.saveError = true
  await save.click()
  await page.getByRole('alert').getByText('schedule save failed', { exact: true }).waitFor()
  assert.equal(await time.inputValue(), '23:59')
  assert.equal(state.settings.model_sync_time, '00:00')
  assert.equal(await save.isEnabled(), true)

  state.saveError = false
  state.holdSave = true
  const request = page.waitForRequest(request => request.method() === 'PUT')
  await save.click()
  await request
  assert.equal(await time.isDisabled(), true)
  assert.equal(await page.getByRole('switch').isDisabled(), true)
  assert.equal(await page.getByLabel(schedule.timezone, { exact: true }).isDisabled(), true)
  // The server response is deliberately held to test the in-flight form state.
  await page.waitForFunction(() => document.querySelector('input[type=time]').disabled)
  assert.ok(state.pendingSave)
  state.pendingSave()
  await page.getByRole('status').getByText(schedule.saved, { exact: true }).waitFor()
  assert.deepEqual(state.writes, [{ model_sync_time: '23:59' }, { model_sync_time: '23:59' }])
  assert.equal(state.settings.model_sync_time, '23:59')
})

test('initial settings failure offers Retry without exposing an unsaved default form', async t => {
  const { page, state } = await fixture(t, 'schedule', { loadError: true })
  await page.getByRole('alert').getByText('settings temporarily unavailable', { exact: true }).waitFor()
  assert.equal(await page.getByRole('switch').count(), 0)
  assert.equal(await page.getByRole('button', { name: schedule.save, exact: true }).count(), 0)
  state.loadError = false
  await page.getByRole('button', { name: en.common.retry, exact: true }).click()
  await page.getByRole('switch', { name: schedule.enabled, exact: true }).waitFor()
  assert.deepEqual(state.writes, [])
})

test('all-account sync confirms the cross-page scope and refreshes both snapshots after partial failure', async t => {
  const { page, state } = await fixture(t)
  assert.equal(await page.getByRole('link', { name: en.traecn.modelSyncSettings, exact: true }).getAttribute('href'), '/settings?tab=traecn')
  await syncButton(page).click()
  const confirmation = page.getByRole('dialog', { name: en.traecn.batchSyncModelsConfirm.allTitle, exact: true })
  await confirmation.getByRole('button', { name: en.common.cancel, exact: true }).click()
  assert.equal(state.batches.length, 0)
  const accountsBefore = requestCount(state, 'accounts')
  const modelsBefore = requestCount(state, 'models')
  await confirmBatch(page, 'all', 42)
  assert.deepEqual(state.batches[0].body, { selector: { channel: 'traecn' } })
  assert.equal(state.batches[0].rows.length, 42)
  assert.ok(state.batches[0].rows.some(account => !account.enabled))
  await page.getByText(en.traecn.batchSyncModelsProgressTitle, { exact: true }).waitFor()
  assert.equal(await syncButton(page).isDisabled(), true)
  assert.equal(await page.getByRole('button', { name: en.traecn.batchCheckin, exact: true }).isDisabled(), true)
  assert.equal(await page.getByRole('button', { name: en.accounts.testConnection, exact: true }).first().isDisabled(), true)
  state.accounts[0].models = ['new-account-model']
  state.catalog = ['new-catalog-model']
  completeBatch(state, [2])

  const results = page.getByRole('dialog', { name: en.traecn.batchSyncModelsResultsTitle, exact: false })
  await results.getByText('account-42@example.test', { exact: true }).waitFor()
  await results.getByRole('button', { name: interpolate(en.accounts.operationResultsFilterAria, { filter: en.accounts.operationProgressFailed, count: 1 }), exact: true }).click()
  assert.equal(await results.locator('tbody tr').count(), 1)
  await results.getByText('account-2@example.test', { exact: true }).waitFor()
  await results.getByText('upstream catalog unavailable', { exact: true }).waitFor()
  const notice = page.getByRole('status').filter({ hasText: interpolate(en.traecn.batchSyncModelsDone, { success: 41, failed: 1 }) })
  await notice.waitFor()
  assert.match(await notice.getAttribute('class'), /text-amber-/)
  await closeResults(page)
  await waitForEnabled(syncButton(page))
  assert.equal(requestCount(state, 'accounts'), accountsBefore + 1)
  assert.equal(requestCount(state, 'models'), modelsBefore + 1)
  await accountRow(page, 1).getByText('new-account-model', { exact: true }).waitFor()
  await accountRow(page, 2).getByText('old-model-2', { exact: true }).waitFor()
  await accountRow(page, 3).getByText('new-catalog-model', { exact: true }).waitFor()
})

test('filtered sync submits the server-side selector, not just the last page', async t => {
  const { page, state } = await fixture(t)
  await filterDisabled(page)
  await nextPage(page, 41)
  assert.equal(await page.getByRole('table').locator('tbody tr').count(), 1)
  await confirmBatch(page, 'filtered', 21)
  assert.deepEqual(state.batches[0].body, { selector: { channel: 'traecn', search: 'account-', status: 'disabled' } })
  assert.equal(state.batches[0].rows.length, 21)
  completeBatch(state)
  await closeResults(page)
  await waitForEnabled(syncButton(page))
  await accountRow(page, 41).waitFor()
})

test('explicit selections across pages override filters and lock competing batch actions', async t => {
  const { page, state } = await fixture(t)
  await filterDisabled(page)
  await accountRow(page, 1).getByRole('checkbox').check()
  await nextPage(page, 41)
  await accountRow(page, 41).getByRole('checkbox').check()
  await confirmBatch(page, 'selected', 2)
  assert.deepEqual(state.batches[0].body, { ids: [1, 41] })
  for (const button of await page.getByRole('button', { name: en.traecn.batchSyncModels, exact: true }).all()) assert.equal(await button.isDisabled(), true)
  assert.equal(await page.getByRole('button', { name: en.traecn.batchDelete, exact: true }).isDisabled(), true)
  assert.equal(await page.getByRole('button', { name: en.accounts.batchTest, exact: true }).isDisabled(), true)
  completeBatch(state)
  await closeResults(page)
  await waitForEnabled(syncButton(page))
  assert.equal(await page.getByRole('button', { name: en.traecn.batchDelete, exact: true }).isEnabled(), true)
})

for (const ending of ['truncated EOF', 'connection reset']) {
  test(`${ending} refreshes both snapshots, clears delayed progress and permits another sync`, async t => {
    const { page, state } = await fixture(t)
    await accountRow(page, 1).getByRole('checkbox').check()
    const accountsBefore = requestCount(state, 'accounts')
    const modelsBefore = requestCount(state, 'models')
    await confirmBatch(page, 'selected', 1)
    await page.getByText(en.traecn.batchSyncModelsProgressTitle, { exact: true }).waitFor()
    const progress = frame({ type: 'progress', account_id: 1, status: 'success', current: 1, total: 1, success: 1, failed: 0 })
    state.accounts[0].models = ['synced-before-disconnect']
    state.catalog = ['catalog-before-disconnect']
    if (ending === 'truncated EOF') state.pendingBatch.res.end(progress)
    else state.pendingBatch.res.destroy()
    state.pendingBatch = null
    const notice = page.getByRole('status').filter({ hasText: 'Batch model sync failed:' })
    await notice.waitFor()
    if (ending === 'truncated EOF') assert.ok((await notice.textContent()).includes(en.accounts.connectionEndedUnexpectedly))
    await waitForEnabled(syncButton(page))
    assert.equal(requestCount(state, 'accounts'), accountsBefore + 1)
    assert.equal(requestCount(state, 'models'), modelsBefore + 1)
    await accountRow(page, 1).getByText('synced-before-disconnect', { exact: true }).waitFor()
    await accountRow(page, 3).getByText('catalog-before-disconnect', { exact: true }).waitFor()
    // The progress hook throttles updates for 200ms; a queued frame must not reopen it.
    await page.waitForTimeout(350)
    assert.equal(await page.getByText(en.traecn.batchSyncModelsProgressTitle, { exact: true }).count(), 0)
    assert.equal(await page.getByRole('dialog', { name: en.traecn.batchSyncModelsResultsTitle, exact: false }).count(), 0)
    assert.equal(await page.getByRole('button', { name: en.traecn.batchDelete, exact: true }).isEnabled(), true)
    await confirmBatch(page, 'selected', 1)
    assert.equal(state.batches.length, 2)
    completeBatch(state)
    await closeResults(page)
    await waitForEnabled(syncButton(page))
  })
}

test('HTTP failure still reloads accounts and catalogs and restores batch controls', async t => {
  const { page, state } = await fixture(t, 'accounts', { batchHTTPError: true })
  const accountsBefore = requestCount(state, 'accounts')
  const modelsBefore = requestCount(state, 'models')
  await confirmBatch(page, 'all', 42)
  await page.getByRole('status').getByText('Batch model sync failed: sync service unavailable', { exact: true }).waitFor()
  await waitForEnabled(syncButton(page))
  assert.equal(requestCount(state, 'accounts'), accountsBefore + 1)
  assert.equal(requestCount(state, 'models'), modelsBefore + 1)
  state.batchHTTPError = false
  await confirmBatch(page, 'all', 42)
  completeBatch(state)
  await closeResults(page)
})

test('catalog reload failure is reported without preventing the account refresh or leaving controls locked', async t => {
  const { page, state } = await fixture(t)
  const accountsBefore = requestCount(state, 'accounts')
  const modelsBefore = requestCount(state, 'models')
  await confirmBatch(page, 'all', 42)
  state.modelReloadError = true
  state.accounts[0].models = ['updated-despite-catalog-error']
  completeBatch(state)
  await page.getByRole('status').getByText('Could not reload model catalogs after syncing: catalog reload failed', { exact: true }).waitFor()
  await closeResults(page)
  await waitForEnabled(syncButton(page))
  assert.equal(requestCount(state, 'accounts'), accountsBefore + 1)
  assert.equal(requestCount(state, 'models'), modelsBefore + 1)
  await accountRow(page, 1).getByText('updated-despite-catalog-error', { exact: true }).waitFor()
})
