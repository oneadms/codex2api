import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Play, Square } from 'lucide-react'
import { api } from '@/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import { splitHarvestList, type HarvestSnapshot, type HarvestManualRequest } from '@/lib/codexHarvest'
import { HarvestPill, HarvestSection, harvestTime, type HarvestAction } from './shared'

/**
 * 手动自采。32 路上限不再由前端强制（Go 按部署校验），只做正整数 sanity check；
 * max_attempts 也不做前端硬限，提交后由后端返回的实际校验结果为准。
 */
export default function HarvestManualPanel({ snapshot, busy, onAction }: { snapshot: HarvestSnapshot; busy: boolean; onAction: HarvestAction }) {
  const { t } = useTranslation()
  const eligible = snapshot.accounts.filter(account => account.eligible)
  const [draft, setDraft] = useState<HarvestManualRequest>({ collect_lanes: 1, account_id: eligible[0]?.id ?? 0, models: [], probe_interval_seconds: 2, rate_limit_cooldown_seconds: 60, max_attempts: 20, node_switch_rule: 'every_request', stop_on_success: true })
  const [lanes, setLanes] = useState(10)
  const [models, setModels] = useState((snapshot.accounts[0]?.tickets ?? []).map(ticket => ticket.model).join(', '))
  const modelList = splitHarvestList(models)
  const lanesValid = Number.isInteger(lanes) && lanes >= 1
  const attemptsValid = Number.isInteger(draft.max_attempts) && draft.max_attempts >= 1
  return <div className="space-y-5">
    <HarvestSection title={t('harvest.manual')}>
      <form className="space-y-4" onSubmit={event => { event.preventDefault(); void onAction(() => api.startManualHarvest({ ...draft, models: modelList })) }}>
        <div className="grid gap-4 sm:grid-cols-2"><label className="space-y-2 text-sm"><span>{t('harvest.account')}</span><Select aria-label={t('harvest.account')} disabled={busy} value={String(draft.account_id)} onValueChange={value => setDraft({ ...draft, account_id: Number(value) })} options={eligible.map(account => ({ value: String(account.id), label: `${account.email || 'Codex'} #${account.id}` }))} /></label>
          <label className="space-y-2 text-sm"><span>{t('harvest.models')}</span><Input value={models} required disabled={busy} onChange={event => setModels(event.target.value)} /></label></div>
        <div className="grid gap-4 sm:grid-cols-3">{(['max_attempts', 'probe_interval_seconds', 'rate_limit_cooldown_seconds'] as const).map(field => <label className="space-y-2 text-sm" key={field}><span>{t(`harvest.${field === 'max_attempts' ? 'attempts' : field === 'rate_limit_cooldown_seconds' ? 'rateCooldown' : field}`)}</span><Input type="number" required disabled={busy} min={field === 'probe_interval_seconds' ? 0 : 1} value={draft[field]} onChange={event => setDraft({ ...draft, [field]: Number(event.target.value) })} /></label>)}</div>
        <label className="block max-w-lg space-y-2 text-sm"><span>{t('harvest.switchRule')}</span><Select aria-label={t('harvest.switchRule')} disabled={busy} value={draft.node_switch_rule} onValueChange={value => setDraft({ ...draft, node_switch_rule: value as HarvestManualRequest['node_switch_rule'] })} options={['every_request', '312_or_2fail', '312_only', 'never'].map(value => ({ value, label: t(`harvest.${value}`) }))} /></label>
        <p className="text-xs leading-relaxed text-muted-foreground">{t('harvest.switchHint')}</p>
        <div className="space-y-3 rounded-lg border border-border p-3">
          <label className="block max-w-48 space-y-2 text-sm"><span>{t('harvest.collectLanes')}</span><Input type="number" min={1} step={1} value={lanes} disabled={busy || !snapshot.parallel_available} onChange={event => setLanes(Number(event.target.value))} /></label>
          <p className="text-xs leading-relaxed text-muted-foreground">{t('harvest.parallelHint')}</p>
          {!lanesValid && <p role="alert" className="text-xs text-amber-600">{t('harvest.lanesInvalid')}</p>}
          {!snapshot.parallel_available && <p className="text-xs text-amber-600">{t('harvest.parallelUnavailable')}</p>}
        </div>
        <p className="text-xs leading-relaxed text-muted-foreground">{t('harvest.probePolicy')}</p>
        <div className="flex flex-wrap items-center justify-between gap-3"><label className="flex items-center gap-2 text-sm"><Switch checked={draft.stop_on_success} disabled={busy} onCheckedChange={stop_on_success => setDraft({ ...draft, stop_on_success })} />{t('harvest.stopOnSuccess')}</label><div className="flex flex-wrap gap-2"><Button type="submit" variant="outline" disabled={busy || !draft.account_id || !modelList.length || !attemptsValid}><Play />{t('harvest.start')}</Button><Button type="button" disabled={busy || !snapshot.parallel_available || !draft.account_id || !modelList.length || !attemptsValid || !lanesValid} onClick={() => void onAction(() => api.startManualHarvest({ ...draft, models: modelList, collect_lanes: lanes }))}><Play />{t('harvest.startParallel')}</Button></div></div>
      </form>
    </HarvestSection>
    <HarvestSection title={t('harvest.jobs')}>
      {snapshot.jobs.length === 0 ? <p className="py-6 text-center text-sm text-muted-foreground">{t('harvest.noJobs')}</p> : <div className="divide-y divide-border">{snapshot.jobs.map(job => <div key={job.id} className="space-y-3 py-4 first:pt-0 last:pb-0"><div className="flex flex-wrap items-center justify-between gap-3"><div className="flex flex-wrap items-center gap-3"><code className="text-sm">#{job.request.account_id} · {job.request.models.join(', ')}</code><HarvestPill good={job.running}>{t(job.running ? 'harvest.running' : job.cancelled ? 'harvest.cancelled' : 'harvest.completed')}</HarvestPill></div>{job.running && <Button size="sm" variant="outline" disabled={busy} onClick={() => void onAction(() => api.stopManualHarvest(job.id))}><Square />{t('harvest.stop')}</Button>}</div>
        <p className="text-sm tabular-nums">{t('harvest.jobProgress', { attempts: job.attempts, max: job.request.max_attempts, tickets: job.tickets_stored })} · {t('harvest.laneCount', { count: job.request.collect_lanes || 1 })}</p>
        <div className="h-1.5 overflow-hidden rounded bg-muted" role="progressbar" aria-label={t('harvest.manual')} aria-valuenow={job.attempts} aria-valuemin={0} aria-valuemax={job.request.max_attempts}><div className="h-full bg-primary" style={{ width: `${Math.min(100, job.attempts / Math.max(1, job.request.max_attempts) * 100)}%` }} /></div>
        {job.last_event && <p className="break-words font-mono text-xs text-muted-foreground">{job.last_event.node_name || 'proxy'} · HTTP {job.last_event.http_status || '—'} · {job.last_event.length}/{job.last_event.expected_length} · {t(`harvest.results.${job.last_event.result}`, { defaultValue: job.last_event.result })}</p>}
        <p className="text-xs text-muted-foreground">{harvestTime(job.started_at)}</p>
      </div>)}</div>}
    </HarvestSection>
  </div>
}
