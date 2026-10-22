import { useTranslation } from 'react-i18next'
import { Activity } from 'lucide-react'
import type { HarvestCoverageSnapshot } from '@/lib/codexHarvest'
import { harvestTime } from './shared'

/** snapshot.coverage 的只读展示：平均覆盖率 + 采样窗口。GET 轮询不改变采样。 */
export default function HarvestCoverageCard({ coverage }: { coverage?: HarvestCoverageSnapshot }) {
  const { t } = useTranslation()
  if (!coverage || coverage.account_count === 0) {
    return <p className="flex items-center gap-2 rounded-lg border border-dashed border-border p-4 text-sm text-muted-foreground"><Activity className="size-4 shrink-0" />{t('harvest.coverageEmpty')}</p>
  }
  const percent = coverage.average_percent
  return <div className="rounded-lg border border-border p-4" data-testid="harvest-coverage-card">
    <p className="text-xs text-muted-foreground">{t('harvest.coverageTitle')}</p>
    <p className="mt-1 flex items-baseline gap-1 text-2xl font-semibold tabular-nums">
      {percent === null || percent === undefined ? '—' : `${(percent * 100).toFixed(1)}%`}
    </p>
    <p className="mt-1 text-xs text-muted-foreground">{t('harvest.coverageDetail', { accounts: coverage.account_count, ready: Math.round(coverage.average_ready_seconds), missing: Math.round(coverage.average_missing_seconds), interval: coverage.sample_interval_seconds })}</p>
    <p className="mt-1 text-xs text-muted-foreground">{t('harvest.updatedAt', { time: harvestTime(coverage.updated_at ?? null) })}</p>
  </div>
}
