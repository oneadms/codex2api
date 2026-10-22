import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api } from '@/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import { harvestSpeedFields, splitHarvestList, type HarvestControlSnapshot, type HarvestControls, type HarvestScope, type HarvestAccount } from '@/lib/codexHarvest'
import HarvestProxyPicker from './HarvestProxyPicker'
import { HarvestPill, HarvestSection, type HarvestAction } from './shared'

/**
 * 采票控制。草稿独立保存：轮询刷新运行时但不覆盖未保存的 speed/transport 修改，
 * 只有点击刷新或放弃修改才回读服务端值。strategy/proxy_ids 与 Go controls.go 对齐。
 */
export default function HarvestControlsPanel({ snapshot, scope, accounts, groups, busy, onAction }: { snapshot: HarvestControlSnapshot; scope: HarvestScope; accounts: HarvestAccount[]; groups: { id: number; name: string }[]; busy: boolean; onAction: HarvestAction }) {
  const { t } = useTranslation()
  const [draft, setDraft] = useState<HarvestControls>(snapshot.settings)
  const [scopeDraft, setScopeDraft] = useState<HarvestScope>(scope)
  const [groupsText, setGroupsText] = useState(scope.group_ids.join(', '))
  const touched = useRef(false)
  const savedKey = JSON.stringify(snapshot.settings)
  useEffect(() => {
    // 服务端保存成功后（settings 与草稿一致或用户未改动）同步草稿。
    if (!touched.current) { setDraft(snapshot.settings) }
  }, [savedKey, snapshot.settings])
  const markTouched = () => { touched.current = true }
  const controlsDirty = JSON.stringify(draft) !== JSON.stringify(snapshot.settings)
  const resetDraft = () => { touched.current = false; setDraft(snapshot.settings) }
  const saveScope = () => onAction(() => api.updateHarvestScope({ ...scopeDraft, skipped_account_ids: scope.skipped_account_ids, group_ids: scopeDraft.mode === 'selected' ? splitHarvestList(groupsText).map(Number).filter(id => Number.isInteger(id) && id > 0) : [] }))
  return <div className="space-y-5">
    <HarvestSection title={t('harvest.controls')} actions={<div className="flex flex-wrap items-center gap-2">
      {controlsDirty && <HarvestPill>{t('harvest.unsaved')}</HarvestPill>}
      <Button size="sm" variant="ghost" onClick={() => { resetDraft(); touched.current = false }} disabled={busy}>{t('harvest.defaults')}</Button>
      {controlsDirty && <Button size="sm" variant="outline" disabled={busy} onClick={resetDraft}>{t('harvest.discard')}</Button>}
    </div>}>
      <form onSubmit={event => { event.preventDefault(); void onAction(async () => { await api.updateHarvestControls(draft); touched.current = false }) }}>
        <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
          <label className="space-y-2 text-sm"><span>{t('harvest.transport')}</span><Select aria-label={t('harvest.transport')} disabled={busy} value={draft.transport} onValueChange={transport => { markTouched(); setDraft({ ...draft, transport: transport as typeof draft.transport }) }} options={[{ value: 'sse', label: 'SSE / HTTP' }, { value: 'websocket', label: 'WebSocket' }]} /></label>
          <label className="space-y-2 text-sm"><span>{t('harvest.strategy')}</span><Select aria-label={t('harvest.strategy')} disabled={busy} value={draft.strategy} onValueChange={strategy => { markTouched(); setDraft({ ...draft, strategy: strategy as HarvestControls['strategy'] }) }} options={[{ value: 'refresh', label: t('harvest.strategyRefresh') }, { value: 'fixed', label: t('harvest.strategyFixed') }]} /><span className="block text-xs leading-relaxed text-muted-foreground">{t('harvest.strategyHint')}</span></label>
          <HarvestProxyPicker selected={draft.proxy_ids ?? []} disabled={busy} onChange={proxy_ids => { markTouched(); setDraft(current => ({ ...current, proxy_ids })) }} />
        </div>
        <p className="my-4 text-xs leading-relaxed text-muted-foreground">{t('harvest.transportHint')}</p>
        <label className="mb-4 block max-w-lg space-y-2 text-sm"><span>{t('harvest.edgeIP')}</span><Input value={draft.edge_ip} disabled={busy} onChange={event => { markTouched(); setDraft({ ...draft, edge_ip: event.target.value.trim() }) }} placeholder="104.18.0.1" /><span className="block text-xs leading-relaxed text-muted-foreground">{t('harvest.edgeHint')}</span></label>
        <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">{harvestSpeedFields.map(field => <label key={field} className="space-y-2 text-sm"><span>{t(`harvest.${field}`)}</span><Input type="number" required min={snapshot.bounds[field].min} max={snapshot.bounds[field].max} disabled={busy} value={draft.speed[field]} onChange={event => { markTouched(); setDraft({ ...draft, speed: { ...draft.speed, [field]: Number(event.target.value) } }) }} /></label>)}</div>
        <p className="mt-3 text-xs text-muted-foreground">{t('harvest.refreshHint')}</p>
        <div className="mt-5 flex flex-wrap items-start justify-between gap-4 border-t pt-4"><div><label className="flex items-center gap-2 text-sm font-medium"><Switch checked={draft.node_memory_enabled} disabled={busy} onCheckedChange={node_memory_enabled => { markTouched(); setDraft({ ...draft, node_memory_enabled }) }} />{t('harvest.learning')}</label><p className="mt-2 text-xs text-muted-foreground">{t('harvest.learningHint')}</p></div><Button type="submit" disabled={busy || !controlsDirty}>{t('harvest.save')}</Button></div>
      </form>
      {!snapshot.available && <p className="mt-3 text-xs leading-relaxed text-amber-600">{t('harvest.unavailable')}</p>}
      {snapshot.settings_error && <p role="alert" className="mt-3 text-sm text-destructive">{snapshot.settings_error}</p>}
    </HarvestSection>
    <HarvestSection title={t('harvest.scope')}>
      <div className="grid gap-4 sm:grid-cols-2">
        <label className="space-y-2 text-sm"><span>{t('harvest.scopeMode')}</span><Select aria-label={t('harvest.scope')} value={scopeDraft.mode} disabled={busy} onValueChange={mode => setScopeDraft({ ...scopeDraft, mode: mode as HarvestScope['mode'] })} options={[{ value: 'all', label: t('harvest.scopeAll') }, { value: 'selected', label: t('harvest.scopeSelected') }]} /></label>
        <label className="space-y-2 text-sm"><span>{t('harvest.scopePolicy')}</span><Select aria-label={t('harvest.schedulable')} value={scopeDraft.account_policy} disabled={busy} onValueChange={account_policy => setScopeDraft({ ...scopeDraft, account_policy: account_policy as HarvestScope['account_policy'] })} options={[{ value: 'schedulable_only', label: t('harvest.schedulable') }, { value: 'prioritize_schedulable', label: t('harvest.prioritize') }]} /></label>
      </div>
      {scopeDraft.mode === 'selected' && <div className="mt-4 grid gap-4 sm:grid-cols-2">
        <label className="space-y-2 text-sm"><span>{t('harvest.groupIDs')}</span><Input value={groupsText} disabled={busy} onChange={event => setGroupsText(event.target.value)} placeholder="1, 2, 3" /></label>
        <label className="space-y-2 text-sm"><span>{t('harvest.knownGroups')}</span><Select aria-label={t('harvest.knownGroups')} value="" disabled={busy} onValueChange={value => { const id = Number(value); if (id > 0 && !splitHarvestList(groupsText).map(Number).includes(id)) setGroupsText(prev => (prev.trim() ? `${prev}, ${id}` : String(id))) }} options={[{ value: '', label: t('harvest.addGroup') }, ...groups.map(group => ({ value: String(group.id), label: `${group.name} (#${group.id})` }))]} /></label>
      </div>}
      <div className="mt-4 flex justify-end"><Button disabled={busy} onClick={() => void saveScope()}>{t('harvest.save')}</Button></div>
    </HarvestSection>
  </div>
}
