import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Check, ChevronsUpDown, Search } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { cn } from '@/lib/utils'
import type { HarvestAccount } from '@/lib/codexHarvest'

/**
 * 网关借票的账号多选。search + checkbox 列表，选中上限 64；
 * blocked 表示已被对侧（来源/目标/WS）占用的账号，点击时忽略以保持互斥。
 */
export default function HarvestAccountPicker({ label, accounts, selected, onToggle, blocked = [], disabled = false }: {
  label: string
  accounts: HarvestAccount[]
  selected: number[]
  onToggle: (id: number) => void
  blocked?: number[]
  disabled?: boolean
}) {
  const { t } = useTranslation()
  const [query, setQuery] = useState('')
  const blockedSet = useMemo(() => new Set(blocked), [blocked])
  const rows = useMemo(() => {
    const q = query.trim().toLowerCase()
    const known = new Set(accounts.map(account => account.id))
    // Preserve selected IDs even after deletion or a temporarily incomplete
    // snapshot, so the user can remove them instead of being stuck on save.
    const candidates = [
      ...accounts.map(account => ({ id: account.id, email: account.email, missing: false })),
      ...[...new Set(selected)].filter(id => !known.has(id)).map(id => ({ id, email: '', missing: true })),
    ]
    return candidates.filter(account => !q || `${account.email} #${account.id}`.toLowerCase().includes(q))
  }, [accounts, selected, query])
  return <fieldset className="min-w-0 rounded-lg border border-border p-3" data-testid={`harvest-account-picker-${label}`}>
    <legend className="px-1 text-xs font-medium text-muted-foreground">{label}</legend>
    <div className="relative mb-2"><Search className="absolute left-2.5 top-2.5 size-4 text-muted-foreground" /><Input className="h-9 pl-8 text-sm" aria-label={t('harvest.searchAccounts')} placeholder={t('harvest.searchAccounts')} value={query} onChange={event => setQuery(event.target.value)} disabled={disabled} /></div>
    <p className="mb-2 text-xs text-muted-foreground">{t('harvest.selectedCount', { count: selected.length, max: 64 })}</p>
    {rows.length === 0 ? <p className="py-6 text-center text-sm text-muted-foreground">{t('harvest.noAccounts')}</p> : <ul className="max-h-56 space-y-1 overflow-y-auto pr-1" role="group" aria-label={label}>
      {rows.map(account => {
        const isSelected = selected.includes(account.id)
        const isBlocked = blockedSet.has(account.id)
        return <li key={account.id}>
          <label className={cn('flex cursor-pointer items-center gap-2 rounded-md border px-2 py-1.5 text-xs transition-colors', isSelected ? 'border-primary/40 bg-primary/10 text-primary' : isBlocked ? 'cursor-not-allowed border-transparent bg-muted/60 text-muted-foreground/60' : 'border-transparent hover:bg-accent/70', disabled && 'cursor-not-allowed opacity-60')}>
            <input type="checkbox" className="size-4 accent-[color:var(--color-primary)]" checked={isSelected} disabled={disabled || (isBlocked && !isSelected)} onChange={() => onToggle(account.id)} />
            <span className="min-w-0 flex-1 truncate">{account.missing ? t('harvest.accountMissing') : account.email || 'Codex'} <span className="font-mono text-muted-foreground">#{account.id}</span></span>
            {isSelected && <Check className="size-3.5 shrink-0" aria-hidden />}
            {isBlocked && !isSelected && <ChevronsUpDown className="size-3.5 shrink-0 opacity-50" aria-hidden />}
          </label>
        </li>})}
    </ul>}
  </fieldset>
}
