// React hook for wrapping one async UI action with loading and error state.

import { useCallback, useState } from "react";
import { errorMessage } from "../utils/status";

// Wrap an async action so callers get stable run, loading, and error state.
export function useAsyncAction<TArgs extends unknown[], TResult>(action: (...args: TArgs) => Promise<TResult>) {
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");

  // Execute the wrapped action while translating thrown errors into UI text.
  const run = useCallback(async (...args: TArgs) => {
    setLoading(true);
    setError("");
    try {
      return await action(...args);
    } catch (actionError) {
      const message = errorMessage(actionError);
      setError(message);
      throw actionError;
    } finally {
      setLoading(false);
    }
  }, [action]);

  return { run, loading, error, setError };
}
