import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api, type ProxyRow } from '@/api'
import { Input } from '@/components/ui/input'
import { Button } from '@/components/ui/button'

/** IDs only enter harvest settings; proxy credentials stay out of the form. */
export default function HarvestProxyPicker({ selected, disabled, onChange }: { selected: number[]; disabled: boolean; onChange: (ids: number[]) => void }) {
  const { t } = useTranslation()
  const [rows, setRows] = useState<ProxyRow[]>([])
  const [search, setSearch] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(true)
  useEffect(() => {
    let active = true
    api.listProxies().then(result => { if (active) setRows(result.proxies ?? []) }).catch(() => { if (active) setError(t('harvest.proxyLoadError')) }).finally(() => { if (active) setLoading(false) })
    return () => { active = false }
  }, [t])
  const filtered = rows.filter(row => `${row.id} ${row.label} ${row.test_location}`.toLowerCase().includes(search.toLowerCase()))
  const missing = selected.filter(id => !rows.some(row => row.id === id))
  return <fieldset className="space-y-3 rounded-lg border border-border p-3" disabled={disabled}>
    <legend className="px-1 text-sm font-medium">{t('harvest.proxyPool')}</legend>
    <p className="text-xs leading-relaxed text-muted-foreground">{t('harvest.proxyPoolHint')}</p>
    <Input aria-label={t('harvest.proxySearch')} placeholder={t('harvest.proxySearch')} value={search} onChange={event => setSearch(event.target.value)} />
    {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
    {loading ? <p role="status" className="text-xs text-muted-foreground">{t('common.loading')}</p> : <div className="max-h-48 space-y-1 overflow-y-auto">
      {filtered.length === 0 && <p className="py-2 text-xs text-muted-foreground">{t('harvest.noProxies')}</p>}
      {filtered.map(row => {
        const checked = selected.includes(row.id)
        const unavailable = !row.enabled || row.test_status === 'error'
        return <label key={row.id} className="flex items-start gap-2 rounded p-2 text-sm hover:bg-muted/50">
          <input type="checkbox" className="mt-0.5" checked={checked} disabled={disabled || (unavailable && !checked)} onChange={() => onChange(checked ? selected.filter(id => id !== row.id) : [...selected, row.id])} />
          <span className="min-w-0 break-words">{row.label || `#${row.id}`} <span className="text-xs text-muted-foreground">#{row.id}{row.test_location ? ` · ${row.test_location}` : ''}{unavailable ? ` · ${t('harvest.disabled')}` : ''}</span></span>
        </label>
      })}
      {missing.map(id => <label key={id} className="flex items-center gap-2 p-2 text-sm"><input type="checkbox" checked disabled={disabled} onChange={() => onChange(selected.filter(value => value !== id))} /><span>#{id} · {t('harvest.proxyMissing')}</span></label>)}
    </div>}
    {selected.length > 0 && <Button type="button" size="xs" variant="ghost" disabled={disabled} onClick={() => onChange([])}>{t('harvest.clearProxySelection', { count: selected.length })}</Button>}
  </fieldset>
}
