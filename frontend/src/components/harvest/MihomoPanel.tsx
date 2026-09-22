import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Download, Play, Search } from 'lucide-react'
import { api } from '@/api'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import { Select } from '@/components/ui/select'
import { splitHarvestList, type MihomoStatus, type MihomoCountryFilter } from '@/lib/codexHarvest'
import { HarvestPill, HarvestSection, harvestTableClass, type HarvestAction } from './shared'

export default function MihomoPanel({ status, busy, onAction }: { status: MihomoStatus; busy: boolean; onAction: HarvestAction }) {
  const { t } = useTranslation()
  const [urls, setURLs] = useState('')
  const [append, setAppend] = useState(true)
  const [filter, setFilter] = useState<MihomoCountryFilter>(status.country_filter)
  const [codes, setCodes] = useState(status.country_filter.codes.join(', '))
  const [search, setSearch] = useState('')
  const disabled = busy || status.busy || !status.supported
  const nodes = useMemo(() => (status.node_states ?? []).filter(node => `${node.display_name} ${node.name} ${node.country_code}`.toLowerCase().includes(search.toLowerCase())), [status.node_states, search])
  const action = (name: string) => void onAction(() => api.updateMihomo({ action: name }))
  return <div className="space-y-5">
    <HarvestSection title={t('harvest.kernel')} actions={<div className="flex gap-2"><HarvestPill good={status.installed}>{t(status.installed ? 'harvest.installed' : 'harvest.notInstalled')}</HarvestPill><HarvestPill good={status.running}>{t(status.running ? 'harvest.running' : 'harvest.stopped')}</HarvestPill></div>}>
      {!status.supported && <p className="mb-4 text-sm text-amber-600">{t('harvest.supported')}</p>}
      {status.error && <p role="alert" className="mb-4 break-words text-sm text-destructive">{status.error}</p>}
      <div className="flex flex-wrap items-center gap-3">
        <Button disabled={disabled} onClick={() => action('install')}><Download />{t('harvest.install')}</Button>
        <Button variant="outline" disabled={disabled || !status.installed} onClick={() => action('start')}><Play />{t('harvest.restart')}</Button>
        <Button variant="outline" disabled={busy || !status.running} onClick={() => void onAction(() => api.updateCodexTicketSettings({ harvest_proxy_url: status.endpoint }))}>{t('harvest.useProxy')}</Button>
        <span className="text-xs text-muted-foreground">{t('harvest.endpoint')}: <code>{status.endpoint}</code></span>
        {status.busy && <span role="status" className="text-sm text-primary">{status.phase}</span>}
      </div>
    </HarvestSection>
    <div className="grid items-start gap-5 xl:grid-cols-2">
      <HarvestSection title={t('harvest.subscriptions')} actions={<span className="text-xs text-muted-foreground">{t('harvest.subscriptionCount', { count: status.subscriptions })}</span>}>
        <label className="sr-only" htmlFor="mihomo-subscriptions">{t('harvest.subscriptions')}</label>
        <textarea id="mihomo-subscriptions" className="min-h-32 w-full rounded-lg border border-input bg-background p-3 font-mono text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring" placeholder="https://example.com/subscription" autoComplete="off" spellCheck={false} value={urls} onChange={event => setURLs(event.target.value)} disabled={disabled} />
        <p className="my-3 text-xs leading-relaxed text-muted-foreground">{t('harvest.subscriptionHint')}</p>
        <div className="flex flex-wrap items-center justify-between gap-3"><label className="flex items-center gap-2 text-sm"><Switch checked={append} disabled={disabled} onCheckedChange={setAppend} />{t('harvest.append')}</label><Button disabled={disabled || !status.installed || !urls.trim()} onClick={async () => { if (await onAction(() => api.updateMihomo({ action: 'apply', subscriptions: splitHarvestList(urls), append }))) setURLs('') }}>{t('harvest.apply')}</Button></div>
      </HarvestSection>
      <HarvestSection title={t('harvest.country')}>
        <div className="space-y-3"><Select aria-label={t('harvest.country')} value={filter.mode} disabled={disabled} onValueChange={mode => setFilter({ ...filter, mode: mode as MihomoCountryFilter['mode'] })} options={['off', 'exclude', 'include'].map(value => ({ value, label: t(`harvest.${value}`) }))} />
          <label className="block space-y-2 text-sm"><span>{t('harvest.countryCodes')}</span><Input value={codes} disabled={disabled || filter.mode === 'off'} onChange={event => setCodes(event.target.value.toUpperCase())} /></label>
          <label className="flex items-center gap-2 text-sm"><Switch checked={filter.allow_unknown} disabled={disabled} onCheckedChange={allow_unknown => setFilter({ ...filter, allow_unknown })} />{t('harvest.unknown')}</label>
          <div className="flex flex-wrap gap-2"><Button disabled={disabled || !status.installed} onClick={() => void onAction(() => api.updateMihomo({ action: 'country_filter', country_filter: { ...filter, codes: splitHarvestList(codes) } }))}>{t('harvest.save')}</Button><Button variant="outline" disabled={disabled || !status.running} onClick={() => action('country_scan')}>{t('harvest.scan')}</Button></div>
        </div>
      </HarvestSection>
    </div>
    <HarvestSection title={t('harvest.useOnce')} actions={<Switch aria-label={t('harvest.useOnce')} checked={status.use_once} disabled={disabled || !status.installed} onCheckedChange={value => action(value ? 'once_on' : 'once_off')} />}><p className="text-sm text-muted-foreground">{t('harvest.useOnceHint')}</p></HarvestSection>
    <HarvestSection title={t('harvest.node')} actions={<HarvestPill good={status.eligible_nodes > 0}>{t('harvest.eligibleNodes', { count: status.eligible_nodes })}</HarvestPill>}>
      <div className="relative mb-3 max-w-md"><Search className="absolute left-3 top-2.5 size-4 text-muted-foreground" /><Input className="pl-9" aria-label={t('harvest.filterNodes')} placeholder={t('harvest.filterNodes')} value={search} onChange={event => setSearch(event.target.value)} /></div>
      {nodes.length === 0 ? <p className="py-8 text-center text-sm text-muted-foreground">{t('harvest.noNodes')}</p> : <div className="max-h-[600px] overflow-auto"><table className={harvestTableClass}><thead><tr>{['node', 'countryCode', 'state', 'actions'].map(key => <th key={key}>{t(`harvest.${key}`)}</th>)}</tr></thead><tbody>{nodes.map(node => <tr key={node.name}>
        <td className="min-w-40 max-w-80 break-words">{node.display_name || node.name}</td><td title={node.country_error}>{node.country_code || '—'}</td><td><HarvestPill good={node.state === 'enabled'}>{t(`harvest.nodeState.${node.state}`, { defaultValue: node.state })}</HarvestPill></td>
        <td><div className="flex flex-wrap gap-1.5"><Button size="xs" variant="outline" disabled={disabled} onClick={() => action(`${node.state === 'enabled' ? 'disable' : 'recover'}/${node.name}`)}>{t(node.state === 'enabled' ? 'harvest.disable' : 'harvest.recover')}</Button><Button size="xs" variant="ghost" disabled={disabled} onClick={() => action(`probe/${node.name}`)}>{t('harvest.probe')}</Button><Button size="xs" variant="ghost" disabled={disabled} onClick={() => action(`country_probe/${node.name}`)}>{t('harvest.countryProbe')}</Button></div></td>
      </tr>)}</tbody></table></div>}
    </HarvestSection>
  </div>
}
