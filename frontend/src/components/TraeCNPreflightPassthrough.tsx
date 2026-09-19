import { useCallback, useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Loader2, RotateCcw, Zap } from 'lucide-react'
import { api } from '../api'
import { useToast } from '../hooks/useToast'
import { getErrorMessage } from '../utils/error'
import { Button } from './ui/button'
import { Switch } from './ui/switch'

// TRAECN 独立的前置元数据开关：默认关闭，开启后上游内容生成前的元数据通知
// 立即下发。与 Codex 侧的 codex_preflight_sse_passthrough 同语义但互不影响。
export default function TraeCNPreflightPassthrough() {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const [enabled, setEnabled] = useState(false)
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')

  const load = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const response = await api.getTraeCNSettings()
      setEnabled(Boolean(response.preflight_sse_passthrough))
    } catch (err) {
      setError(getErrorMessage(err))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => { void load() }, [load])

  const change = async (checked: boolean) => {
    setSaving(true)
    setError('')
    try {
      const response = await api.updateTraeCNSettings({ preflight_sse_passthrough: checked })
      setEnabled(Boolean(response.preflight_sse_passthrough))
      showToast(t('settings.traecnPreflightSSEPassthroughSaved'), 'success')
    } catch (err) {
      setError(getErrorMessage(err))
    } finally {
      setSaving(false)
    }
  }

  if (loading) {
    return <div role="status" className="flex items-center gap-2 text-sm text-muted-foreground"><Loader2 className="size-4 animate-spin" />{t('common.loading')}</div>
  }

  return (
    <div className="rounded-xl border border-border p-4">
      <div className="flex items-start justify-between gap-4">
        <div className="space-y-1.5">
          <div className="flex items-center gap-2 text-sm font-medium">
            <Zap className="size-4 text-muted-foreground" />
            <span>{t('settings.traecnPreflightSSEPassthrough')}</span>
          </div>
          <p className="text-sm leading-relaxed text-muted-foreground">{t('settings.traecnPreflightSSEPassthroughDesc')}</p>
          {enabled && <p className="text-sm leading-relaxed text-amber-600">{t('settings.traecnPreflightSSEPassthroughWarning')}</p>}
        </div>
        <Switch
          aria-label={t('settings.traecnPreflightSSEPassthroughEnabled')}
          checked={enabled}
          disabled={saving}
          onCheckedChange={(checked) => void change(checked)}
        />
      </div>
      {error && (
        <div className="mt-3 flex items-center justify-between gap-3">
          <p role="alert" className="text-sm text-destructive">{error}</p>
          <Button variant="outline" size="sm" onClick={() => void load()}><RotateCcw className="size-4" />{t('common.retry')}</Button>
        </div>
      )}
    </div>
  )
}