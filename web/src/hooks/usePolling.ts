// React hook for interval polling pages that do not consume SSE streams.

import { useEffect, useState, type DependencyList } from "react";
import { errorMessage } from "../utils/status";

// LoadState is intentionally small so pages can render loading, error, or stale data consistently.
export type LoadState<T> = {
  data?: T;
  error?: string;
  loading: boolean;
};

// Load data immediately and optionally refresh it on a fixed interval.
// usePolling expects callers to pass a stable loader or include loader inputs in deps.
export function usePolling<T>(loader: () => Promise<T>, intervalMs: number, deps: DependencyList = []): LoadState<T> {
  const [state, setState] = useState<LoadState<T>>({ loading: true });
  useEffect(() => {
    // cancelled fences late async completions after unmount or dependency replacement.
    let cancelled = false;
    let timer: number | undefined;
    // Run one polling request and ignore late results after unmount.
    const load = async () => {
      try {
        const data = await loader();
        // Successful refresh replaces stale data and clears any previous error.
        if (!cancelled) setState({ data, loading: false });
      } catch (error) {
        // On failure, keep the state compact; pages that need stale data should preserve it externally.
        if (!cancelled) setState({ error: errorMessage(error), loading: false });
      }
    };
    // New dependencies start a fresh loading state instead of showing stale success as current.
    setState({ loading: true });
    void load();
    // intervalMs <= 0 gives pages a one-shot loader without a separate hook.
    if (intervalMs > 0) {
      timer = window.setInterval(load, intervalMs);
    }
    return () => {
      cancelled = true;
      // Clear the interval before the next effect run creates a replacement poller.
      if (timer) window.clearInterval(timer);
    };
  }, deps);
  return state;
}
