import {
  Children,
  cloneElement,
  isValidElement,
  type ReactElement,
  useCallback,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from 'react'
import {
  Activity,
  ExternalLink,
  Loader2,
  Play,
  RefreshCw,
  Save,
} from 'lucide-react'
import { api } from '../api'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import { useToast } from '../hooks/useToast'
import { getErrorMessage } from '../utils/error'
import {
  diagnosticPayload,
  diagnosticPRLink,
  type DiagnosticHistoryItem,
  type DiagnosticIncident,
  type DiagnosticReport,
  type DiagnosticSettings,
  type DiagnosticStatus,
} from '../lib/diagnostics'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Badge } from '@/components/ui/badge'
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
} from '@/components/ui/dialog'

const statusLabels: Record<string, string> = {
  diagnosing: '分析中',
  prepared: '补丁已准备',
  published: '已关联 PR',
  no_fix: '需要人工处理',
  failed: '失败',
}
const timeLabel = (value?: string | null) =>
  value ? new Date(value).toLocaleString() : '—'

export default function Diagnostics() {
  const { showToast } = useToast()
  const [settings, setSettings] = useState<DiagnosticSettings | null>(null)
  const [status, setStatus] = useState<DiagnosticStatus | null>(null)
  const [history, setHistory] = useState<DiagnosticHistoryItem[]>([])
  const [incidents, setIncidents] = useState<DiagnosticIncident[]>([])
  const [apiKey, setAPIKey] = useState('')
  const [githubToken, setGitHubToken] = useState('')
  const [clearKey, setClearKey] = useState(false)
  const [clearToken, setClearToken] = useState(false)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState<'save' | 'run' | 'refresh' | null>(null)
  const [dirty, setDirty] = useState(false)
  const [report, setReport] = useState<DiagnosticReport | null>(null)
  const [reportLoading, setReportLoading] = useState<string | null>(null)
  const finishedRef = useRef<string | null>(null)

  const refreshRecords = useCallback(async () => {
    const [runs, errors] = await Promise.all([
      api.getDiagnosticHistory(),
      api.getDiagnosticIncidents(),
    ])
    setHistory(runs.items)
    setIncidents(errors.groups)
  }, [])

  const load = useCallback(async () => {
    setLoading(true)
    try {
      const [config, runtime] = await Promise.all([
        api.getDiagnosticSettings(),
        api.getDiagnosticStatus(),
      ])
      setSettings(config)
      setStatus(runtime)
      finishedRef.current = runtime.last_finished
      await refreshRecords()
      setError('')
    } catch (e) {
      setError(getErrorMessage(e, '加载诊断配置失败'))
    } finally {
      setLoading(false)
    }
  }, [refreshRecords])

  useEffect(() => {
    void load()
  }, [load])
  useEffect(() => {
    let active = true
    const timer = window.setInterval(async () => {
      if (document.hidden) return
      try {
        const runtime = await api.getDiagnosticStatus()
        if (!active) return
        setStatus(runtime)
        if (finishedRef.current !== runtime.last_finished) {
          finishedRef.current = runtime.last_finished
          await refreshRecords()
        }
      } catch {
        /* Manual refresh presents connection errors without toast storms. */
      }
    }, 15000)
    return () => {
      active = false
      window.clearInterval(timer)
    }
  }, [refreshRecords])

  const change = <K extends keyof DiagnosticSettings>(
    key: K,
    value: DiagnosticSettings[K],
  ) => {
    setSettings((previous) =>
      previous
        ? {
            ...previous,
            [key]: value,
            ...(key === 'enabled' && !value ? { auto_run: false } : {}),
          }
        : previous,
    )
    setDirty(true)
  }

  const save = async () => {
    if (!settings) return
    setBusy('save')
    try {
      const saved = await api.saveDiagnosticSettings(
        diagnosticPayload(settings, apiKey, githubToken, clearKey, clearToken),
      )
      setSettings(saved)
      setAPIKey('')
      setGitHubToken('')
      setClearKey(false)
      setClearToken(false)
      setDirty(false)
      setStatus(await api.getDiagnosticStatus())
      await refreshRecords()
      showToast('设置已保存，服务器已应用新配置', 'success')
    } catch (e) {
      showToast(getErrorMessage(e, '保存失败'), 'error')
    } finally {
      setBusy(null)
    }
  }

  const run = async () => {
    setBusy('run')
    try {
      setStatus(await api.runDiagnosticScan())
      showToast('服务器已开始扫描', 'success')
    } catch (e) {
      showToast(getErrorMessage(e, '启动失败'), 'error')
    } finally {
      setBusy(null)
    }
  }

  const refresh = async () => {
    setBusy('refresh')
    try {
      setStatus(await api.getDiagnosticStatus())
      await refreshRecords()
    } catch (e) {
      showToast(getErrorMessage(e, '刷新失败'), 'error')
    } finally {
      setBusy(null)
    }
  }

  const openReport = async (id: string) => {
    setReportLoading(id)
    try {
      setReport(await api.getDiagnosticReport(id))
    } catch (e) {
      showToast(getErrorMessage(e, '该记录暂无报告'), 'error')
    } finally {
      setReportLoading(null)
    }
  }

  return (
    <StateShell
      variant="page"
      loading={loading}
      error={error}
      onRetry={() => void load()}
    >
      <PageHeader
        title="AI 诊断"
        description="在服务器上收集错误、分析原因并准备修复。配置保存后持久生效，重启服务会自动恢复。"
        actions={
          <div className="flex flex-wrap gap-2">
            <Button
              variant="outline"
              disabled={busy !== null}
              onClick={() => void refresh()}
            >
              <RefreshCw className="size-4" />
              刷新
            </Button>
            <Button
              variant="outline"
              disabled={
                busy !== null || status?.running || !status?.collecting || dirty
              }
              onClick={() => void run()}
            >
              {status?.running ? (
                <Loader2 className="size-4 animate-spin" />
              ) : (
                <Play className="size-4" />
              )}
              立即扫描
            </Button>
            <Button
              disabled={busy !== null || !settings}
              onClick={() => void save()}
            >
              {busy === 'save' ? (
                <Loader2 className="size-4 animate-spin" />
              ) : (
                <Save className="size-4" />
              )}
              保存配置
            </Button>
          </div>
        }
      />

      <div className="mb-6 grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
        <Summary
          label="日志采集"
          value={status?.collecting ? '采集中' : '已关闭'}
        />
        <Summary
          label="后台任务"
          value={status?.running ? '正在诊断' : '空闲'}
          detail={`最近完成：${timeLabel(status?.last_finished)}`}
        />
        <Summary
          label="下次扫描"
          value={timeLabel(status?.next_run)}
          detail="自动扫描开启后按间隔运行"
        />
        <Summary
          label="修复目标"
          value="custom/main"
          detail="草稿 PR · 人工合并与部署"
        />
      </div>
      {status?.last_error && (
        <div
          role="alert"
          className="mb-5 rounded-lg border border-destructive/30 bg-destructive/5 p-4 text-sm text-destructive break-words"
        >
          {status.last_error}
        </div>
      )}
      {status && (!status.git_available || !status.github_cli_available) && (
        <div className="mb-5 rounded-lg border border-border bg-muted/40 p-4 text-sm">
          服务器缺少 {!status.git_available ? 'Git ' : ''}
          {!status.github_cli_available ? 'GitHub CLI' : ''}。请更新到内置诊断
          worker 的 Docker 镜像。
        </div>
      )}
      {dirty && (
        <p className="mb-4 text-sm text-muted-foreground">
          有未保存的修改。保存后可以使用“立即扫描”。
        </p>
      )}

      {settings && (
        <div className="mb-6 grid gap-5 xl:grid-cols-2">
          <Card>
            <CardContent className="space-y-5 p-6">
              <div>
                <h2 className="font-semibold">运行配置</h2>
                <p className="mt-1 text-sm text-muted-foreground">
                  worker 与服务一起运行，无需单独启动命令或配置环境文件。
                </p>
              </div>
              <Toggle
                label="开启错误采集"
                checked={settings.enabled}
                onChange={(value) => change('enabled', value)}
                description="采集 5xx、panic 及已接入的上游错误。"
              />
              <Toggle
                label="自动诊断"
                checked={settings.auto_run}
                disabled={!settings.enabled}
                onChange={(value) => change('auto_run', value)}
                description="按设定间隔扫描；也可以只采集，再手动扫描。"
              />
              <Toggle
                label="自动创建草稿 PR"
                checked={settings.publish}
                onChange={(value) => change('publish', value)}
                description="关闭时只生成报告和补丁；开启后推送修复分支，PR 目标固定为 custom/main。"
              />
              <div className="grid gap-4 sm:grid-cols-2">
                <Field label="扫描间隔（分钟）">
                  <Input
                    type="number"
                    min={1}
                    max={1440}
                    value={settings.interval_minutes}
                    onChange={(e) =>
                      change('interval_minutes', Number(e.target.value))
                    }
                  />
                </Field>
                <Field label="回溯时间（小时）">
                  <Input
                    type="number"
                    min={1}
                    max={168}
                    value={settings.window_hours}
                    onChange={(e) =>
                      change('window_hours', Number(e.target.value))
                    }
                  />
                </Field>
                <Field label="最少出现次数">
                  <Input
                    type="number"
                    min={1}
                    max={10000}
                    value={settings.min_count}
                    onChange={(e) =>
                      change('min_count', Number(e.target.value))
                    }
                  />
                </Field>
                <Field label="每次最多诊断问题数">
                  <Input
                    type="number"
                    min={1}
                    max={10}
                    value={settings.max_per_run}
                    onChange={(e) =>
                      change('max_per_run', Number(e.target.value))
                    }
                  />
                </Field>
                <Field label="生成补丁的置信度下限">
                  <Input
                    type="number"
                    min={0}
                    max={1}
                    step={0.05}
                    value={settings.min_confidence}
                    onChange={(e) =>
                      change('min_confidence', Number(e.target.value))
                    }
                  />
                </Field>
              </div>
            </CardContent>
          </Card>
          <Card>
            <CardContent className="space-y-5 p-6">
              <div>
                <h2 className="font-semibold">模型与 GitHub</h2>
                <p className="mt-1 text-sm text-muted-foreground">
                  配置保存在服务端。密钥保存后不回显，留空可保留已有值。
                </p>
              </div>
              <Field
                label="模型 API 地址"
                hint="填写完整的 Chat Completions 地址；远程地址使用 HTTPS。"
              >
                <Input
                  type="url"
                  placeholder="https://api.example.com/v1/chat/completions"
                  value={settings.model_url}
                  onChange={(e) => change('model_url', e.target.value)}
                />
              </Field>
              <Field label="模型名称">
                <Input
                  placeholder="你的分析模型"
                  value={settings.model}
                  onChange={(e) => change('model', e.target.value)}
                />
              </Field>
              <Field
                label="模型 API Key"
                hint={
                  settings.has_api_key
                    ? '已保存，留空保留原值。'
                    : '填入模型服务的 API Key。'
                }
              >
                <Input
                  type="password"
                  autoComplete="new-password"
                  value={apiKey}
                  onChange={(e) => {
                    setAPIKey(e.target.value)
                    setDirty(true)
                  }}
                />
                {settings.has_api_key && (
                  <label className="flex items-center gap-2 text-xs text-muted-foreground">
                    <input
                      type="checkbox"
                      checked={clearKey}
                      onChange={(e) => {
                        setClearKey(e.target.checked)
                        setDirty(true)
                      }}
                    />
                    删除已保存的模型密钥
                  </label>
                )}
              </Field>
              <Field
                label="GitHub 仓库"
                hint="服务器自动拉取此仓库的 custom/main，无需手动 git clone。"
              >
                <Input
                  placeholder="oneadms/codex2api"
                  value={settings.repository}
                  onChange={(e) => change('repository', e.target.value)}
                />
              </Field>
              <Field
                label="GitHub Token"
                hint={
                  settings.has_github_token
                    ? '已保存。更换时输入新 Token；留空保留原值。'
                    : '私有仓库读取或创建 PR 时需要；授权目标仓库 Contents 与 Pull requests 读写。'
                }
              >
                <Input
                  type="password"
                  autoComplete="new-password"
                  value={githubToken}
                  onChange={(e) => {
                    setGitHubToken(e.target.value)
                    setDirty(true)
                  }}
                />
                {settings.has_github_token && (
                  <label className="flex items-center gap-2 text-xs text-muted-foreground">
                    <input
                      type="checkbox"
                      checked={clearToken}
                      onChange={(e) => {
                        setClearToken(e.target.checked)
                        setDirty(true)
                      }}
                    />
                    删除已保存的 GitHub Token
                  </label>
                )}
              </Field>
            </CardContent>
          </Card>
        </div>
      )}

      <Card className="mb-6">
        <CardContent className="p-6">
          <h2 className="mb-4 font-semibold">最近诊断记录</h2>
          {history.length === 0 ? (
            <p className="py-6 text-sm text-muted-foreground">
              暂无诊断记录。开启采集后，满足次数阈值的错误会进入诊断。
            </p>
          ) : (
            <div className="overflow-x-auto">
              <table className="w-full text-left text-sm">
                <thead className="border-b text-muted-foreground">
                  <tr>
                    <th className="py-3 font-medium">错误指纹</th>
                    <th className="font-medium">结果</th>
                    <th className="font-medium">最近更新</th>
                    <th className="font-medium">操作</th>
                  </tr>
                </thead>
                <tbody>
                  {history.map((item) => (
                    <tr key={item.id} className="border-b last:border-0">
                      <td className="py-4">
                        <code>{item.id.slice(0, 12)}</code>
                        {item.error && (
                          <p className="mt-1 max-w-md break-words text-xs text-destructive">
                            {item.error}
                          </p>
                        )}
                      </td>
                      <td>
                        <Badge variant="outline">
                          {statusLabels[item.status] || item.status}
                        </Badge>
                      </td>
                      <td className="whitespace-nowrap pr-3">
                        {timeLabel(item.updated_at)}
                      </td>
                      <td>
                        <div className="flex gap-2">
                          <Button
                            size="sm"
                            variant="outline"
                            disabled={
                              reportLoading !== null ||
                              item.status === 'diagnosing'
                            }
                            onClick={() => void openReport(item.id)}
                          >
                            {reportLoading === item.id && (
                              <Loader2 className="size-3 animate-spin" />
                            )}
                            查看报告
                          </Button>
                          {diagnosticPRLink(item.pr_url) && (
                            <a
                              className="inline-flex items-center gap-1 text-primary"
                              href={diagnosticPRLink(item.pr_url)}
                              target="_blank"
                              rel="noreferrer"
                            >
                              PR
                              <ExternalLink className="size-3" />
                            </a>
                          )}
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardContent className="p-6">
          <div className="mb-4 flex items-center gap-2">
            <Activity className="size-4" />
            <h2 className="font-semibold">近期错误</h2>
          </div>
          {incidents.length === 0 ? (
            <p className="py-6 text-sm text-muted-foreground">
              当前时间窗口内没有采集到错误。
            </p>
          ) : (
            <div className="space-y-3">
              {incidents.map((item) => (
                <div
                  key={item.fingerprint}
                  className="rounded-lg border border-border p-4"
                >
                  <div className="flex flex-wrap items-center gap-2">
                    <Badge variant="outline">{item.sample.status}</Badge>
                    <span className="font-medium">
                      {item.sample.route || item.sample.kind}
                    </span>
                    <span className="text-sm text-muted-foreground">
                      {item.count} 次 · {timeLabel(item.last_seen)}
                    </span>
                  </div>
                  <p className="mt-2 break-words text-sm text-muted-foreground">
                    {item.sample.message}
                  </p>
                </div>
              ))}
            </div>
          )}
        </CardContent>
      </Card>

      <Dialog
        open={report !== null}
        onOpenChange={(open) => {
          if (!open) setReport(null)
        }}
      >
        {report && (
          <DialogContent className="sm:max-w-5xl">
            <DialogHeader>
              <DialogTitle>
                {report.report.diagnosis.title || '诊断报告'}
              </DialogTitle>
              <DialogDescription className="break-all">
                基线：{report.report.base_sha}
              </DialogDescription>
            </DialogHeader>
            <p className="whitespace-pre-wrap text-sm leading-6">
              {report.report.diagnosis.root_cause}
            </p>
            <p className="text-sm text-muted-foreground">
              {report.report.validation}
            </p>
            {report.patch && (
              <pre className="overflow-x-auto rounded-lg bg-muted p-4 text-xs leading-5">
                {report.patch}
              </pre>
            )}
          </DialogContent>
        )}
      </Dialog>
    </StateShell>
  )
}

function Field({
  label,
  hint,
  children,
}: {
  label: string
  hint?: string
  children: ReactNode
}) {
  return (
    <div className="grid gap-2 text-sm">
      <span className="font-medium">{label}</span>
      {Children.map(children, (child) =>
        isValidElement(child) && child.type === Input
          ? cloneElement(child as ReactElement<{ 'aria-label': string }>, {
              'aria-label': label,
            })
          : child,
      )}
      {hint && (
        <span className="text-xs leading-5 text-muted-foreground">{hint}</span>
      )}
    </div>
  )
}

function Toggle({
  label,
  description,
  checked,
  disabled,
  onChange,
}: {
  label: string
  description: string
  checked: boolean
  disabled?: boolean
  onChange: (value: boolean) => void
}) {
  return (
    <label className="flex items-start justify-between gap-4">
      <div>
        <div className="text-sm font-medium">{label}</div>
        <p className="mt-1 text-xs leading-5 text-muted-foreground">
          {description}
        </p>
      </div>
      <input
        type="checkbox"
        aria-label={label}
        className="mt-1 size-4 accent-primary"
        checked={checked}
        disabled={disabled}
        onChange={(e) => onChange(e.target.checked)}
      />
    </label>
  )
}

function Summary({
  label,
  value,
  detail,
}: {
  label: string
  value: string
  detail?: string
}) {
  return (
    <Card>
      <CardContent className="p-5">
        <p className="text-xs text-muted-foreground">{label}</p>
        <p className="mt-2 break-words text-base font-semibold">{value}</p>
        {detail && (
          <p className="mt-2 text-xs text-muted-foreground">{detail}</p>
        )}
      </CardContent>
    </Card>
  )
}
