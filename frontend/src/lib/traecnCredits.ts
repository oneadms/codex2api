import type { TraeCNCreditsPool, TraeCNCreditsPoolMode } from "../types";

/** 账号积分可用性：unknown 表示快照过期或没查到，不做判断。 */
export type TraeCNCreditsAvailability = "unknown" | "ok" | "work_only" | "exhausted";

export const CREDITS_EXHAUSTED_BADGE_KEY = "traecn.creditsExhaustedBadge";
export const CREDITS_WORK_ONLY_BADGE_KEY = "traecn.creditsWorkOnlyBadge";

function remainingOf(pools: TraeCNCreditsPool[] | undefined, kind: "code" | "work") {
  const pool = pools?.find((item) => item.kind === kind);
  // 查询失败的池没有余额数据，当作未知而不是 0，避免把"查不到"标成"已用尽"。
  if (!pool || pool.error) return undefined;
  return pool.remaining;
}

/**
 * 汇总账号当下能不能出货，口径与后端 auth.TraeCNCreditsStateFor 一致：
 * Code 池还有额度就是 ok；Code 用尽但 Work 池还有额度（且没被强制只用 Code）是 work_only；
 * 可用的池都是 0 就是 exhausted。
 */
export function traeCNCreditsAvailability(
  mode: TraeCNCreditsPoolMode | undefined,
  pools: TraeCNCreditsPool[] | undefined,
): TraeCNCreditsAvailability {
  const code = remainingOf(pools, "code");
  const work = remainingOf(pools, "work");
  if (code === undefined && work === undefined) return "unknown";
  const normalized = mode ?? "auto";
  const codeUsable = normalized !== "work" && (code ?? 0) > 0;
  const workUsable = normalized !== "code" && (work ?? 0) > 0;
  if (codeUsable) return "ok";
  if (workUsable) return "work_only";
  return "exhausted";
}

/** 额度用尽 / 只剩 Work 池时，状态列要额外显示的徽章文案 key。 */
export function traeCNCreditsBadgeKey(state: TraeCNCreditsAvailability | string | undefined) {
  switch (state) {
    case "exhausted":
      return CREDITS_EXHAUSTED_BADGE_KEY;
    case "work_only":
      return CREDITS_WORK_ONLY_BADGE_KEY;
    default:
      return "";
  }
}
