import { api } from "../api/client";
import { ErrorPanel } from "../components/ErrorPanel";
import { PanelTitle } from "../components/PanelTitle";
import { WorkerTable } from "../components/domainTables";
import { usePolling } from "../hooks/usePolling";

export function WorkersPage() {
  const state = usePolling(() => api.workers(), 1000);
  if (state.error) return <ErrorPanel message={state.error} />;
  return (
    <section className="panel">
      <PanelTitle title="Workers" />
      <WorkerTable rows={state.data?.workers ?? []} />
    </section>
  );
}
