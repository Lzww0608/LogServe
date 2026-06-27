import { useCallback, useState } from "react";
import { errorMessage } from "../utils/status";

export function useAsyncAction<TArgs extends unknown[], TResult>(action: (...args: TArgs) => Promise<TResult>) {
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");

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
