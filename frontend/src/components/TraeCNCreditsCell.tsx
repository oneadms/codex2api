import type { ReactNode } from "react";
import { RefreshCw } from "lucide-react";
import { useTranslation } from "react-i18next";
import type { TraeCNCreditsState } from "../hooks/useTraeCNCredits";
import type { TraeCNCreditsPool, TraeCNCreditsPoolKind } from "../types";
import { cn } from "../lib/utils";

const poolLabelKeys: Record<TraeCNCreditsPoolKind, string> = {
  code: "traecn.creditsPoolCode",
  work: "traecn.creditsPoolWork",
};

const poolAccents: Record<TraeCNCreditsPoolKind, string> = {
  code: "text-sky-600 dark:text-sky-400",
  work: "text-violet-600 dark:text-violet-400",
};

function poolLabelKey(kind: string) {
  return poolLabelKeys[kind as TraeCNCreditsPoolKind] ?? "traecn.creditsPoolCode";
}

function poolAccent(kind: string) {
  return poolAccents[kind as TraeCNCreditsPoolKind] ?? poolAccents.code;
}

// Trae CN 把积分按客户端分池：Code 走 IDE，Work 走 Work 端，两个池各用各的，
// 因此这里逐池展示，不合并成一个余额。
function CreditsPoolBlock({ pool, stale, refreshButton }: {
  pool: TraeCNCreditsPool;
  stale?: boolean;
  refreshButton: ReactNode;
}) {
  const { t, i18n } = useTranslation();
  const label = t(poolLabelKey(pool.kind));
  const format = (value: number) => value.toLocaleString(i18n.language, { maximumFractionDigits: 2 });
  const updatedAt = pool.updated_at ? new Date(pool.updated_at).toLocaleString(i18n.language) : "";

  if (pool.error) {
    return (
      <div className="space-y-1">
        <div className="flex items-center justify-between gap-2">
          <span className={cn("font-medium", poolAccent(pool.kind))}>{label}</span>
          {refreshButton}
        </div>
        <div className="text-muted-foreground" title={pool.error}>{t("traecn.creditsPoolUnavailable")}</div>
      </div>
    );
  }

  const percent = Math.max(0, Math.min(100, pool.used_percent));
  return (
    <div className="space-y-1">
      <div className="flex items-center justify-between gap-2">
        <span className={cn("font-medium", poolAccent(pool.kind))}>{label}</span>
        <span className="ml-auto font-semibold">{format(pool.total)}</span>
        {refreshButton}
      </div>
      <div className="flex items-center justify-between gap-2">
        <span className="text-muted-foreground">{t("traecn.creditsUsed")}</span>
        <span>{format(pool.used)} <span className="text-muted-foreground">· {percent.toFixed(1)}%</span></span>
      </div>
      <div role="progressbar" aria-label={t("traecn.creditsPoolProgress", { pool: label })}
        aria-valuemin={0} aria-valuemax={100} aria-valuenow={percent}
        aria-valuetext={t("traecn.creditsPoolProgressValue", { pool: label, used: format(pool.used), total: format(pool.total) })}
        className="h-1.5 overflow-hidden rounded-full bg-muted">
        <div className={cn("h-full rounded-full transition-all", percent >= 90 ? "bg-red-500" : percent >= 70 ? "bg-amber-500" : "bg-emerald-500")}
          style={{ width: `${percent}%` }} />
      </div>
      <div className="flex items-center justify-between gap-2 text-muted-foreground">
        <span>{t("traecn.creditsRemaining", { value: format(pool.remaining) })}</span>
        <span title={stale ? t("traecn.creditsUpdatedAt", { time: updatedAt }) : updatedAt}
          className={cn("text-[10px]", stale && "text-amber-600 dark:text-amber-400")}>
          {stale ? t("traecn.creditsStale") : new Date(pool.updated_at ?? Date.now()).toLocaleTimeString(i18n.language, { hour: "2-digit", minute: "2-digit" })}
        </span>
      </div>
    </div>
  );
}

export default function TraeCNCreditsCell({ state, onRefresh }: {
  state?: TraeCNCreditsState;
  onRefresh: () => void;
}) {
  const { t } = useTranslation();
  const data = state?.data;
  const loading = state?.loading ?? true;
  const pools = data?.pools ?? [];
  const refreshButton = (
    <button type="button" onClick={onRefresh} disabled={loading}
      title={t("traecn.creditsRefresh")} aria-label={t("traecn.creditsRefresh")}
      className="shrink-0 rounded p-0.5 text-muted-foreground transition-colors hover:text-foreground disabled:opacity-50">
      <RefreshCw className={cn("size-3", loading && "animate-spin")} />
    </button>
  );

  if (pools.length === 0) {
    return (
      <div className="flex min-w-[190px] items-center gap-2 text-xs text-muted-foreground" title={state?.error}>
        <span>{loading ? t("common.loading") : t("traecn.creditsUnavailable")}</span>
        {refreshButton}
      </div>
    );
  }

  return (
    <div className="w-[224px] space-y-2 text-[11px] tabular-nums" title={state?.error || t("traecn.creditsScope")}>
      {pools.map((pool, index) => (
        <CreditsPoolBlock key={pool.kind} pool={pool} stale={state?.stale}
          refreshButton={index === 0 ? refreshButton : null} />
      ))}
    </div>
  );
}
