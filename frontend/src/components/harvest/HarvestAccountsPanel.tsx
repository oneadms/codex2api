import { useTranslation } from 'react-i18next'
import { Switch } from '@/components/ui/switch'
import { harvestTicketVerification, type HarvestAccount } from '@/lib/codexHarvest'
import { HarvestPill, HarvestSection, harvestTableClass } from './shared'

export default function HarvestAccountsPanel({ accounts, busy, onSkipChange }: {
  accounts: HarvestAccount[]
  busy: boolean
  onSkipChange: (accountID: number, skipped: boolean) => void
}) {
  const { t } = useTranslation()
  return <HarvestSection title={t('harvest.accounts')}>
    <p className="mb-2 text-xs leading-relaxed text-muted-foreground">{t('harvest.binding')}</p>
    <p className="mb-4 text-xs leading-relaxed text-muted-foreground">{t('harvest.ticketStatusHint')}</p>
    {accounts.length === 0 ? <p className="py-6 text-sm text-muted-foreground">{t('harvest.noAccounts')}</p> : <div className="overflow-x-auto">
      <table className={harvestTableClass}>
        <thead><tr>{['account', 'model', 'ticketHolding', 'probeHistory', 'cookies', 'standby', 'skip'].map(key => <th key={key}>{t(`harvest.${key}`)}</th>)}</tr></thead>
        <tbody>{accounts.map(account => <tr key={account.id}>
          <td className="min-w-40 max-w-64 break-words">
            <span>{account.email || 'Codex'}</span>
            <span className="mt-1 block text-xs text-muted-foreground">#{account.id} {account.busy ? `· ${t('harvest.busyAccount')}` : ''}</span>
          </td>
          <td className="whitespace-nowrap font-mono text-xs">{(account.tickets ?? []).map(ticket => <div className="py-2" key={ticket.model}>{ticket.model}</div>)}</td>
          <td>{(account.tickets ?? []).map(ticket => <div className="flex items-center gap-2 py-1.5" key={ticket.model}>
            <HarvestPill good={ticket.ready}>{t(ticket.ready ? 'harvest.ticketHeld' : 'harvest.ticketUnavailable')}</HarvestPill>
            <span className="whitespace-nowrap font-mono text-xs">{ticket.ready ? t('harvest.remaining', { seconds: ticket.remaining_seconds }) : '—'}</span>
          </div>)}</td>
          <td>{(account.tickets ?? []).map(ticket => {
            const verification = harvestTicketVerification(ticket)
            return <div className="flex items-center gap-2 py-1.5" key={ticket.model}>
              <HarvestPill good={verification.good}>{t(verification.labelKey)}</HarvestPill>
              <span className="text-xs text-muted-foreground">{ticket.transport?.toUpperCase() || '—'}</span>
            </div>
          })}</td>
          <td>{(account.tickets ?? []).map(ticket => <div className="whitespace-nowrap py-2 font-mono text-xs" key={ticket.model}>{ticket.cookie_count || 0} · {ticket.cookie_expired ? t('harvest.expired') : `${ticket.cookie_remaining_seconds || 0}s`}</div>)}</td>
          <td>{(account.tickets ?? []).map(ticket => <div className="py-1.5" key={ticket.model}><HarvestPill good={ticket.standby_ready}>{t(ticket.standby_ready ? 'harvest.ready' : 'harvest.empty')}</HarvestPill></div>)}</td>
          <td><Switch aria-label={`${t('harvest.skip')} #${account.id}`} disabled={busy} checked={account.skipped} onCheckedChange={value => onSkipChange(account.id, value)} /></td>
        </tr>)}</tbody>
      </table>
    </div>}
  </HarvestSection>
}
