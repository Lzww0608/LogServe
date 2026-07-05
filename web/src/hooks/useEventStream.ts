// React hook that owns authenticated SSE connection lifecycle and reconnects.

import { useEffect, useState, type DependencyList } from "react";
import { consumeEventStream, type EventStreamOptions, type SSEMessage } from "../api/events";
import { errorMessage } from "../utils/status";

// reconnectDelayMs keeps transient SSE failures from tight-looping fetches.
const reconnectDelayMs = 1000;

// EventStreamState exposes only UI-facing connection status and last error text.
export type EventStreamState = {
  connected: boolean;
  error?: string;
};

// EventStreamHandlers separates required message handling from optional toast/error reporting.
export type EventStreamHandlers = {
  onMessage: (message: SSEMessage) => void;
  onError?: (message: string) => void;
};

// Maintain one reconnecting SSE subscription for a React view.
// useEventStream expects callers to include every option/handler dependency that should rebuild the stream.
export function useEventStream(options: EventStreamOptions, handlers: EventStreamHandlers, deps: DependencyList = []): EventStreamState {
  const [state, setState] = useState<EventStreamState>({ connected: false });
  // authRevision forces the effect to rebuild headers after token changes without storing the token in state.
  const [authRevision, setAuthRevision] = useState(0);

  useEffect(() => {
    // Bump a revision so the stream reconnects with the latest bearer token.
    const onTokenChange = () => setAuthRevision((current) => current + 1);
    // The event is emitted by setStoredToken; the hook avoids reading sessionStorage during render.
    window.addEventListener("logserve:token-change", onTokenChange);
    return () => window.removeEventListener("logserve:token-change", onTokenChange);
  }, []);

  useEffect(() => {
    // Disabled subscriptions report disconnected and avoid creating an AbortController.
    if (options.enabled === false) {
      // Clearing error here makes a deliberately disabled stream look idle rather than failed.
      setState({ connected: false });
      return;
    }
    let active = true;
    const controller = new AbortController();
    // Run the reconnect loop until the component unmounts or the abort signal fires.
    const run = async () => {
      while (active && !controller.signal.aborted) {
        // Mark connected when a stream attempt starts; consumeEventStream reports server errors through exceptions.
        setState({ connected: true });
        try {
          // consumeEventStream returns only on EOF or abort; normal messages are delivered through the callback.
          await consumeEventStream(options, handlers.onMessage, controller.signal);
          if (!active || controller.signal.aborted) return;
          // Clean EOF resets connection state before the reconnect delay starts.
          setState({ connected: false });
        } catch (error) {
          if (!active || controller.signal.aborted) return;
          const message = errorMessage(error);
          setState({ connected: false, error: message });
          // Surface reconnectable errors to callers without stopping the loop permanently.
          handlers.onError?.(message);
        }
        // Even clean EOF reconnects because the backend may close idle SSE responses.
        await delayReconnect(controller.signal);
      }
    };
    void run();
    return () => {
      active = false;
      // Aborting releases the fetch reader and wakes any pending reconnect delay.
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
    // Resolve the delay on abort so cleanup does not wait for the full reconnect delay.
    const timer = window.setTimeout(resolve, reconnectDelayMs);
    // The listener is one-shot because each delay promise is tied to one reconnect sleep.
    signal.addEventListener("abort", () => {
      window.clearTimeout(timer);
      resolve();
    }, { once: true });
  });
}