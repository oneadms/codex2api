import { useCallback, useEffect, useId, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Loader2, Plus, RotateCcw, Save, Trash2 } from 'lucide-react'
import { api } from '../api'
import { useToast } from '../hooks/useToast'
import { getErrorMessage } from '../utils/error'
import { serializeModelMappingEntries, type ModelMappingEntry } from '../lib/modelMapping'
import { Button } from './ui/button'
import { Input } from './ui/input'
import { Select } from './ui/select'
import type { TraeCNSettingsResponse } from '../types'

const entriesFrom = (settings: TraeCNSettingsResponse): ModelMappingEntry[] =>
  Object.entries(settings.model_mapping).map(([from, to]) => ({ from, to }))

// 使用独立接口和保存状态，避免通用设置的保存操作覆盖 TRAECN 的编辑内容。
export default function TraeCNModelMapping() {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const sourceListId = useId()
  const [settings, setSettings] = useState<TraeCNSettingsResponse | null>(null)
  const [entries, setEntries] = useState<ModelMappingEntry[]>([])
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')

  const load = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const response = await api.getTraeCNSettings()
      setSettings(response)
      setEntries(entriesFrom(response))
    } catch (err) {
      setError(getErrorMessage(err))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { void load() }, [load])

  const serialized = serializeModelMappingEntries(entries)
  const saved = settings ? serializeModelMappingEntries(entriesFrom(settings)) : null
  const dirty = !serialized.ok || !saved?.ok || serialized.value !== saved.value
  const invalid = !serialized.ok || entries.some(({ from, to }) => !from.trim() || !to.trim())
  const change = (index: number, field: keyof ModelMappingEntry, value: string) => {
    setEntries((current) => current.map((entry, i) => i === index ? { ...entry, [field]: value } : entry))
  }

  const save = async () => {
    if (!serialized.ok || invalid) return
    setSaving(true)
    setError('')
    try {
      const response = await api.updateTraeCNSettings(JSON.parse(serialized.value || '{}'))
      setSettings(response)
      setEntries(entriesFrom(response))
      showToast(t('settings.traecnMapping.saved'), 'success')
    } catch (err) {
      setError(getErrorMessage(err))
    } finally {
      setSaving(false)
    }
  }

  if (loading) return <div role="status" className="flex items-center gap-2 text-sm text-muted-foreground"><Loader2 className="size-4 animate-spin" />{t('common.loading')}</div>
  if (!settings) return <div className="space-y-3"><p role="alert" className="text-sm text-destructive">{error}</p><Button variant="outline" onClick={() => void load()}>{t('common.retry')}</Button></div>

  const options = settings.models.map((model) => ({ value: model, label: model }))
  return (
    <div className="space-y-4">
      <p className="text-sm leading-relaxed text-muted-foreground">{t('settings.traecnMapping.hint')}</p>
      {entries.length === 0 && <div className="rounded-xl border border-dashed px-4 py-7 text-center text-sm text-muted-foreground">{t('settings.traecnMapping.empty')}</div>}
      <div className="space-y-3">
        {entries.map((entry, index) => (
          <div key={index} className="grid grid-cols-1 items-end gap-3 rounded-xl border border-border p-3 sm:grid-cols-[minmax(0,1fr)_minmax(0,1fr)_auto]">
            <label className="space-y-1.5 text-xs font-medium">
              <span>{t('settings.traecnMapping.source')}</span>
              <Input aria-label={`${t('settings.traecnMapping.source')} ${index + 1}`} value={entry.from} list={sourceListId} placeholder="my-code-model" disabled={saving} onChange={(event) => change(index, 'from', event.target.value)} className="font-mono text-sm" />
            </label>
            <div className="space-y-1.5 text-xs font-medium">
              <span>{t('settings.traecnMapping.target')}</span>
              <Select value={entry.to} placeholder={t('settings.traecnMapping.selectTarget')} options={entry.to && !settings.models.includes(entry.to) ? [...options, { value: entry.to, label: entry.to }] : options} disabled={saving} onValueChange={(value) => change(index, 'to', value)} />
            </div>
            <Button variant="ghost" size="icon" disabled={saving} aria-label={`${t('common.delete')} ${index + 1}`} onClick={() => setEntries((current) => current.filter((_, i) => i !== index))}><Trash2 className="size-4" /></Button>
          </div>
        ))}
      </div>
      <datalist id={sourceListId}>{settings.models.map((model) => <option key={model} value={model} />)}</datalist>
      {invalid && <p role="alert" className="text-sm text-destructive">{t('settings.traecnMapping.invalid')}</p>}
      {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
      <div className="flex flex-wrap items-center justify-between gap-3">
        <Button variant="outline" disabled={saving || entries.length >= 200} onClick={() => setEntries((current) => [...current, { from: '', to: '' }])}><Plus className="size-4" />{t('settings.traecnMapping.add')}</Button>
        <div className="flex gap-2">
          <Button variant="ghost" disabled={saving || !dirty} onClick={() => { setEntries(entriesFrom(settings)); setError('') }}><RotateCcw className="size-4" />{t('settings.traecnMapping.reset')}</Button>
          <Button disabled={saving || !dirty || invalid} onClick={() => void save()}>{saving ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}{t('settings.traecnMapping.save')}</Button>
        </div>
      </div>
    </div>
  )
}
