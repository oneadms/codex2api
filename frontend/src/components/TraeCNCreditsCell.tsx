import { RefreshCw } from "lucide-react";
import { useTranslation } from "react-i18next";
import type { TraeCNCreditsState } from "../hooks/useTraeCNCredits";
import { cn } from "../lib/utils";

export default function TraeCNCreditsCell({ state, onRefresh }: {
  state?: TraeCNCreditsState;
  onRefresh: () => void;
}) {
  const { t, i18n } = useTranslation();
  const data = state?.data;
  const loading = state?.loading ?? true;
  const format = (value: number) => value.toLocaleString(i18n.language, { maximumFractionDigits: 2 });
  const percent = data ? Math.max(0, Math.min(100, data.used_percent)) : 0;
  const refreshButton = (
    <button type="button" onClick={onRefresh} disabled={loading}
      title={t("traecn.creditsRefresh")} aria-label={t("traecn.creditsRefresh")}
      className="shrink-0 rounded p-0.5 text-muted-foreground transition-colors hover:text-foreground disabled:opacity-50">
      <RefreshCw className={cn("size-3", loading && "animate-spin")} />
    </button>
  );

  if (!data) {
    return (
      <div className="flex min-w-[190px] items-center gap-2 text-xs text-muted-foreground" title={state?.error}>
        <span>{loading ? t("common.loading") : t("traecn.creditsUnavailable")}</span>
        {refreshButton}
      </div>
    );
  }

  const updatedAt = new Date(data.updated_at).toLocaleString(i18n.language);
  return (
    <div className="w-[210px] space-y-1 text-[11px] tabular-nums" title={t("traecn.creditsScope")}>
      <div className="flex items-center justify-between gap-2">
        <span className="text-muted-foreground">{t("traecn.creditsTotal")}</span>
        <span className="ml-auto font-semibold">{format(data.total)}</span>
        {refreshButton}
      </div>
      <div className="flex items-center justify-between gap-2">
        <span className="text-muted-foreground">{t("traecn.creditsUsed")}</span>
        <span>{format(data.used)} <span className="text-muted-foreground">· {percent.toFixed(1)}%</span></span>
      </div>
      <div role="progressbar" aria-label={t("traecn.creditsUsed")} aria-valuemin={0} aria-valuemax={100}
        aria-valuenow={percent} aria-valuetext={t("traecn.creditsProgress", { used: format(data.used), total: format(data.total) })}
        className="h-1.5 overflow-hidden rounded-full bg-muted">
        <div className={cn("h-full rounded-full transition-all", percent >= 90 ? "bg-red-500" : percent >= 70 ? "bg-amber-500" : "bg-emerald-500")}
          style={{ width: `${percent}%` }} />
      </div>
      <div className="flex items-center justify-between gap-2 text-muted-foreground">
        <span>{t("traecn.creditsRemaining", { value: format(data.remaining) })}</span>
        <span title={state?.error || t("traecn.creditsUpdatedAt", { time: updatedAt })}
          className={cn("text-[10px]", state?.stale && "text-amber-600 dark:text-amber-400")}>
          {state?.stale ? t("traecn.creditsStale") : new Date(data.updated_at).toLocaleTimeString(i18n.language, { hour: "2-digit", minute: "2-digit" })}
        </span>
      </div>
    </div>
  );
}
