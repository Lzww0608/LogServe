// Function registry route with copy and submit-task shortcuts.

import { useState } from "react";
import { api } from "../api/client";
import { InlineError } from "../components/ErrorPanel";
import { PanelTitle } from "../components/PanelTitle";
import { FunctionTable } from "../components/domainTables";
import { usePolling } from "../hooks/usePolling";

// Render registered functions and shortcuts into task submission.
export function FunctionsPage() {
  // Function registrations change less often than task state, so this route polls on a slower cadence.
  const state = usePolling(() => api.functions(), 2000);
  const [message, setMessage] = useState("");

  // Handle the copy hash helper action from the UI.
  const copyHash = async (functionHash: string) => {
    setMessage("");
    try {
      // Browser clipboard access can fail outside secure contexts or without user permission.
      await navigator.clipboard.writeText(functionHash);
      setMessage("Copied hash");
    } catch {
      setMessage("Clipboard unavailable");
    }
  };

  return (
    <section className="panel">
      <PanelTitle title="Function Registry" action={<a data-nav className="button primary" href="/submit/task">Submit Task</a>} />
      {state.error && <InlineError message={state.error} />}
      {message && <span className="subtle">{message}</span>}
      <FunctionTable rows={state.data?.functions ?? []} onCopy={(functionHash) => void copyHash(functionHash)} />
    </section>
  );
}
