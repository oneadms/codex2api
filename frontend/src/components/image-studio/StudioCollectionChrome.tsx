import type { ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { ArrowLeft, ArrowRight, Search, X } from 'lucide-react'
import type { LucideIcon } from 'lucide-react'
import { Button } from '@/components/ui/button'

export function StudioCollectionHero({ eyebrow, title, description, children, actions }: {
  eyebrow: string; title: string; description: string; children?: ReactNode; actions?: ReactNode
}) {
  return (
    <header className="studio-collection-hero">
      <div className="studio-collection-intro">
        <span className="studio-collection-eyebrow">{eyebrow}</span>
        <h2>{title}</h2>
        <p>{description}</p>
        {actions && <div className="studio-collection-hero-actions">{actions}</div>}
      </div>
      {children}
    </header>
  )
}

export function StudioCollectionSearch({ value, onChange, placeholder }: { value: string; onChange: (value: string) => void; placeholder: string }) {
  const { t } = useTranslation()
  return (
    <div className="studio-collection-search">
      <Search className="size-4" aria-hidden="true" />
      <input type="search" value={value} onChange={event => onChange(event.target.value)} placeholder={placeholder} aria-label={placeholder} />
      {value && <button type="button" onClick={() => onChange('')} aria-label={t('images.collections.clearSearch')}><X className="size-3.5" /></button>}
    </div>
  )
}

export function StudioCollectionEmpty({ icon: Icon, title, description, children }: { icon: LucideIcon; title: string; description: string; children?: ReactNode }) {
  return (
    <div className="studio-collection-empty">
      <div className="studio-collection-empty-icon"><Icon className="size-6" strokeWidth={1.5} /></div>
      <h3>{title}</h3>
      <p>{description}</p>
      {children && <div className="mt-5 flex flex-wrap justify-center gap-2">{children}</div>}
    </div>
  )
}

export function StudioCollectionLoading({ rows = false }: { rows?: boolean }) {
  const { t } = useTranslation()
  return (
    <div className={rows ? 'studio-loading-rows' : 'studio-template-grid'} role="status" aria-label={t('common.loading')}>
      <span className="sr-only">{t('common.loading')}</span>
      {Array.from({ length: rows ? 3 : 6 }, (_, index) => <div key={index} className="studio-collection-skeleton" aria-hidden="true"><i /><i /><i /></div>)}
    </div>
  )
}

export function StudioCollectionPagination({ page, pageSize, total, loading, onChange }: { page: number; pageSize: number; total: number; loading: boolean; onChange: (page: number) => void }) {
  const { t } = useTranslation()
  const pages = Math.max(1, Math.ceil(total / pageSize))
  return (
    <nav className="studio-collection-pagination" aria-label={t('images.collections.pagination')}>
      <span>{t('images.collections.pageRange', { start: total ? (page - 1) * pageSize + 1 : 0, end: Math.min(page * pageSize, total), total })}</span>
      <div>
        <Button size="sm" variant="outline" disabled={page <= 1 || loading} onClick={() => onChange(page - 1)}><ArrowLeft className="size-3.5" /><span>{t('common.prev')}</span></Button>
        <span className="studio-page-number">{page}<span>/ {pages}</span></span>
        <Button size="sm" variant="outline" disabled={page >= pages || loading} onClick={() => onChange(page + 1)}><span>{t('common.next')}</span><ArrowRight className="size-3.5" /></Button>
      </div>
    </nav>
  )
}
