import assert from 'node:assert/strict'
import { before, after, test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { build } from 'vite'
import { chromium } from 'playwright'
import { harvestEn } from '../src/locales/harvest.ts'

const root = fileURLToPath(new URL('../', import.meta.url))
const apiPath = fileURLToPath(new URL('../src/api', import.meta.url)).replaceAll('\\', '/')
let browser, script
before(async () => {
  const bundle = await build({
    configFile: false, root, logLevel: 'silent',
    resolve: { alias: { '@': `${root}src` } },
    define: { 'process.env.NODE_ENV': '"production"' },
    esbuild: { jsx: 'automatic' },
    plugins: [{
      name: 'harvest-regression-harness', enforce: 'pre',
      resolveId(id) {
        if (id === 'virtual:harvest-harness') return '\0harvest-harness'
        if (id === '@/api' || id.replaceAll('\\', '/') === apiPath || id.replaceAll('\\', '/') === `${apiPath}.ts`) return '\0harvest-api'
      },
      load(id) {
        if (id === '\0harvest-api') return `
          export class AdminAPIError extends Error { constructor(status) { super('revision conflict'); this.status = status } }
          export const api = {
            getCodexGateway: async () => structuredClone(window.__settings),
            getCodexGatewayStatus: async () => ({ revision: window.__settings.revision, generated_at: new Date().toISOString(), mode: 'cookie_route', preparing: false, ready_routes: 0, sources: [], targets: [], ws: [{account_id:3, active_sessions:0, ready:false, reason:'waiting'}], setup: {state:'ready', reason:'', account_id:0} }),
            updateCodexGateway: async value => {
              window.__writes.push(structuredClone(value));
              if (value.revision !== window.__settings.revision) throw new AdminAPIError(409);
              window.__settings = {...structuredClone(value), revision: 'saved-revision'};
              return structuredClone(window.__settings);
            },
            testCodexGateway: async (action, account_id) => { window.__diagnostics.push({action,account_id}); return { action, account_id, success:true, reason:'success', duration_ms:1, checked_at:new Date().toISOString() } },
            getCodexGatewayHistory: async () => ({items:[],total:0}),
            listProxies: async () => ({proxies: window.__proxies}),
          }
        `
        if (id !== '\0harvest-harness') return
        return `
          import React from 'react'; import {createRoot} from 'react-dom/client';
          import i18next from 'i18next'; import {initReactI18next} from 'react-i18next';
          import GatewayPanel from '@/components/harvest/HarvestGatewayPanel.tsx';
          import ProxyPicker from '@/components/harvest/HarvestProxyPicker.tsx';
          import AccountsPanel from '@/components/harvest/HarvestAccountsPanel.tsx';
          import {harvestEn} from '@/locales/harvest.ts';
          i18next.use(initReactI18next).init({lng:'en', fallbackLng:'en', resources:{en:{translation:{harvest:harvestEn,common:{loading:'Loading'}}}}, interpolation:{escapeValue:false}}).then(() => {
            const root = createRoot(document.getElementById('root'));
            const accounts = [1,2,3].map(id => ({id,email:'account-'+id,group_ids:[],eligible:true,skipped:false,busy:false,tickets:[]}));
            function Gateway() {
              const [rows,setRows] = React.useState(accounts);
              window.__setGatewayAccounts = setRows;
              return React.createElement(GatewayPanel,{accounts:rows,busy:false,onAction:fn=>fn()});
            }
            function Picker() { const [selected,setSelected] = React.useState([]); return React.createElement(ProxyPicker,{selected,disabled:false,onChange:ids=>{window.__selected=ids;setSelected(ids)}}) }
            function Accounts() {
              const [rows,setRows] = React.useState(window.__accounts);
              window.__setAccounts = setRows;
              return React.createElement(AccountsPanel,{accounts:rows,busy:false,onSkipChange:(id,skipped)=>{
                window.__skipChanges.push({id,skipped});
                setRows(current=>current.map(account=>account.id===id ? {...account,skipped} : account));
              }});
            }
            root.render(window.__component === 'accounts' ? React.createElement(Accounts) : window.__component === 'picker' ? React.createElement(Picker) : React.createElement(Gateway));
          });
        `
      },
    }],
    build: { write: false, minify: false, emptyOutDir: false, lib: {entry:'virtual:harvest-harness',formats:['iife'],name:'HarvestHarness'}, rollupOptions:{input:'virtual:harvest-harness'} },
  })
  script = (Array.isArray(bundle) ? bundle[0] : bundle).output.find(item => item.type === 'chunk').code
  browser = await chromium.launch({headless:true})
})
after(async () => { await browser?.close() })

async function fixture(t, component = 'gateway', ticketOverrides = []) {
  const page = await browser.newPage()
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  page.setDefaultTimeout(8000)
  t.after(async () => { await page.close(); assert.deepEqual(errors, []) })
  await page.route('http://harvest.test/**', route => route.fulfill({contentType:'text/html',body:'<!doctype html><div id="root"></div>'}))
  await page.goto('http://harvest.test/')
  await page.evaluate(({ component, ticketOverrides }) => {
    window.__component = component
    window.__writes = []; window.__diagnostics = []; window.__selected = []; window.__skipChanges = []
    window.__accounts = ticketOverrides.map((ticket, index) => ({
      id: index + 1, email: 'account-' + (index + 1), group_ids: [], eligible: true, skipped: false, busy: false,
      tickets: [{ model: 'gpt-6-astra', ready: false, probe_verified: false, revoked: false, length: 0, remaining_seconds: 0,
        cookie_count: 0, cookie_remaining_seconds: 0, cookie_expired: false, egress_bound: false, session_bound: false, standby_ready: false, ...ticket }],
    }))
    window.__settings = {
      cookie_pool:{enabled:true,source_account_ids:[1],target_account_ids:[3],ip_affinity:false,rotate_nodes:false,max_node_attempts:3,node_cooldown_seconds:3600,ttl_seconds:230},
      ws_session:{enabled:true,account_ids:[3],ttl_seconds:3600},
      ticket_pool_scheduling:false,scheduling_mode:'account',scheduling_group_ids:[],revision:'revision-1',route_revision:'route-1',
    }
    window.__proxies = [
      {id:10,label:'Proxy alpha',enabled:true,test_status:'ok',test_location:'Tokyo',url:'http://private:credential@proxy.invalid'},
      {id:11,label:'Proxy beta',enabled:true,test_status:'ok',test_location:'London',url:'http://private:credential@proxy2.invalid'},
      {id:12,label:'Proxy disabled',enabled:false,test_status:'ok',test_location:'',url:'http://proxy3.invalid'},
    ]
  }, { component, ticketOverrides })
  await page.addScriptTag({content:script})
  await page.waitForFunction(() => document.getElementById('root').childElementCount > 0).catch(async error => { t.diagnostic(JSON.stringify({scriptSize:script.length, html:await page.content()})); throw error })
  return page
}

test('account table separates current holdings from historical verification', async t => {
  const cases = [
    { ticket: { ready: true, probe_verified: true, length: 780, remaining_seconds: 90, transport: 'sse' }, label: harvestEn.probeVerified, good: true },
    { ticket: { ready: false, probe_verified: true, transport: 'websocket' }, label: harvestEn.probePreviouslyVerified },
    { ticket: { ready: false, probe_verified: true, revoked: true }, label: harvestEn.probePreviouslyVerified },
    { ticket: { ready: false, probe_verified: true, cookie_expired: true }, label: harvestEn.probePreviouslyVerified },
    { ticket: { ready: false, probe_verified: false }, label: harvestEn.probeNoRecord },
    { ticket: { ready: true, probe_verified: false, length: 292 }, label: harvestEn.probeNotRequired },
    { ticket: { ready: true, probe_verified: false, length: 332 }, label: harvestEn.probeNotRequired },
    { ticket: { ready: true, probe_verified: true, revoked: true, length: 780 }, label: harvestEn.probeVerified, good: true },
  ]
  const page = await fixture(t, 'accounts', cases.map(tc => tc.ticket))
  const headings = await page.getByRole('columnheader').allTextContents()
  assert.ok(headings.indexOf(harvestEn.ticketHolding) < headings.indexOf(harvestEn.probeHistory))
  await page.getByText(harvestEn.ticketStatusHint, { exact: true }).waitFor()
  for (const [index, { ticket, label, good }] of cases.entries()) {
    const row = page.getByRole('row').filter({ has: page.getByText(`account-${index + 1}`, { exact: true }) })
    await row.getByText(ticket.ready ? harvestEn.ticketHeld : harvestEn.ticketUnavailable, { exact: true }).waitFor()
    const badge = row.getByText(label, { exact: true })
    await badge.waitFor()
    assert.equal((await badge.getAttribute('class')).includes('text-emerald-700'), !!good)
    assert.equal(await row.getByText(harvestEn.probePending, { exact: true }).count(), 0)
  }
  assert.deepEqual(await page.evaluate(() => window.__diagnostics), [])
})

test('ticket expiry removes positive verification styling and fresh tickets restore it', async t => {
  const page = await fixture(t, 'accounts', [{ ready: true, probe_verified: true, length: 780, remaining_seconds: 90 }])
  await page.getByText(harvestEn.probeVerified, { exact: true }).waitFor()
  // Simulate the next polling snapshot retaining probe_verified after expiry.
  await page.evaluate(() => window.__setAccounts(rows => rows.map(account => ({
    ...account, tickets: account.tickets.map(ticket => ({ ...ticket, ready: false, remaining_seconds: 0, cookie_expired: true })),
  }))))
  await page.getByText(harvestEn.probePreviouslyVerified, { exact: true }).waitFor()
  assert.equal(await page.getByText(harvestEn.ticketUnavailable, { exact: true }).count(), 1)
  assert.equal(await page.getByText(harvestEn.probeVerified, { exact: true }).count(), 0)
  assert.equal(await page.locator('tbody .text-emerald-700').count(), 0)
  await page.evaluate(() => window.__setAccounts(rows => rows.map(account => ({
    ...account, tickets: account.tickets.map(ticket => ({ ...ticket, ready: true, remaining_seconds: 120, cookie_expired: false })),
  }))))
  await page.getByText(harvestEn.probeVerified, { exact: true }).waitFor()
  assert.equal(await page.getByText(harvestEn.ticketHeld, { exact: true }).count(), 1)
  await page.getByRole('switch', { name: `${harvestEn.skip} #1`, exact: true }).click()
  assert.deepEqual(await page.evaluate(() => window.__skipChanges), [{ id: 1, skipped: true }])
})

test('account table tolerates missing ticket lists and empty accounts', async t => {
  const page = await fixture(t, 'accounts', [{}])
  await page.getByText('account-1', { exact: true }).waitFor()
  await page.evaluate(() => window.__setAccounts(rows => rows.map(account => ({ ...account, tickets: null }))))
  await page.waitForFunction(() => !document.body.textContent.includes('No passed check recorded'))
  assert.equal(await page.getByText(harvestEn.probePending, { exact: true }).count(), 0)
  await page.evaluate(() => window.__setAccounts([]))
  await page.getByText(harvestEn.noAccounts, { exact: true }).waitFor()
})

test('stale draft keeps its own revision and cannot overwrite a concurrent save', async t => {
  const page = await fixture(t)
  await page.getByRole('switch', {name:harvestEn.ipAffinity,exact:true}).click()
  await page.evaluate(() => { window.__settings = {...window.__settings, revision:'revision-2', ticket_pool_scheduling:true} })
  await page.getByRole('button',{name:harvestEn.save,exact:true}).click()
  await page.getByRole('alert').getByText(harvestEn.revisionConflict,{exact:true}).waitFor()
  assert.equal(await page.evaluate(() => window.__writes[0].revision), 'revision-1')
  assert.equal(await page.evaluate(() => window.__settings.ticket_pool_scheduling), true)
  // Real polling must not silently discard the unsaved conflict.
  await page.waitForTimeout(5200)
  assert.equal(await page.getByRole('alert').getByText(harvestEn.revisionConflict,{exact:true}).count(), 1)
  await page.getByRole('button',{name:harvestEn.reloadDraft,exact:true}).click()
  await page.getByRole('button',{name:harvestEn.save,exact:true}).waitFor()
  await page.waitForFunction(() => !document.body.textContent.includes('Unsaved changes'))
})

test('disabling the cookie pool also disables dependent WS sessions on save', async t => {
  const page = await fixture(t)
  await page.getByRole('switch', { name: harvestEn.cookiePool, exact: true }).click()
  assert.equal(await page.getByRole('switch', { name: harvestEn.wsSession, exact: true }).isChecked(), false)
  await page.getByRole('button', { name: harvestEn.save, exact: true }).click()
  await page.waitForFunction(() => window.__writes.length === 1)
  const value = await page.evaluate(() => window.__writes[0])
  assert.equal(value.cookie_pool.enabled, false)
  assert.equal(value.ws_session.enabled, false)
})

test('deleted selected target stays removable and cannot be restored by WS normalization', async t => {
  const page = await fixture(t)
  const targets = page.getByTestId(`harvest-account-picker-${harvestEn.targets}`)
  await targets.getByRole('checkbox', { name: /#3/ }).waitFor()
  await page.evaluate(() => window.__setGatewayAccounts(rows => rows.filter(account => account.id !== 3)))
  await targets.getByText(harvestEn.accountMissing, { exact: false }).waitFor()
  // Keep borrowing enabled with a replacement, then remove the stale selection.
  await targets.getByRole('checkbox', { name: /#2/ }).check()
  await targets.getByRole('checkbox', { name: /#3/ }).click()
  await page.getByRole('button', { name: harvestEn.save, exact: true }).click()
  await page.waitForFunction(() => window.__writes.length === 1)
  const value = await page.evaluate(() => window.__writes[0])
  assert.deepEqual(value.cookie_pool.target_account_ids, [2])
  assert.deepEqual(value.ws_session.account_ids, [])
  assert.equal(value.ws_session.enabled, false)
})

test('deleted selected source stays removable when the account list becomes empty', async t => {
  const page = await fixture(t)
  await page.getByRole('switch', { name: harvestEn.cookiePool, exact: true }).click()
  await page.evaluate(() => window.__setGatewayAccounts([]))
  for (const label of [harvestEn.sources, harvestEn.targets]) {
    const picker = page.getByTestId(`harvest-account-picker-${label}`)
    await picker.getByText(harvestEn.accountMissing, { exact: false }).waitFor()
    await picker.getByRole('checkbox').click()
  }
  await page.getByRole('button', { name: harvestEn.save, exact: true }).click()
  await page.waitForFunction(() => window.__writes.length === 1)
  const value = await page.evaluate(() => window.__writes[0])
  assert.deepEqual(value.cookie_pool.source_account_ids, [])
  assert.deepEqual(value.cookie_pool.target_account_ids, [])
  assert.deepEqual(value.ws_session.account_ids, [])
  assert.equal(value.cookie_pool.enabled, false)
})

test('WS diagnostic is available before any business session exists', async t => {
  const page = await fixture(t)
  const button = page.getByRole('button',{name:harvestEn.verifyWS,exact:true})
  await button.waitFor()
  assert.equal(await button.isEnabled(), true)
  await button.click()
  await page.waitForFunction(() => window.__diagnostics.length === 1)
  assert.deepEqual(await page.evaluate(() => window.__diagnostics[0]), {action:'ws',account_id:3})
})

test('proxy picker selects real proxy IDs, rejects disabled rows and hides credentials', async t => {
  const page = await fixture(t, 'picker')
  await page.getByRole('checkbox',{name:/Proxy alpha/}).check()
  await page.getByRole('checkbox',{name:/Proxy beta/}).check()
  assert.deepEqual(await page.evaluate(() => window.__selected), [10,11])
  assert.equal(await page.getByRole('checkbox',{name:/Proxy disabled/}).isDisabled(), true)
  assert.equal(await page.locator('body').textContent().then(text => text.includes('private:credential')), false)
  await page.getByRole('checkbox',{name:/Proxy alpha/}).uncheck()
  assert.deepEqual(await page.evaluate(() => window.__selected), [11])
})
