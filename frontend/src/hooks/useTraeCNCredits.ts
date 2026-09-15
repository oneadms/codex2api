import { useCallback, useEffect, useRef, useState } from "react";
import { api } from "../api";
import type { TraeCNCreditsSnapshot } from "../types";

export interface TraeCNCreditsState {
  loading: boolean;
  data?: TraeCNCreditsSnapshot;
  stale?: boolean;
  error?: string;
}

type QuerySession = {
  controller: AbortController;
  pending: Map<number, Promise<void>>;
};

// 只查询当前页，最多四个并发；离开页面时取消排队与浏览器请求。
export function useTraeCNCredits(accountIDs: number[]) {
  const [states, setStates] = useState<Record<number, TraeCNCreditsState>>({});
  const session = useRef<QuerySession | null>(null);

  const load = useCallback(function loadCredits(id: number, force = false): Promise<void> {
    const current = session.current;
    if (!current || current.controller.signal.aborted) return Promise.resolve();
    const existing = current.pending.get(id);
    if (existing) return force ? existing.then(() => loadCredits(id, true)) : existing;
    const signal = current.controller.signal;
    setStates(previous => ({ ...previous, [id]: { ...previous[id], loading: true } }));
    const request = (async () => {
      try {
        const response = await api.getTraeCNCredits(id, signal, force);
        if (!signal.aborted) {
          setStates(previous => ({ ...previous, [id]: {
            loading: false, data: response.credits, stale: response.stale, error: response.error,
          } }));
        }
      } catch (error) {
        if (!signal.aborted) {
          setStates(previous => ({ ...previous, [id]: {
            ...previous[id], loading: false, stale: !!previous[id]?.data,
            error: error instanceof Error ? error.message : String(error),
          } }));
        }
      } finally {
        current.pending.delete(id);
      }
    })();
    current.pending.set(id, request);
    return request;
  }, []);

  useEffect(() => {
    const current: QuerySession = { controller: new AbortController(), pending: new Map() };
    session.current = current;
    setStates(previous => Object.fromEntries(accountIDs.map(id => [id, previous[id] ?? { loading: true }])));
    let polling = false;
    const poll = async () => {
      if (polling || document.hidden || current.controller.signal.aborted) return;
      polling = true;
      try {
        const queue = [...accountIDs];
        await Promise.all(Array.from({ length: Math.min(4, queue.length) }, async () => {
          while (queue.length && !current.controller.signal.aborted) {
            const id = queue.shift()!;
            await load(id);
          }
        }));
      } finally {
        polling = false;
      }
    };
    const onVisible = () => { if (!document.hidden) void poll(); };
    void poll();
    const timer = window.setInterval(() => void poll(), 60_000);
    document.addEventListener("visibilitychange", onVisible);
    return () => {
      current.controller.abort();
      session.current = null;
      window.clearInterval(timer);
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, [accountIDs, load]);

  const refresh = useCallback((id: number) => load(id, true), [load]);
  return { states, refresh };
}
