import { useCallback, useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { History, Loader2, RefreshCw, ShieldCheck } from 'lucide-react'
import { api } from '@/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import { AdminAPIError } from '@/api'
import {
  normalizeGatewaySettings, toggleGatewayAccount, validateGatewaySettings,
  type GatewayHistoryRecord, type GatewayRuntime, type GatewaySettings, type HarvestAccount, type HarvestPage,
} from '@/lib/codexHarvest'
import HarvestAccountPicker from './HarvestAccountPicker'
import { HarvestPill, HarvestSection, harvestTableClass, harvestTime, type HarvestAction } from './shared'

/**
 * 网关借票面板：对应 GET/PUT /codex-harvest/gateway(/status|/test|/history)。
 * 关键行为（与上游 sub2api 对齐）：
 *  - 草稿独立于轮询快照，未保存的修改在刷新/轮询后保留；
 *  - PUT 409（revision 冲突）提示可刷新重载，确认后放弃本地修改；
 *  - /status 仅读取，不触发探测；prepare/verify/ws 是显式诊断并写入脱敏历史。
 */
export default function HarvestGatewayPanel({ accounts, busy, onAction }: { accounts: HarvestAccount[]; busy: boolean; onAction: HarvestAction }) {
  const { t } = useTranslation()
  const [saved, setSaved] = useState<GatewaySettings | null>(null)
  const [draft, setDraft] = useState<GatewaySettings | null>(null)
  const [runtime, setRuntime] = useState<GatewayRuntime | null>(null)
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')
  const [revisionConflict, setRevisionConflict] = useState(false)
  const [testing, setTesting] = useState(false)
  const [testResult, setTestResult] = useState<GatewayRuntime['last_test']>(undefined)
  const [history, setHistory] = useState<HarvestPage<GatewayHistoryRecord> | null>(null)
  const [historyOpen, setHistoryOpen] = useState(false)
  const [historyOffset, setHistoryOffset] = useState(0)
  const dirty = !!draft && !!saved && JSON.stringify(draft) !== JSON.stringify(saved)
  const validation = draft ? validateGatewaySettings(draft) : ''

  const applySettings = useCallback((value: GatewaySettings) => {
    const normalized = normalizeGatewaySettings(value)
    setSaved(normalized)
    // 只在无未保存修改或首次加载时同步草稿，避免轮询覆盖用户输入。
    setDraft(current => (current && JSON.stringify(current) !== JSON.stringify(normalized) && draftTouched.current ? current : normalized))
  }, [])

  const draftTouched = useRef(false)
  const markTouched = () => { draftTouched.current = true }

  const load = useCallback(async (keepDraft = true) => {
    setLoading(true)
    try {
      const [settings, status] = await Promise.all([api.getCodexGateway(), api.getCodexGatewayStatus()])
      if (!keepDraft || !draftTouched.current) applySettings(settings)
      else setSaved(normalizeGatewaySettings(settings))
      setRuntime(status)
      if (!keepDraft || !draftTouched.current) {
        setError('')
        setRevisionConflict(false)
      }
    } catch (err) {
      setError(getGatewayError(err, t))
    } finally { setLoading(false) }
  }, [applySettings, t])

  useEffect(() => {
    let stopped = false
    let timer: ReturnType<typeof setTimeout>
    const refresh = async () => { try { await load(!stopped) } catch { /* handled in load */ } finally { if (!stopped) timer = setTimeout(() => void refresh(), 5000) } }
    void refresh()
    return () => { stopped = true; clearTimeout(timer) }
  }, [load])

  const save = async () => {
    if (!draft || validation || !dirty) return
    setSaving(true); setError(''); setRevisionConflict(false)
    try {
      // Keep the revision associated with this draft. Fetching a newer token
      // here would allow an old form to overwrite another administrator's save.
      const result = await api.updateCodexGateway(normalizeGatewaySettings(draft))
      draftTouched.current = false
      applySettings(result)
      await load(false)
    } catch (err) {
      if (err instanceof AdminAPIError && err.status === 409) {
        setRevisionConflict(true)
        setError(t('harvest.revisionConflict'))
      } else setError(getGatewayError(err, t))
    } finally { setSaving(false) }
  }

  const discard = async () => {
    draftTouched.current = false
    if (saved) { setDraft(normalizeGatewaySettings(saved)) }
    setRevisionConflict(false); setError('')
    await load(false).catch(() => {})
  }

  const runTest = async (action: 'prepare' | 'verify' | 'ws', accountId: number) => {
    if (!draft?.cookie_pool.enabled) return
    setTesting(true); setError('')
    try {
      const result = await api.testCodexGateway(action, accountId)
      setTestResult(result)
      await load(true)
    } catch (err) { setError(getGatewayError(err, t)) }
    finally { setTesting(false) }
  }

  const loadHistory = useCallback(async (offset: number) => {
    try {
      setHistory(await api.getCodexGatewayHistory(offset))
      setHistoryOffset(offset)
      setHistoryOpen(true)
    } catch (err) { setError(getGatewayError(err, t)) }
  }, [t])

  const update = (patch: (current: GatewaySettings) => GatewaySettings) => {
    markTouched()
    setDraft(current => current ? patch(current) : current)
  }
  const toggleList = (list: number[], id: number, ...others: number[][]) => {
    markTouched()
    return toggleGatewayAccount(list, id, others.flat())
  }

  if (loading && !saved) return <div role="status" className="flex items-center gap-2 py-10 text-sm text-muted-foreground"><Loader2 className="size-4 animate-spin motion-reduce:animate-none" />{t('common.loading')}</div>

  return <div className="space-y-5">
    {error && <div role="alert" className="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive"><span>{error}</span>{revisionConflict && <Button size="sm" variant="outline" onClick={() => void discard()}>{t('harvest.reloadDraft')}</Button>}</div>}
    <HarvestSection title={t('harvest.gateway')} actions={<div className="flex flex-wrap items-center gap-2">
      {dirty && <HarvestPill>{t('harvest.unsaved')}</HarvestPill>}
      {dirty && <Button size="sm" variant="outline" disabled={busy || saving} onClick={() => void discard()}>{t('harvest.discard')}</Button>}
      <Button size="sm" variant="ghost" disabled={busy || saving} onClick={() => void load(false)}><RefreshCw className={cnIcon(loading)} />{t('harvest.refresh')}</Button>
      <Button size="sm" disabled={busy || saving || !dirty || !!validation} onClick={() => void save()}>{saving ? t('harvest.saving') : t('harvest.save')}</Button>
    </div>}>
      {draft && (
        <div className="space-y-5">
          {validation && <p role="alert" className="text-sm text-amber-600">{t(`harvest.gatewayErrors.${validation}`)}</p>}
          <div className="rounded-lg border border-border p-4">
          <div className="flex flex-wrap items-start justify-between gap-4">
            <div><h4 className="text-sm font-medium">{t('harvest.cookiePool')}</h4><p className="mt-1 max-w-xl text-xs leading-relaxed text-muted-foreground">{t('harvest.cookiePoolHint')}</p></div>
            <label className="flex items-center gap-2 text-sm"><Switch aria-label={t('harvest.cookiePool')} checked={draft.cookie_pool.enabled} disabled={busy || saving} onCheckedChange={enabled => update(current => ({ ...current, cookie_pool: { ...current.cookie_pool, enabled }, ws_session: { ...current.ws_session, enabled: enabled && current.ws_session.enabled } }))} />{t(draft.cookie_pool.enabled ? 'harvest.enabled' : 'harvest.disabled')}</label>
          </div>
          <div className="mt-4 grid gap-4 lg:grid-cols-2">
            <HarvestAccountPicker label={t('harvest.sources')} accounts={accounts} selected={draft.cookie_pool.source_account_ids}
              blocked={[...draft.cookie_pool.target_account_ids]}
              disabled={busy || saving}
              onToggle={id => update(current => ({ ...current, cookie_pool: { ...current.cookie_pool, source_account_ids: toggleList(current.cookie_pool.source_account_ids, id, current.cookie_pool.target_account_ids) } }))} />
            <HarvestAccountPicker label={t('harvest.targets')} accounts={accounts} selected={draft.cookie_pool.target_account_ids}
              blocked={[...draft.cookie_pool.source_account_ids]}
              disabled={busy || saving}
              onToggle={id => update(current => {
                const target_account_ids = toggleList(current.cookie_pool.target_account_ids, id, current.cookie_pool.source_account_ids)
                const account_ids = current.ws_session.account_ids.filter(accountID => target_account_ids.includes(accountID))
                return { ...current, cookie_pool: { ...current.cookie_pool, target_account_ids }, ws_session: { ...current.ws_session, account_ids, enabled: current.ws_session.enabled && account_ids.length > 0 } }
              })} />
          </div>
          <div className="mt-4 grid gap-4 sm:grid-cols-2">
            <label className="flex items-center justify-between gap-4 rounded-lg border border-border p-3 text-sm"><span>{t('harvest.ipAffinity')}<span className="mt-1 block text-xs font-normal text-muted-foreground">{t('harvest.ipAffinityHint')}</span></span><Switch aria-label={t('harvest.ipAffinity')} checked={draft.cookie_pool.ip_affinity} disabled={busy || saving} onCheckedChange={enabled => update(current => ({ ...current, cookie_pool: { ...current.cookie_pool, ip_affinity: enabled, rotate_nodes: enabled ? current.cookie_pool.rotate_nodes : false } }))} /></label>
            <label className="flex items-center justify-between gap-4 rounded-lg border border-border p-3 text-sm"><span>{t('harvest.rotateNodes')}<span className="mt-1 block text-xs font-normal text-muted-foreground">{t('harvest.rotateNodesHint')}</span></span><Switch aria-label={t('harvest.rotateNodes')} checked={draft.cookie_pool.rotate_nodes} disabled={busy || saving} onCheckedChange={enabled => update(current => ({ ...current, cookie_pool: { ...current.cookie_pool, rotate_nodes: enabled, ip_affinity: enabled ? true : current.cookie_pool.ip_affinity } }))} /></label>
          </div>
          {draft.cookie_pool.rotate_nodes && <div className="mt-3 grid gap-4 sm:grid-cols-2">
            <label className="space-y-2 text-sm"><span>{t('harvest.maxNodeAttempts')}</span><Input type="number" min={1} max={10} required disabled={busy || saving} value={draft.cookie_pool.max_node_attempts} onChange={event => update(current => ({ ...current, cookie_pool: { ...current.cookie_pool, max_node_attempts: Number(event.target.value) } }))} /></label>
            <label className="space-y-2 text-sm"><span>{t('harvest.nodeCooldown')}</span><Input type="number" min={60} max={86400} required disabled={busy || saving} value={draft.cookie_pool.node_cooldown_seconds} onChange={event => update(current => ({ ...current, cookie_pool: { ...current.cookie_pool, node_cooldown_seconds: Number(event.target.value) } }))} /></label>
          </div>}
          <label className="mt-3 block max-w-xs space-y-2 text-sm"><span>{t('harvest.cookieTTL')}</span><Input type="number" min={30} max={240} required disabled={busy || saving} value={draft.cookie_pool.ttl_seconds} onChange={event => update(current => ({ ...current, cookie_pool: { ...current.cookie_pool, ttl_seconds: Number(event.target.value) } }))} /></label>
          <p className="mt-2 text-xs leading-relaxed text-muted-foreground">{t('harvest.cookieTTLHint')}</p>
          </div>
          <div className="rounded-lg border border-border p-4">
          <div className="flex flex-wrap items-start justify-between gap-4">
            <div><h4 className="text-sm font-medium">{t('harvest.wsSession')}</h4><p className="mt-1 max-w-xl text-xs leading-relaxed text-muted-foreground">{t('harvest.wsSessionHint')}</p></div>
            <label className="flex items-center gap-2 text-sm"><Switch aria-label={t('harvest.wsSession')} checked={draft.ws_session.enabled} disabled={busy || saving} onCheckedChange={enabled => update(current => ({ ...current, ws_session: { ...current.ws_session, enabled }, cookie_pool: { ...current.cookie_pool, enabled: enabled || current.cookie_pool.enabled } }))} />{t(draft.ws_session.enabled ? 'harvest.enabled' : 'harvest.disabled')}</label>
          </div>
          <div className="mt-4"><HarvestAccountPicker label={t('harvest.wsAccounts')} accounts={accounts} selected={draft.ws_session.account_ids}
            blocked={[...draft.cookie_pool.source_account_ids]}
            disabled={busy || saving}
            onToggle={id => update(current => {
              const account_ids = toggleList(current.ws_session.account_ids, id, current.cookie_pool.source_account_ids)
              const target_account_ids = [...new Set([...current.cookie_pool.target_account_ids, ...account_ids])]
              return { ...current, ws_session: { ...current.ws_session, account_ids }, cookie_pool: { ...current.cookie_pool, target_account_ids } }
            })} /></div>
          <label className="mt-3 block max-w-xs space-y-2 text-sm"><span>{t('harvest.wsTTL')}</span><Input type="number" min={60} max={3600} required disabled={busy || saving} value={draft.ws_session.ttl_seconds} onChange={event => update(current => ({ ...current, ws_session: { ...current.ws_session, ttl_seconds: Number(event.target.value) } }))} /></label>
          <p className="mt-2 text-xs leading-relaxed text-muted-foreground">{t('harvest.wsNoSSEHint')}</p>
          </div>
          <div className="rounded-lg border border-border p-4">
          <div className="flex flex-wrap items-start justify-between gap-4">
            <div><h4 className="text-sm font-medium">{t('harvest.poolScheduling')}</h4><p className="mt-1 max-w-xl text-xs leading-relaxed text-muted-foreground">{t('harvest.poolSchedulingHint')}</p></div>
            <label className="flex items-center gap-2 text-sm"><Switch aria-label={t('harvest.poolScheduling')} checked={draft.ticket_pool_scheduling} disabled={busy || saving} onCheckedChange={ticket_pool_scheduling => update(current => ({ ...current, ticket_pool_scheduling }))} />{t(draft.ticket_pool_scheduling ? 'harvest.enabled' : 'harvest.disabled')}</label>
          </div>
          {draft.ticket_pool_scheduling && (
            <div className="mt-4 grid gap-4 sm:grid-cols-2">
              <label className="space-y-2 text-sm">
                <span>{t('harvest.schedulingMode')}</span>
                <Select
                  aria-label={t('harvest.schedulingMode')}
                  disabled={busy || saving}
                  value={draft.scheduling_mode}
                  onValueChange={mode => update(current => ({ ...current, scheduling_mode: mode as GatewaySettings['scheduling_mode'] }))}
                  options={[
                    { value: 'account', label: t('harvest.schedulingAccount') },
                    { value: 'model', label: t('harvest.schedulingModel') },
                    { value: 'groups', label: t('harvest.schedulingGroups') },
                  ]}
                />
              </label>
              {draft.scheduling_mode === 'groups' && (
                <label className="space-y-2 text-sm">
                  <span>{t('harvest.schedulingGroupIDs')}</span>
                  <Input
                    value={draft.scheduling_group_ids.join(', ')}
                    disabled={busy || saving}
                    onChange={event =>
                      update(current => ({
                        ...current,
                        scheduling_group_ids: [
                          ...new Set(
                            event.target.value
                              .split(/[\s,，]+/)
                              .map(item => Number(item.trim()))
                              .filter(id => Number.isInteger(id) && id > 0),
                          ),
                        ],
                      }))
                    }
                  />
                </label>
              )}
            </div>
          )}
          </div>
        </div>
      )}
    </HarvestSection>

    {runtime && (
      <HarvestSection title={t('harvest.gatewayRuntime')} actions={<Button size="sm" disabled={busy || testing || !draft?.cookie_pool.enabled || dirty} onClick={() => void runTest('prepare', 0)}>{testing ? t('harvest.testing') : t('harvest.prepare')}</Button>}>
      <div className="flex flex-wrap items-center gap-2">
        <HarvestPill good={runtime.mode !== 'disabled'}>{t(draft?.cookie_pool.enabled ? (runtime.mode === 'unified_ticket' ? 'harvest.modeUnified' : runtime.mode === 'cookie_route' ? 'harvest.modeCookieRoute' : 'harvest.modeDisabled') : 'harvest.modeDisabled')}</HarvestPill>
        <HarvestPill good={runtime.ready_routes > 0}>{t('harvest.readyRoutes', { count: runtime.ready_routes })}</HarvestPill>
        {runtime.preparing && <span role="status" className="inline-flex items-center gap-1 text-xs text-primary"><Loader2 className="size-3.5 animate-spin motion-reduce:animate-none" />{t('harvest.preparing')}</span>}
        {dirty && <span className="text-xs text-amber-600">{t('harvest.saveFirst')}</span>}
      </div>
      <p className="mt-3 text-xs leading-relaxed text-muted-foreground">{t('harvest.probeHint')}</p>
      <div className="mt-2 rounded-lg border border-border p-3 text-sm">
        {t('harvest.automaticStatus')}：{t(`harvest.setupStates.${runtime.setup.state}`, { defaultValue: runtime.setup.state })}{runtime.setup.account_id ? ` #${runtime.setup.account_id}` : ''}{runtime.setup.reason ? ` · ${t(`harvest.gatewayReasons.${runtime.setup.reason}`, { defaultValue: runtime.setup.reason })}` : ''}
      </div>
      <div className="mt-4 grid gap-4 lg:grid-cols-2">
        {(['sources', 'targets'] as const).map(key => <div key={key} className="overflow-x-auto">
          <h4 className="mb-2 text-xs font-medium text-muted-foreground">{t(`harvest.${key}`)}</h4>
          <table className={harvestTableClass}><thead><tr>{['account', 'state', 'gatewayHost', 'remaining'].map(col => <th key={col}>{t(`harvest.${col === 'remaining' ? 'remainingSeconds' : col}`)}</th>)}</tr></thead>
            <tbody>{runtime[key].map(row => <tr key={`${key}-${row.account_id}`}>
              <td className="font-mono text-xs">#{row.account_id}</td>
              <td><HarvestPill good={row.state === 'ready'}>{t(`harvest.routeStates.${row.state}`, { defaultValue: row.state })}</HarvestPill><span className="mt-1 block text-xs text-muted-foreground">{t(`harvest.gatewayReasons.${row.reason}`, { defaultValue: row.reason })}</span></td>
              <td className="max-w-40 break-all font-mono text-xs">{row.gateway || '—'}{row.proxy_node && <span className="block text-muted-foreground">{row.proxy_node}</span>}</td>
              <td className="whitespace-nowrap font-mono text-xs">{row.state === 'ready' && row.remaining_seconds > 0 ? `${row.remaining_seconds}s` : '—'}</td>
            </tr>)}</tbody></table>
        </div>)}
        <div className="space-y-3">
          <h4 className="text-xs font-medium text-muted-foreground">{t('harvest.wsAccounts')}</h4>
          {runtime.ws.length === 0 ? <p className="text-sm text-muted-foreground">{t('harvest.empty')}</p> : runtime.ws.map(row => <div key={`ws-${row.account_id}`} className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-border p-3">
            <div className="text-sm"><span className="font-mono text-xs">#{row.account_id}</span><HarvestPill good={row.ready}>{t(row.ready ? 'harvest.ready' : 'harvest.empty')}</HarvestPill><span className="ml-2 text-xs text-muted-foreground">{t('harvest.wsSessions', { count: row.active_sessions })} · {t(`harvest.gatewayReasons.${row.reason}`, { defaultValue: row.reason })}</span></div>
            <Button size="xs" variant="outline" disabled={busy || testing || dirty || !draft?.cookie_pool.enabled || !draft.ws_session.enabled} onClick={() => void runTest('ws', row.account_id)}>{t('harvest.verifyWS')}</Button>
          </div>)}
        </div>
      </div>
      {runtime.targets.length > 0 && <div className="mt-4 flex flex-wrap gap-2">{runtime.targets.map(row => <Button key={`verify-${row.account_id}`} size="xs" variant="outline" disabled={busy || testing || dirty || !draft?.cookie_pool.enabled} onClick={() => void runTest('verify', row.account_id)}>{t('harvest.verifyTarget')} #{row.account_id}</Button>)}</div>}
      {(testResult ?? runtime.last_test) && <p role="status" className="mt-3 break-words rounded-lg bg-muted/60 p-3 text-xs text-muted-foreground">{harvestTime((testResult ?? runtime.last_test)!.checked_at)} · {t(`harvest.testActions.${(testResult ?? runtime.last_test)!.action}`, { defaultValue: (testResult ?? runtime.last_test)!.action })}{(testResult ?? runtime.last_test)!.account_id ? ` #${(testResult ?? runtime.last_test)!.account_id}` : ''} · {(testResult ?? runtime.last_test)!.success ? t('harvest.success') : t('harvest.failed')} · {t(`harvest.gatewayReasons.${(testResult ?? runtime.last_test)!.reason}`, { defaultValue: (testResult ?? runtime.last_test)!.reason })} · {((testResult ?? runtime.last_test)!.duration_ms / 1000).toFixed(1)}s</p>}
    </HarvestSection>
    )}

    <HarvestSection title={t('harvest.gatewayHistory')} actions={<div className="flex gap-2">
      {historyOpen && <Button size="sm" variant="outline" disabled={busy} onClick={() => void loadHistory(historyOffset)}><RefreshCw className="size-4" /></Button>}
      <Button size="sm" variant={historyOpen ? 'ghost' : 'outline'} disabled={busy} onClick={() => (historyOpen ? setHistoryOpen(false) : void loadHistory(0))}><History className="size-4" />{t(historyOpen ? 'harvest.hide' : 'harvest.show')}</Button>
    </div>}>
      <p className="mb-3 flex items-center gap-1.5 text-xs leading-relaxed text-muted-foreground"><ShieldCheck className="size-3.5 shrink-0" />{t('harvest.historyMasked')}</p>
      {historyOpen && (history === null ? <div role="status" className="flex items-center gap-2 py-6 text-sm text-muted-foreground"><Loader2 className="size-4 animate-spin motion-reduce:animate-none" />{t('common.loading')}</div>
        : history.items.length === 0 ? <p className="py-6 text-sm text-muted-foreground">{t('harvest.noRecords')}</p>
        : <div className="overflow-x-auto"><table className={harvestTableClass}><thead><tr>{['time', 'action', 'source', 'target', 'gatewayHost', 'result'].map(col => <th key={col}>{t(`harvest.${col}`)}</th>)}</tr></thead>
          <tbody>{history.items.map(record => <tr key={record.id}>
            <td className="whitespace-nowrap text-xs">{harvestTime(record.checked_at)}</td>
            <td className="whitespace-nowrap text-xs">{t(`harvest.historyActions.${record.action}`, { defaultValue: record.action })}</td>
            <td className="font-mono text-xs">#{record.source_account_id || '—'}</td>
            <td className="font-mono text-xs">#{record.target_account_id}</td>
            <td className="max-w-40 break-all font-mono text-xs">{record.gateway || '—'}</td>
            <td><HarvestPill good={record.success}>{t(record.success ? 'harvest.success' : 'harvest.failed')}</HarvestPill><span className="mt-1 block text-xs text-muted-foreground">{t(`harvest.gatewayReasons.${record.reason}`, { defaultValue: record.reason })}</span></td>
          </tr>)}</tbody></table>
          <div className="mt-4 flex flex-wrap items-center justify-between gap-3 text-xs text-muted-foreground"><span>{t('harvest.total', { count: history.total })}</span><div className="flex gap-2"><Button size="sm" variant="outline" disabled={historyOffset === 0} onClick={() => void loadHistory(Math.max(0, historyOffset - 25))}>{t('harvest.previous')}</Button><Button size="sm" variant="outline" disabled={historyOffset + 25 >= history.total} onClick={() => void loadHistory(historyOffset + 25)}>{t('harvest.next')}</Button></div></div>
        </div>)}
    </HarvestSection>
  </div>
}

function cnIcon(loading: boolean): string { return `size-4 ${loading ? 'animate-spin motion-reduce:animate-none' : ''}` }

function getGatewayError(err: unknown, t: (key: string, options?: Record<string, unknown>) => string): string {
  if (err instanceof AdminAPIError && err.status === 409) return t('harvest.revisionConflict')
  const message = err instanceof Error ? err.message : String(err)
  return message || t('harvest.gatewayError')
}
