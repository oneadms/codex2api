import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api } from '@/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import { harvestSpeedFields, splitHarvestList, type HarvestControlSnapshot, type HarvestScope } from '@/lib/codexHarvest'
import { HarvestSection, type HarvestAction } from './shared'

export default function HarvestControlsPanel({ snapshot, scope, busy, onAction }: { snapshot: HarvestControlSnapshot; scope: HarvestScope; busy: boolean; onAction: HarvestAction }) {
  const { t } = useTranslation()
  const [draft, setDraft] = useState(snapshot.settings)
  const [scopeDraft, setScopeDraft] = useState(scope)
  const [groups, setGroups] = useState(scope.group_ids.join(', '))
  const saveScope = () => onAction(() => api.updateHarvestScope({ ...scopeDraft, skipped_account_ids: scope.skipped_account_ids, group_ids: splitHarvestList(groups).map(Number) }))
  return <div className="space-y-5">
    <HarvestSection title={t('harvest.controls')} actions={<Button size="sm" variant="ghost" onClick={() => setDraft(snapshot.defaults)} disabled={busy}>{t('harvest.defaults')}</Button>}>
      <div className="mb-5 flex flex-wrap gap-2">{Object.entries(snapshot.presets).map(([name, speed]) => <Button key={name} size="sm" variant={JSON.stringify(draft.speed) === JSON.stringify(speed) ? 'default' : 'outline'} disabled={busy} onClick={() => setDraft({ ...draft, speed })}>{t(`harvest.${name}`)}</Button>)}</div>
      <form onSubmit={event => { event.preventDefault(); void onAction(() => api.updateHarvestControls(draft)) }}>
        <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">{harvestSpeedFields.map(field => <label key={field} className="space-y-2 text-sm"><span>{t(`harvest.${field}`)}</span><Input type="number" required min={snapshot.bounds[field].min} max={snapshot.bounds[field].max} disabled={busy} value={draft.speed[field]} onChange={event => setDraft({ ...draft, speed: { ...draft.speed, [field]: Number(event.target.value) } })} /></label>)}</div>
        <p className="mt-3 text-xs text-muted-foreground">{t('harvest.refreshHint')}</p>
        <div className="mt-5 flex items-start justify-between gap-4 border-t pt-4"><div><label className="flex items-center gap-2 text-sm font-medium"><Switch checked={draft.node_memory_enabled} disabled={busy} onCheckedChange={node_memory_enabled => setDraft({ ...draft, node_memory_enabled })} />{t('harvest.learning')}</label><p className="mt-2 text-xs text-muted-foreground">{t('harvest.learningHint')}</p></div><Button type="submit" disabled={busy}>{t('harvest.save')}</Button></div>
      </form>
      {!snapshot.available && <p className="mt-3 text-xs leading-relaxed text-amber-600">{t('harvest.unavailable')}</p>}
      {snapshot.settings_error && <p role="alert" className="mt-3 text-sm text-destructive">{snapshot.settings_error}</p>}
    </HarvestSection>
    <HarvestSection title={t('harvest.scope')}>
      <div className="grid gap-4 sm:grid-cols-2"><Select aria-label={t('harvest.scope')} value={scopeDraft.mode} disabled={busy} onValueChange={mode => setScopeDraft({ ...scopeDraft, mode: mode as HarvestScope['mode'] })} options={['all', 'selected'].map(value => ({ value, label: t(`harvest.${value}`) }))} />
        <Select aria-label={t('harvest.schedulable')} value={scopeDraft.account_policy} disabled={busy} onValueChange={account_policy => setScopeDraft({ ...scopeDraft, account_policy: account_policy as HarvestScope['account_policy'] })} options={[{ value: 'schedulable_only', label: t('harvest.schedulable') }, { value: 'prioritize_schedulable', label: t('harvest.prioritize') }]} /></div>
      {scopeDraft.mode === 'selected' && <label className="mt-4 block space-y-2 text-sm"><span>{t('harvest.groupIDs')}</span><Input value={groups} disabled={busy} onChange={event => setGroups(event.target.value)} /></label>}
      <div className="mt-4 flex justify-end"><Button disabled={busy} onClick={() => void saveScope()}>{t('harvest.save')}</Button></div>
    </HarvestSection>
  </div>
}
