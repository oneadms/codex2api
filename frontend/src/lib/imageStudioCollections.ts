import type { ImageAsset, ImageGenerationJob, ImagePromptTemplate } from '../types'

export type ImageTemplateSort = 'updated' | 'used' | 'name'
export type ImageOrientation = 'all' | 'square' | 'landscape' | 'portrait'

function searchable(value: string): string {
  return value.normalize('NFKC').trim().toLowerCase()
}

function timestamp(value: string): number {
  const time = Date.parse(value)
  return Number.isFinite(time) ? time : 0
}

export function selectImageTemplates(
  templates: ImagePromptTemplate[],
  filters: { query: string; tag: string; favoritesOnly: boolean; sort: ImageTemplateSort },
): ImagePromptTemplate[] {
  const query = searchable(filters.query)
  const tag = searchable(filters.tag)
  return templates.filter(template => (
    (!filters.favoritesOnly || template.favorite)
    && (!tag || template.tags.some(item => searchable(item) === tag))
    && (!query || searchable([template.name, template.prompt, template.model, template.style, ...template.tags].join('\n')).includes(query))
  )).sort((a, b) => {
    if (filters.sort === 'name') {
      const order = a.name.localeCompare(b.name)
      if (order) return order
    }
    if (filters.sort === 'used' && a.usage_count !== b.usage_count) return b.usage_count - a.usage_count
    return timestamp(b.updated_at) - timestamp(a.updated_at) || b.id - a.id
  })
}

export function imageAssetOrientation(asset: ImageAsset): Exclude<ImageOrientation, 'all'> | 'unknown' {
  const actual = /^(\d+)\s*[x×]\s*(\d+)$/i.exec(asset.actual_size?.trim() || '')
  const width = asset.width > 0 ? asset.width : Number(actual?.[1])
  const height = asset.height > 0 ? asset.height : Number(actual?.[2])
  if (!(width > 0 && height > 0)) return 'unknown'
  return width === height ? 'square' : width > height ? 'landscape' : 'portrait'
}

export function selectImageAssets(
  assets: ImageAsset[],
  filters: { query: string; orientation: ImageOrientation },
  promptForAsset: (asset: ImageAsset) => string,
): ImageAsset[] {
  const query = searchable(filters.query)
  return assets.filter(asset => (
    (filters.orientation === 'all' || imageAssetOrientation(asset) === filters.orientation)
    && (!query || searchable([asset.filename, asset.model, promptForAsset(asset), asset.revised_prompt].join('\n')).includes(query))
  ))
}

export function selectImageJobs(
  jobs: ImageGenerationJob[],
  filters: { query: string; status: string },
): ImageGenerationJob[] {
  const query = searchable(filters.query).replace(/^#(?=\d)/, '')
  return jobs.filter(job => {
    if (filters.status !== 'all' && job.status !== filters.status) return false
    if (!query) return true
    let model = ''
    try { model = JSON.parse(job.params_json || '{}').model || '' } catch { /* Historical rows may have invalid parameters. */ }
    return searchable([job.id, job.prompt, model, job.error_message, job.warning, job.api_key_name].join('\n')).includes(query)
  })
}
