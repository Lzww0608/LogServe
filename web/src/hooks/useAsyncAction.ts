// React hook for wrapping one async UI action with loading and error state.

import { useCallback, useState } from "react";
import { errorMessage } from "../utils/status";

// Wrap an async action so callers get stable run, loading, and error state.
// The generic tuple keeps run arguments aligned with the wrapped async action signature.
export function useAsyncAction<TArgs extends unknown[], TResult>(action: (...args: TArgs) => Promise<TResult>) {
  // This hook intentionally keeps one shared loading/error pair per wrapped action.
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");

  // Execute the wrapped action while translating thrown errors into UI text.
  const run = useCallback(async (...args: TArgs) => {
    setLoading(true);
    // Clear stale error text at the start so retry attempts do not display a previous failure.
    setError("");
    try {
      return await action(...args);
    } catch (actionError) {
      const message = errorMessage(actionError);
      setError(message);
      // Re-throw so callers can decide whether to refresh data, close modals, or keep focus.
      throw actionError;
    } finally {
      // Loading is cleared for both success and failure; overlapping calls share this simple boolean state.
      setLoading(false);
    }
  }, [action]);

  // setError is returned so callers can clear or override inline validation text without re-wrapping run.
  return { run, loading, error, setError };
}
