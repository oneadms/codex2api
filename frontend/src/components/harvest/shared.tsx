import type { PropsWithChildren, ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'

export type HarvestAction = (action: () => Promise<unknown>) => Promise<boolean>

export function HarvestSection({ title, actions, children }: PropsWithChildren<{ title: string; actions?: ReactNode }>) {
  return <section className="rounded-xl border border-border bg-card p-4 sm:p-5"><div className="mb-4 flex flex-wrap items-center justify-between gap-3"><h3 className="text-base font-semibold">{title}</h3>{actions}</div>{children}</section>
}

export function HarvestPill({ children, good = false }: PropsWithChildren<{ good?: boolean }>) {
  return <span className={cn('inline-flex items-center rounded-md px-2 py-1 text-xs font-medium', good ? 'bg-emerald-500/10 text-emerald-700 dark:text-emerald-400' : 'bg-muted text-muted-foreground')}>{children}</span>
}

export function HarvestPager({ offset, total, onChange }: { offset: number; total: number; onChange: (value: number) => void }) {
  const { t } = useTranslation()
  return <div className="mt-4 flex flex-wrap items-center justify-between gap-3 text-xs text-muted-foreground"><span>{t('harvest.total', { count: total })}</span><div className="flex gap-2"><Button size="sm" variant="outline" disabled={offset === 0} onClick={() => onChange(Math.max(0, offset - 25))}>{t('harvest.previous')}</Button><Button size="sm" variant="outline" disabled={offset + 25 >= total} onClick={() => onChange(offset + 25)}>{t('harvest.next')}</Button></div></div>
}

export function harvestTime(value?: string | null): string { return value ? new Date(value).toLocaleString() : '—' }

export const harvestTableClass = 'w-full text-left text-sm [&_th]:whitespace-nowrap [&_th]:border-b [&_th]:p-3 [&_th]:text-xs [&_th]:font-medium [&_th]:text-muted-foreground [&_td]:border-b [&_td]:border-border/60 [&_td]:p-3 [&_tr:last-child_td]:border-0'
