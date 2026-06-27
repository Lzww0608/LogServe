import { useEffect, useState, type DependencyList } from "react";
import { errorMessage } from "../utils/status";

export type LoadState<T> = {
  data?: T;
  error?: string;
  loading: boolean;
};

export function usePolling<T>(loader: () => Promise<T>, intervalMs: number, deps: DependencyList = []): LoadState<T> {
  const [state, setState] = useState<LoadState<T>>({ loading: true });
  useEffect(() => {
    let cancelled = false;
    let timer: number | undefined;
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
