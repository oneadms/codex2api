export interface DiagnosticSettings {
  enabled: boolean
  auto_run: boolean
  publish: boolean
  repository: string
  model_url: string
  model: string
  interval_minutes: number
  window_hours: number
  min_count: number
  max_per_run: number
  min_confidence: number
  has_api_key: boolean
  has_github_token: boolean
  base_branch: string
}

export interface DiagnosticOutcome {
  fingerprint: string
  status: string
  pr_url?: string
  error?: string
}

export interface DiagnosticStatus {
  running: boolean
  collecting: boolean
  git_available: boolean
  github_cli_available: boolean
  last_started: string | null
  last_finished: string | null
  next_run: string | null
  last_error: string
  dropped_events: number
  last_result: { outcomes: DiagnosticOutcome[] | null; skipped: number }
}

export interface DiagnosticHistoryItem {
  id: string
  status: string
  attempts: number
  updated_at: string
  pr_url?: string
  error?: string
}

export interface DiagnosticIncident {
  fingerprint: string
  count: number
  last_seen: string
  sample: {
    kind: string
    route?: string
    model?: string
    status: number
    message: string
  }
}

export interface DiagnosticReport {
  report: {
    status: string
    base_sha: string
    validation: string
    diagnosis: {
      title: string
      root_cause: string
      confidence: number
      can_fix: boolean
    }
  }
  patch: string
}

export function diagnosticPayload(
  settings: DiagnosticSettings,
  apiKey: string,
  githubToken: string,
  clearKey = false,
  clearToken = false,
) {
  const { has_api_key: _key, has_github_token: _token, ...values } = settings
  return {
    ...values,
    base_branch: 'custom/main',
    ...(apiKey.trim() ? { api_key: apiKey.trim() } : {}),
    ...(githubToken.trim() ? { github_token: githubToken.trim() } : {}),
    ...(clearKey ? { clear_api_key: true } : {}),
    ...(clearToken ? { clear_github_token: true } : {}),
  }
}

export function diagnosticPRLink(value?: string): string | undefined {
  if (!value) return undefined
  try {
    const url = new URL(value)
    if (
      url.protocol === 'https:' &&
      url.hostname === 'github.com' &&
      !url.username &&
      !url.password &&
      /^\/[^/]+\/[^/]+\/pull\/\d+$/.test(url.pathname)
    )
      return url.href
  } catch {
    /* Invalid stored links are not clickable. */
  }
  return undefined
}
