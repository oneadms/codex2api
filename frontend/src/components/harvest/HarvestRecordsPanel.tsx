import { useCallback, useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api } from '@/api'
import { Button } from '@/components/ui/button'
import { Select } from '@/components/ui/select'
import { getErrorMessage } from '@/utils/error'
import type { HarvestAccount, HarvestPage, HarvestEvent, HarvestNodeRecord } from '@/lib/codexHarvest'
import { HarvestPager, HarvestPill, HarvestSection, harvestTableClass, harvestTime, type HarvestAction } from './shared'

export default function HarvestRecordsPanel({ accounts, busy, onAction }: { accounts: HarvestAccount[]; busy: boolean; onAction: HarvestAction }) {
  const { t } = useTranslation()
  const [nodes, setNodes] = useState<HarvestPage<HarvestNodeRecord>>({ items: [], total: 0 })
  const [events, setEvents] = useState<HarvestPage<HarvestEvent>>({ items: [], total: 0 })
  const [nodeOffset, setNodeOffset] = useState(0)
  const [eventOffset, setEventOffset] = useState(0)
  const [accountID, setAccountID] = useState(0)
  const [error, setError] = useState('')
  const load = useCallback(async () => {
    const [nextNodes, nextEvents] = await Promise.all([api.getHarvestNodes(nodeOffset), api.getHarvestEvents(eventOffset, accountID)])
    setNodes(nextNodes); setEvents(nextEvents); setError('')
  }, [nodeOffset, eventOffset, accountID])
  useEffect(() => {
    let cancelled = false
    let timer: ReturnType<typeof setTimeout>
    const refresh = async () => { try { if (!cancelled) await load() } catch (err) { if (!cancelled) setError(getErrorMessage(err)) } finally { if (!cancelled) timer = setTimeout(() => void refresh(), 5000) } }
    void refresh()
    return () => { cancelled = true; clearTimeout(timer) }
  }, [load])
  const reset = async (id: number) => { if (await onAction(() => api.resetHarvestNodes(id))) { setNodeOffset(0); await load().catch(err => setError(getErrorMessage(err))) } }
  return <div className="space-y-5">
    {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
    <HarvestSection title={t('harvest.nodeRecords')} actions={<Button size="sm" variant="outline" disabled={busy || nodes.total === 0} onClick={() => void reset(0)}>{t('harvest.reset')}</Button>}>
      {nodes.items.length === 0 ? <p className="py-6 text-sm text-muted-foreground">{t('harvest.noRecords')}</p> : <div className="overflow-x-auto"><table className={harvestTableClass}><thead><tr>{['node', 'account', 'model', 'successes', 'misses', 'networkErrors', 'accountErrors', 'cooldown', 'actions'].map(key => <th key={key}>{t(`harvest.${key}`)}</th>)}</tr></thead><tbody>{nodes.items.map(node => <tr key={node.id}><td className="min-w-40 max-w-64 break-words">{node.node_name}<span className="block text-xs text-muted-foreground">{node.provider}</span></td><td>#{node.account_id}</td><td className="whitespace-nowrap font-mono text-xs">{node.model}</td><td>{node.successes}</td><td>{node.misses}</td><td>{node.network_errors}</td><td>{node.account_errors}</td><td className="whitespace-nowrap text-xs">{harvestTime(node.cooldown_until)}</td><td><Button size="xs" variant="ghost" disabled={busy} onClick={() => void reset(node.id)}>{t('harvest.reset')}</Button></td></tr>)}</tbody></table></div>}
      <HarvestPager offset={nodeOffset} total={nodes.total} onChange={setNodeOffset} />
    </HarvestSection>
    <HarvestSection title={t('harvest.events')} actions={<Select aria-label={t('harvest.account')} value={String(accountID)} onValueChange={value => { setAccountID(Number(value)); setEventOffset(0) }} options={[{ value: '0', label: t('harvest.allAccounts') }, ...accounts.map(account => ({ value: String(account.id), label: `${account.email || 'Codex'} #${account.id}` }))]} />}>
      {events.items.length === 0 ? <p className="py-6 text-sm text-muted-foreground">{t('harvest.noRecords')}</p> : <div className="overflow-x-auto"><table className={harvestTableClass}><thead><tr>{['time', 'account', 'model', 'source', 'node', 'result', 'length', 'cookies', 'latency'].map(key => <th key={key}>{t(`harvest.${key}`)}</th>)}</tr></thead><tbody>{events.items.map(event => <tr key={event.id}><td className="whitespace-nowrap text-xs">{harvestTime(event.created_at)}</td><td>#{event.account_id}</td><td className="whitespace-nowrap font-mono text-xs">{event.model}</td><td>{t(`harvest.${event.source}`, { defaultValue: event.source })}</td><td className="max-w-56 break-words">{event.node_name || '—'}</td><td><HarvestPill good={event.result === 'success'}>{t(`harvest.results.${event.result}`, { defaultValue: event.result })}</HarvestPill><span className="mt-1 block text-xs text-muted-foreground">HTTP {event.http_status || '—'}</span></td><td className="font-mono">{event.length}/{event.expected_length}</td><td>{event.cookie_count}</td><td className="whitespace-nowrap font-mono text-xs">{event.latency_ms} ms</td></tr>)}</tbody></table></div>}
      <HarvestPager offset={eventOffset} total={events.total} onChange={setEventOffset} />
    </HarvestSection>
  </div>
}
