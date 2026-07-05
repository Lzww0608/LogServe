// Worker inventory route for capacity, heartbeat, and cached model status.

import { api } from "../api/client";
import { ErrorPanel } from "../components/ErrorPanel";
import { PanelTitle } from "../components/PanelTitle";
import { WorkerTable } from "../components/domainTables";
import { usePolling } from "../hooks/usePolling";

// Render polled worker capacity, heartbeat, and cache state.
export function WorkersPage() {
  // Worker heartbeats and cached-model lists are live diagnostics, so keep this page on a short poll.
  const state = usePolling(() => api.workers(), 1000);
  if (state.error) return <ErrorPanel message={state.error} />;
  return (
    <section className="panel">
      <PanelTitle title="Workers" />
      <WorkerTable rows={state.data?.workers ?? []} />
    </section>
  );
}
