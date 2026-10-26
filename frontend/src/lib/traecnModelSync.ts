import type { AccountOperationSelector, TraeCNModelSyncSettings } from '../types'

export const DEFAULT_TRAECN_MODEL_SYNC_SETTINGS: TraeCNModelSyncSettings = {
  model_sync_enabled: false,
  model_sync_time: '00:00',
  model_sync_timezone: 'Asia/Shanghai',
}

export function normalizeTraeCNModelSyncSettings(settings: Partial<TraeCNModelSyncSettings>): TraeCNModelSyncSettings {
  return {
    model_sync_enabled: settings.model_sync_enabled ?? DEFAULT_TRAECN_MODEL_SYNC_SETTINGS.model_sync_enabled,
    model_sync_time: settings.model_sync_time ?? DEFAULT_TRAECN_MODEL_SYNC_SETTINGS.model_sync_time,
    model_sync_timezone: settings.model_sync_timezone ?? DEFAULT_TRAECN_MODEL_SYNC_SETTINGS.model_sync_timezone,
  }
}

export function isValidModelSyncTime(value: string): boolean {
  return value.length === 5 && /^([01]\d|2[0-3]):[0-5]\d$/.test(value)
}

/** Only changed schedule fields belong in this independent settings update. */
export function buildTraeCNModelSyncPatch(draft: TraeCNModelSyncSettings, saved: TraeCNModelSyncSettings): Partial<TraeCNModelSyncSettings> {
  const patch: Partial<TraeCNModelSyncSettings> = {}
  if (draft.model_sync_enabled !== saved.model_sync_enabled) patch.model_sync_enabled = draft.model_sync_enabled
  if (draft.model_sync_time !== saved.model_sync_time) patch.model_sync_time = draft.model_sync_time
  if (draft.model_sync_timezone !== saved.model_sync_timezone) patch.model_sync_timezone = draft.model_sync_timezone
  return patch
}

type TraeCNModelSyncScope = {
  scope: 'selected' | 'filtered' | 'all'
  count: number
  body: { ids: number[] } | { selector: AccountOperationSelector }
}

/** Capture the full operation scope before confirming; pagination is never a filter. */
export function buildTraeCNModelSyncScope(ids: number[], selector: AccountOperationSelector, total: number): TraeCNModelSyncScope {
  if (ids.length > 0) {
    const selected = Array.from(new Set(ids))
    return { scope: 'selected', count: selected.length, body: { ids: selected } }
  }
  // This page currently exposes only search and status filters. Keep the channel explicit.
  const scopedSelector: AccountOperationSelector = {
    channel: 'traecn',
    search: selector.search || undefined,
    status: selector.status === 'all' ? undefined : selector.status || undefined,
  }
  return {
    scope: scopedSelector.search || scopedSelector.status ? 'filtered' : 'all',
    count: total,
    body: { selector: scopedSelector },
  }
}
