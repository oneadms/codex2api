import { useCallback, useEffect, useId, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Loader2, RotateCcw, Save } from 'lucide-react'
import { api } from '../api'
import type { TraeCNModelSyncSettings as ModelSyncSettings } from '../types'
import { useToast } from '../hooks/useToast'
import { getErrorMessage } from '../utils/error'
import { displayTimezoneOptions, isValidTimezone } from '../lib/displayTimezone'
import {
  buildTraeCNModelSyncPatch,
  DEFAULT_TRAECN_MODEL_SYNC_SETTINGS,
  isValidModelSyncTime,
  normalizeTraeCNModelSyncSettings,
} from '../lib/traecnModelSync'
import { Button } from './ui/button'
import { Input } from './ui/input'
import { Select } from './ui/select'
import { Switch } from './ui/switch'

// The daily schedule is saved independently; model mappings and passthrough stay untouched.
export default function TraeCNModelSyncSettings() {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const fieldId = useId()
  const [saved, setSaved] = useState<ModelSyncSettings | null>(null)
  const [draft, setDraft] = useState(DEFAULT_TRAECN_MODEL_SYNC_SETTINGS)
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')

  const load = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const settings = normalizeTraeCNModelSyncSettings(await api.getTraeCNSettings())
      setSaved(settings)
      setDraft(settings)
    } catch (err) {
      setError(getErrorMessage(err))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { void load() }, [load])

  const timezoneOptions = useMemo(() => displayTimezoneOptions(draft.model_sync_timezone), [draft.model_sync_timezone])
  const patch = saved ? buildTraeCNModelSyncPatch(draft, saved) : {}
  const dirty = Object.keys(patch).length > 0
  const validTime = isValidModelSyncTime(draft.model_sync_time)
  const validTimezone = isValidTimezone(draft.model_sync_timezone)

  const save = async () => {
    if (!saved || saving || !dirty || !validTime || !validTimezone) return
    setSaving(true)
    setError('')
    try {
      const settings = normalizeTraeCNModelSyncSettings(await api.updateTraeCNSettings(patch))
      setSaved(settings)
      setDraft(settings)
      showToast(t('settings.traecnModelSync.saved'), 'success')
    } catch (err) {
      setError(getErrorMessage(err))
    } finally {
      setSaving(false)
    }
  }

  if (loading) return <div role="status" className="flex items-center gap-2 text-sm text-muted-foreground"><Loader2 className="size-4 animate-spin" />{t('common.loading')}</div>
  if (!saved) return (
    <div className="space-y-3">
      <p role="alert" className="text-sm text-destructive">{error}</p>
      <Button variant="outline" onClick={() => void load()}><RotateCcw className="size-4" />{t('common.retry')}</Button>
    </div>
  )

  return (
    <div className="space-y-4">
      <div className="flex items-start justify-between gap-4 rounded-xl border border-border p-4">
        <div className="space-y-1.5">
          <label htmlFor={`${fieldId}-enabled`} className="text-sm font-medium">{t('settings.traecnModelSync.enabled')}</label>
          <p className="text-sm leading-relaxed text-muted-foreground">{t('settings.traecnModelSync.scope')}</p>
        </div>
        <Switch
          id={`${fieldId}-enabled`}
          aria-label={t('settings.traecnModelSync.enabled')}
          checked={draft.model_sync_enabled}
          disabled={saving}
          onCheckedChange={(checked) => setDraft((current) => ({ ...current, model_sync_enabled: checked }))}
        />
      </div>
      <div className="grid gap-4 sm:grid-cols-2">
        <div className="space-y-1.5">
          <label htmlFor={`${fieldId}-time`} className="text-xs font-medium">{t('settings.traecnModelSync.time')}</label>
          <Input
            id={`${fieldId}-time`}
            type="time"
            step={60}
            required
            value={draft.model_sync_time}
            disabled={saving}
            aria-invalid={!validTime}
            aria-describedby={`${fieldId}-time-hint`}
            onChange={(event) => setDraft((current) => ({ ...current, model_sync_time: event.target.value }))}
            className="font-mono"
          />
          <p id={`${fieldId}-time-hint`} className="text-xs leading-relaxed text-muted-foreground">{t('settings.traecnModelSync.timeHint')}</p>
        </div>
        <div className="space-y-1.5">
          <label htmlFor={`${fieldId}-timezone`} className="text-xs font-medium">{t('settings.traecnModelSync.timezone')}</label>
          <Select
            id={`${fieldId}-timezone`}
            aria-label={t('settings.traecnModelSync.timezone')}
            value={draft.model_sync_timezone}
            options={timezoneOptions}
            disabled={saving}
            onValueChange={(value) => setDraft((current) => ({ ...current, model_sync_timezone: value }))}
          />
          <p className="text-xs leading-relaxed text-muted-foreground">{t('settings.traecnModelSync.timezoneHint')}</p>
        </div>
      </div>
      <div className="space-y-1 rounded-lg border border-border bg-muted/25 p-3 text-xs leading-relaxed text-muted-foreground">
        <p>{t('settings.traecnModelSync.catchUp')}</p>
        <p>{t('settings.traecnModelSync.failureHint')}</p>
      </div>
      {!validTime && <p role="alert" className="text-sm text-destructive">{t('settings.traecnModelSync.invalidTime')}</p>}
      {!validTimezone && <p role="alert" className="text-sm text-destructive">{t('settings.traecnModelSync.invalidTimezone')}</p>}
      {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
      <div className="flex flex-wrap justify-end gap-2">
        <Button variant="ghost" disabled={saving || !dirty} onClick={() => { setDraft(saved); setError('') }}><RotateCcw className="size-4" />{t('settings.traecnMapping.reset')}</Button>
        <Button disabled={saving || !dirty || !validTime || !validTimezone} onClick={() => void save()}>
          {saving ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}
          {saving ? t('common.saving') : t('settings.traecnModelSync.save')}
        </Button>
      </div>
    </div>
  )
}
