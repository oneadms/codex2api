import { useCallback, useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { ArrowRight, Loader2, Play, Ticket } from 'lucide-react'
import { api } from '@/api'
import PageHeader from '@/components/PageHeader'
import CodexTicketSettings from '@/components/CodexTicketSettings'
import { Button } from '@/components/ui/button'
import { Switch } from '@/components/ui/switch'
import MihomoPanel from '@/components/harvest/MihomoPanel'
import HarvestControlsPanel from '@/components/harvest/HarvestControlsPanel'
import HarvestManualPanel from '@/components/harvest/HarvestManualPanel'
import HarvestRecordsPanel from '@/components/harvest/HarvestRecordsPanel'
import { HarvestPill, HarvestSection, harvestTableClass, harvestTime, type HarvestAction } from '@/components/harvest/shared'
import type { HarvestSnapshot, MihomoStatus } from '@/lib/codexHarvest'
import { getErrorMessage } from '@/utils/error'
import { useToast } from '@/hooks/useToast'
import { cn } from '@/lib/utils'

type HarvestTab = 'overview' | 'mihomo' | 'manual' | 'records'

export default function CodexHarvest() {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const [snapshot, setSnapshot] = useState<HarvestSnapshot | null>(null)
  const [mihomo, setMihomo] = useState<MihomoStatus | null>(null)
  const [tab, setTab] = useState<HarvestTab>('overview')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [configRevision, setConfigRevision] = useState(0)
  const load = useCallback(async () => {
    const [state, kernel] = await Promise.all([api.getCodexHarvest(), api.getMihomo()])
    setSnapshot(state); setMihomo(kernel)
  }, [])
  useEffect(() => {
    let stopped = false
    let timer: ReturnType<typeof setTimeout>
    // 完成上一轮后再安排轮询，避免内核操作较慢时堆积请求。
    const refresh = async () => { try { await load() } catch (err) { if (!stopped) setError(getErrorMessage(err)) } finally { if (!stopped) timer = setTimeout(() => void refresh(), 5000) } }
    void refresh()
    return () => { stopped = true; clearTimeout(timer) }
  }, [load])
  const onAction: HarvestAction = async action => {
    setBusy(true); setError('')
    try { await action(); await load(); showToast(t('harvest.submitted'), 'success'); return true }
    catch (err) { setError(getErrorMessage(err)); return false }
    finally { setBusy(false) }
  }
  const refresh = () => { void load().then(() => setError('')).catch(err => setError(getErrorMessage(err))) }
  const tickets = snapshot?.accounts.flatMap(account => account.tickets ?? []) ?? []
  const ready = tickets.filter(ticket => ticket.ready).length
  const standby = tickets.filter(ticket => ticket.standby_ready).length
  const runtime = snapshot?.controls.runtime
  return <div className="space-y-5">
    <PageHeader title={t('harvest.title')} description={t('harvest.description')} onRefresh={refresh} actions={<Button disabled={busy || !snapshot} onClick={() => void onAction(() => api.kickHarvest())}><Play />{t('harvest.kick')}</Button>} />
    {error && <div role="alert" className="rounded-lg border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive">{error}</div>}
    {!snapshot || !mihomo ? <div role="status" className="flex items-center gap-2 py-10 text-sm text-muted-foreground"><Loader2 className="size-4 animate-spin motion-reduce:animate-none" />{t('common.loading')}</div> : <>
      <div className="grid overflow-hidden rounded-xl border border-border bg-card sm:grid-cols-2 xl:grid-cols-4">
        {[
          { name: 'proxyStage', content: t('harvest.eligibleNodes', { count: mihomo.eligible_nodes }) },
          { name: 'harvestStage', content: `${t(runtime?.running ? 'harvest.running' : 'harvest.idle')} · ${runtime?.requests_used ?? 0}/${runtime?.request_budget ?? 0}` },
          { name: 'ticketStage', content: t('harvest.ticketCounts', { ready, standby }) },
          { name: 'businessStage', content: 'Cookie + Session + Proxy' },
        ].map((stage, index) => <div key={stage.name} className="flex items-center justify-between gap-3 border-b border-r border-border/60 p-4 last:border-0"><div><p className="text-xs text-muted-foreground">{t(`harvest.${stage.name}`)}</p><p className="mt-2 text-sm font-semibold tabular-nums">{stage.content}</p></div>{index < 3 ? <ArrowRight className="size-4 shrink-0 text-muted-foreground/50" /> : <Ticket className="size-4 text-primary" />}</div>)}
      </div>
      <div className="flex flex-wrap gap-x-6 gap-y-2 text-xs text-muted-foreground"><span>{t('harvest.nextRound')}: {harvestTime(runtime?.next_round_at)}</span>{runtime?.current_node && <span>{t('harvest.currentNode')}: {runtime.current_node}</span>}</div>
      <nav aria-label={t('harvest.title')} className="flex gap-1 overflow-x-auto border-b border-border">{(['overview', 'mihomo', 'manual', 'records'] as const).map(value => <button key={value} type="button" aria-current={tab === value ? 'page' : undefined} onClick={() => setTab(value)} className={cn('whitespace-nowrap border-b-2 px-4 py-3 text-sm font-medium outline-none focus-visible:ring-2 focus-visible:ring-ring', tab === value ? 'border-primary text-primary' : 'border-transparent text-muted-foreground hover:text-foreground')}>{t(`harvest.${value}`)}</button>)}</nav>
      {tab === 'overview' && <>
        <p className="text-sm leading-relaxed text-muted-foreground">{t('harvest.cookiePolicy')}</p>
        <HarvestControlsPanel snapshot={snapshot.controls} scope={snapshot.scope} busy={busy} onAction={onAction} />
        <HarvestSection title={t('harvest.configuration')}><CodexTicketSettings key={configRevision} /></HarvestSection>
        <HarvestSection title={t('harvest.accounts')}>
          <p className="mb-4 text-xs leading-relaxed text-muted-foreground">{t('harvest.binding')}</p>
          {snapshot.accounts.length === 0 ? <p className="py-6 text-sm text-muted-foreground">{t('harvest.noAccounts')}</p> : <div className="overflow-x-auto"><table className={harvestTableClass}><thead><tr>{['account', 'model', 'tickets', 'cookies', 'standby', 'skip'].map(key => <th key={key}>{t(`harvest.${key}`)}</th>)}</tr></thead><tbody>{snapshot.accounts.map(account => <tr key={account.id}><td className="min-w-40 max-w-64 break-words"><span>{account.email || 'Codex'}</span><span className="mt-1 block text-xs text-muted-foreground">#{account.id} {account.busy ? `· ${t('harvest.busyAccount')}` : ''}</span></td>
            <td className="whitespace-nowrap font-mono text-xs">{(account.tickets ?? []).map(ticket => <div className="py-2" key={ticket.model}>{ticket.model}</div>)}</td>
            <td>{(account.tickets ?? []).map(ticket => <div className="flex items-center gap-2 py-1.5" key={ticket.model}><HarvestPill good={ticket.ready}>{t(ticket.ready ? 'harvest.ready' : 'harvest.empty')}</HarvestPill><span className="whitespace-nowrap font-mono text-xs">{ticket.ready ? `${ticket.remaining_seconds}s` : '—'}</span></div>)}</td>
            <td>{(account.tickets ?? []).map(ticket => <div className="whitespace-nowrap py-2 font-mono text-xs" key={ticket.model}>{ticket.cookie_count || 0} · {ticket.cookie_expired ? t('harvest.expired') : `${ticket.cookie_remaining_seconds || 0}s`}</div>)}</td>
            <td>{(account.tickets ?? []).map(ticket => <div className="py-1.5" key={ticket.model}><HarvestPill good={ticket.standby_ready}>{t(ticket.standby_ready ? 'harvest.ready' : 'harvest.empty')}</HarvestPill></div>)}</td>
            <td><Switch aria-label={`${t('harvest.skip')} #${account.id}`} disabled={busy} checked={account.skipped} onCheckedChange={value => void onAction(() => api.updateHarvestScope({ ...snapshot.scope, skipped_account_ids: value ? [...snapshot.scope.skipped_account_ids, account.id] : snapshot.scope.skipped_account_ids.filter(id => id !== account.id) }))} /></td>
          </tr>)}</tbody></table></div>}
        </HarvestSection>
      </>}
      {tab === 'mihomo' && <MihomoPanel status={mihomo} busy={busy} onAction={async action => { const success = await onAction(action); if (success) setConfigRevision(value => value + 1); return success }} />}
      {tab === 'manual' && <HarvestManualPanel snapshot={snapshot} busy={busy} onAction={onAction} />}
      {tab === 'records' && <HarvestRecordsPanel accounts={snapshot.accounts} busy={busy} onAction={onAction} />}
    </>}
  </div>
}
