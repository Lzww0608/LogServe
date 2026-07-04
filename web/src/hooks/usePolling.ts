// React hook for interval polling pages that do not consume SSE streams.

import { useEffect, useState, type DependencyList } from "react";
import { errorMessage } from "../utils/status";

export type LoadState<T> = {
  data?: T;
  error?: string;
  loading: boolean;
};

// Load data immediately and optionally refresh it on a fixed interval.
export function usePolling<T>(loader: () => Promise<T>, intervalMs: number, deps: DependencyList = []): LoadState<T> {
  const [state, setState] = useState<LoadState<T>>({ loading: true });
  useEffect(() => {
    let cancelled = false;
    let timer: number | undefined;
    // Run one polling request and ignore late results after unmount.
    const load = async () => {
      try {
        const data = await loader();
        if (!cancelled) setState({ data, loading: false });
      } catch (error) {
        if (!cancelled) setState({ error: errorMessage(error), loading: false });
      }
    };
    setState({ loading: true });
    void load();
    if (intervalMs > 0) {
      timer = window.setInterval(load, intervalMs);
    }
    return () => {
      cancelled = true;
      if (timer) window.clearInterval(timer);
    };
  }, deps);
  return state;
}
