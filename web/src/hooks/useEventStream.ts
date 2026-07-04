// React hook that owns authenticated SSE connection lifecycle and reconnects.

import { useEffect, useState, type DependencyList } from "react";
import { consumeEventStream, type EventStreamOptions, type SSEMessage } from "../api/events";
import { errorMessage } from "../utils/status";

const reconnectDelayMs = 1000;

export type EventStreamState = {
  connected: boolean;
  error?: string;
};

export type EventStreamHandlers = {
  onMessage: (message: SSEMessage) => void;
  onError?: (message: string) => void;
};

// Maintain one reconnecting SSE subscription for a React view.
export function useEventStream(options: EventStreamOptions, handlers: EventStreamHandlers, deps: DependencyList = []): EventStreamState {
  const [state, setState] = useState<EventStreamState>({ connected: false });
  const [authRevision, setAuthRevision] = useState(0);

  useEffect(() => {
    // Bump a revision so the stream reconnects with the latest bearer token.
    const onTokenChange = () => setAuthRevision((current) => current + 1);
    window.addEventListener("logserve:token-change", onTokenChange);
    return () => window.removeEventListener("logserve:token-change", onTokenChange);
  }, []);

  useEffect(() => {
    if (options.enabled === false) {
      setState({ connected: false });
      return;
    }
    let active = true;
    const controller = new AbortController();
    // Run the reconnect loop until the component unmounts or the abort signal fires.
    const run = async () => {
      while (active && !controller.signal.aborted) {
        setState({ connected: true });
        try {
          await consumeEventStream(options, handlers.onMessage, controller.signal);
          if (!active || controller.signal.aborted) return;
          setState({ connected: false });
        } catch (error) {
          if (!active || controller.signal.aborted) return;
          const message = errorMessage(error);
          setState({ connected: false, error: message });
          handlers.onError?.(message);
        }
        await delayReconnect(controller.signal);
      }
    };
    void run();
    return () => {
      active = false;
      controller.abort();
    };
  }, [authRevision, ...deps]);
  return state;
}

// Wait between reconnect attempts while still resolving promptly on abort.
function delayReconnect(signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) {
      resolve();
      return;
    }
    const timer = window.setTimeout(resolve, reconnectDelayMs);
    signal.addEventListener("abort", () => {
      window.clearTimeout(timer);
      resolve();
    }, { once: true });
  });
}