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
          import {harvestEn} from '@/locales/harvest.ts';
          i18next.use(initReactI18next).init({lng:'en', fallbackLng:'en', resources:{en:{translation:{harvest:harvestEn,common:{loading:'Loading'}}}}, interpolation:{escapeValue:false}}).then(() => {
            const root = createRoot(document.getElementById('root'));
            const accounts = [1,2,3].map(id => ({id,email:'account-'+id,group_ids:[],eligible:true,skipped:false,busy:false,tickets:[]}));
            function Picker() { const [selected,setSelected] = React.useState([]); return React.createElement(ProxyPicker,{selected,disabled:false,onChange:ids=>{window.__selected=ids;setSelected(ids)}}) }
            root.render(window.__component === 'picker' ? React.createElement(Picker) : React.createElement(GatewayPanel,{accounts,busy:false,onAction:fn=>fn()}));
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

async function fixture(t, component = 'gateway') {
  const page = await browser.newPage()
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  page.setDefaultTimeout(8000)
  t.after(async () => { await page.close(); assert.deepEqual(errors, []) })
  await page.route('http://harvest.test/**', route => route.fulfill({contentType:'text/html',body:'<!doctype html><div id="root"></div>'}))
  await page.goto('http://harvest.test/')
  await page.evaluate(component => {
    window.__component = component
    window.__writes = []; window.__diagnostics = []; window.__selected = []
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
  }, component)
  await page.addScriptTag({content:script})
  await page.waitForFunction(() => document.getElementById('root').childElementCount > 0).catch(async error => { t.diagnostic(JSON.stringify({scriptSize:script.length, html:await page.content()})); throw error })
  return page
}

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
