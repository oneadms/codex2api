// Trae CN 账号 JSON 导入解析：与后端 admin.TraeCNImportJSON 保持同一套宽容规则，
// 但放在前端是为了能在导入前把条目列出来、让管理员勾选要导入哪几条。
//
// 支持三种输入：
//   { "accounts": [...] }   本页导出的文件；
//   [ {...}, {...} ]        裸数组；
//   { "refresh_token": ...}  单个账号对象。
// 字段名兼容 refresh_token / refreshToken / RefreshToken / rt 等写法。

export interface TraeCNImportPreviewItem {
  /** 原始下标，提交时保留顺序。 */
  index: number
  name: string
  email: string
  user_id: string
  host: string
  proxy_url: string
  refresh_token: string
  access_token: string
  expires_at: string
  machine_id: string
  device_id: string
  enabled?: boolean
  /** 该条目的排除原因（缺 refresh_token 等），有值时不可勾选。 */
  problem?: string
}

export interface TraeCNImportParseResult {
  items: TraeCNImportPreviewItem[]
  /** 整体解析失败的原因。 */
  error?: string
}

function text(value: unknown): string {
  if (value === null || value === undefined) return ''
  if (typeof value === 'string') return value.trim()
  if (typeof value === 'number' || typeof value === 'boolean') return String(value)
  return ''
}

function firstText(source: Record<string, unknown>, keys: readonly string[]): string {
  for (const key of keys) {
    const value = text(source[key])
    if (value) return value
  }
  return ''
}

function toItem(raw: unknown, index: number): TraeCNImportPreviewItem {
  const source = (raw && typeof raw === 'object' && !Array.isArray(raw) ? raw : {}) as Record<string, unknown>
  const item: TraeCNImportPreviewItem = {
    index,
    name: firstText(source, ['name']),
    email: firstText(source, ['email']),
    user_id: firstText(source, ['user_id', 'userId', 'UserID', 'uid']),
    host: firstText(source, ['host', 'traecn_host', 'base_url']),
    proxy_url: firstText(source, ['proxy_url', 'proxyUrl']),
    refresh_token: firstText(source, ['refresh_token', 'refreshToken', 'RefreshToken', 'Refresh_Token', 'rt', 'auth_token', 'token']),
    access_token: firstText(source, ['access_token', 'accessToken', 'AccessToken', 'at']),
    expires_at: firstText(source, ['expires_at', 'expiresAt']),
    machine_id: firstText(source, ['machine_id', 'machineId', 'x_machine_id']),
    device_id: firstText(source, ['device_id', 'deviceId', 'x_device_id']),
  }
  if (typeof source.enabled === 'boolean') item.enabled = source.enabled
  if (!item.refresh_token) item.problem = 'missing_refresh_token'
  return item
}

function entriesOf(value: unknown): unknown[] | null {
  if (Array.isArray(value)) return value
  return null
}

export function parseTraeCNImportJSON(raw: string): TraeCNImportParseResult {
  const trimmed = (raw ?? '').trim()
  if (!trimmed) return { items: [], error: 'empty' }

  let decoded: unknown
  try {
    decoded = JSON.parse(trimmed)
  } catch {
    return { items: [], error: 'invalid_json' }
  }

  let entries: unknown[] | null = entriesOf(decoded)
  if (!entries && decoded && typeof decoded === 'object') {
    const envelope = decoded as Record<string, unknown>
    for (const key of ['accounts', 'trae_accounts', 'data', 'list', 'items']) {
      const nested = entriesOf(envelope[key])
      if (nested) {
        entries = nested
        break
      }
    }
    if (!entries) entries = [envelope]
  }
  if (!entries) return { items: [], error: 'invalid_shape' }
  if (entries.length === 0) return { items: [], error: 'no_accounts' }

  return { items: entries.map((entry, index) => toItem(entry, index)) }
}

/** 只保留可导入的条目，并把它们还原成后端接受的原始 JSON 数组。 */
export function selectTraeCNImportEntries(raw: string, selectedIndices: readonly number[]): unknown[] {
  const trimmed = (raw ?? '').trim()
  if (!trimmed) return []
  let decoded: unknown
  try {
    decoded = JSON.parse(trimmed)
  } catch {
    return []
  }
  let entries = entriesOf(decoded)
  if (!entries && decoded && typeof decoded === 'object') {
    const envelope = decoded as Record<string, unknown>
    for (const key of ['accounts', 'trae_accounts', 'data', 'list', 'items']) {
      const nested = entriesOf(envelope[key])
      if (nested) {
        entries = nested
        break
      }
    }
    if (!entries) entries = [envelope]
  }
  if (!entries) return []
  const picked: unknown[] = []
  for (const index of selectedIndices) {
    const entry = entries[index]
    if (entry !== undefined) picked.push(entry)
  }
  return picked
}

/** 导入预览里显示的中文标签（email / user_id / 主机名 / 行号）。 */
export function traeCNImportPreviewLabel(item: TraeCNImportPreviewItem): string {
  return item.email || item.user_id || item.name || `#${item.index + 1}`
}
