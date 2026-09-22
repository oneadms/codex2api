import { useCallback, useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Loader2, RefreshCw, RotateCcw, Ticket } from 'lucide-react'
import { api } from '../api'
import type { CodexTicketSettingsResponse } from '../types'
import { useToast } from '../hooks/useToast'
import { getErrorMessage } from '../utils/error'
import { Button } from './ui/button'
import { Input } from './ui/input'
import { Switch } from './ui/switch'

// Codex turn state 后台打票配置。独立读写 /settings/codex-ticket，改动即时保存，
// 不受通用设置保存影响。
//
// 界面要传达两件容易被误解的事：
//   1. 采票使用专用代理，票据携带出口快照，业务注入后沿用该快照。
//   2. FailClosed 打开时，门控模型上没票的账号会被直接拒绝出站，而不是裸打上游。
export default function CodexTicketSettings() {
  const { t } = useTranslation()
  const { showToast } = useToast()
  const [settings, setSettings] = useState<CodexTicketSettingsResponse | null>(null)
  const [modelsText, setModelsText] = useState('')
  const [proxyText, setProxyText] = useState('')
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')

  const applyResponse = useCallback((response: CodexTicketSettingsResponse) => {
    setSettings(response)
    setModelsText((response.models ?? []).join(', '))
    setProxyText(response.harvest_proxy_url ?? '')
  }, [])

  const load = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      applyResponse(await api.getCodexTicketSettings())
    } catch (err) {
      setError(getErrorMessage(err))
    } finally {
      setLoading(false)
    }
  }, [applyResponse])

  useEffect(() => { void load() }, [load])

  const patch = useCallback(async (body: Record<string, unknown>) => {
    setSaving(true)
    setError('')
    try {
      applyResponse(await api.updateCodexTicketSettings(body as never))
      showToast(t('settings.codexTicket.saved'), 'success')
    } catch (err) {
      setError(getErrorMessage(err))
      // 保存失败时把界面拉回服务端的真实状态，避免显示一份没生效的配置。
      void load()
    } finally {
      setSaving(false)
    }
  }, [applyResponse, load, showToast, t])

  const modelList = useMemo(
    () => modelsText.split(',').map((entry) => entry.trim().toLowerCase()).filter((entry, index, all) => entry !== '' && all.indexOf(entry) === index),
    [modelsText],
  )

  const commitModels = useCallback(() => {
    const current = (settings?.models ?? []).join(', ')
    const next = modelList.join(', ')
    if (next === current) return
    void patch({ models: modelList })
  }, [modelList, patch, settings])

  const commitProxy = useCallback(() => {
    if (proxyText === (settings?.harvest_proxy_url ?? '')) return
    void patch({ harvest_proxy_url: proxyText })
  }, [patch, proxyText, settings])

  if (loading) {
    return <div role="status" className="flex items-center gap-2 text-sm text-muted-foreground"><Loader2 className="size-4 animate-spin" />{t('common.loading')}</div>
  }

  return (
    <div className="space-y-4">
      <div className="flex items-start justify-between gap-4 rounded-xl border border-border p-4">
        <div className="space-y-1.5">
          <div className="flex items-center gap-2 text-sm font-medium">
            <Ticket className="size-4 text-muted-foreground" />
            <span>{t('settings.codexTicket.enabled')}</span>
          </div>
          <p className="text-sm leading-relaxed text-muted-foreground">{t('settings.codexTicket.enabledDesc')}</p>
          {settings?.enabled && !settings.gate_active ? (
            <p className="text-sm leading-relaxed text-amber-600">{t('settings.codexTicket.gateInactive')}</p>
          ) : null}
          {settings?.gate_active ? (
            <p className="text-sm leading-relaxed text-muted-foreground">
              {t('settings.codexTicket.ready', { ready: settings.ready_accounts, total: settings.total_accounts })}
            </p>
          ) : null}
        </div>
        <Switch
          aria-label={t('settings.codexTicket.enabled')}
          checked={Boolean(settings?.enabled)}
          disabled={saving}
          onCheckedChange={(checked) => void patch({ enabled: checked })}
        />
      </div>

      <div className="grid gap-4 sm:grid-cols-2">
        <div className="space-y-1.5">
          <label className="text-sm font-medium" htmlFor="codex-ticket-proxy">{t('settings.codexTicket.proxy')}</label>
          <Input
            id="codex-ticket-proxy"
            value={proxyText}
            placeholder="socks5h://user:pass@host:1080"
            disabled={saving}
            onChange={(event) => setProxyText(event.target.value)}
            onBlur={commitProxy}
          />
          <p className="text-sm leading-relaxed text-muted-foreground">{t('settings.codexTicket.proxyDesc')}</p>
        </div>
        <div className="space-y-1.5">
          <label className="text-sm font-medium" htmlFor="codex-ticket-models">{t('settings.codexTicket.models')}</label>
          <Input
            id="codex-ticket-models"
            value={modelsText}
            placeholder="gpt-5.2-codex"
            disabled={saving}
            onChange={(event) => setModelsText(event.target.value)}
            onBlur={commitModels}
          />
          <p className="text-sm leading-relaxed text-muted-foreground">{t('settings.codexTicket.modelsDesc')}</p>
        </div>
      </div>

      <div className="flex items-start justify-between gap-4 rounded-xl border border-border p-4">
        <div className="space-y-1.5">
          <div className="text-sm font-medium">{t('settings.codexTicket.failClosed')}</div>
          <p className="text-sm leading-relaxed text-muted-foreground">{t('settings.codexTicket.failClosedDesc')}</p>
        </div>
        <Switch
          aria-label={t('settings.codexTicket.failClosed')}
          checked={Boolean(settings?.fail_closed)}
          disabled={saving}
          onCheckedChange={(checked) => void patch({ fail_closed: checked })}
        />
      </div>

      {error ? (
        <div className="flex items-center justify-between gap-3">
          <p role="alert" className="text-sm text-destructive">{error}</p>
          <Button variant="outline" size="sm" onClick={() => void load()}><RotateCcw className="size-4" />{t('common.retry')}</Button>
        </div>
      ) : null}
      <div className="flex justify-end">
        <Button variant="outline" size="sm" disabled={loading} onClick={() => void load()}>
          <RefreshCw className="size-4" />{t('common.refresh')}
        </Button>
      </div>
    </div>
  )
}
