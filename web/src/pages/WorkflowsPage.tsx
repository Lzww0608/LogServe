import { api } from "../api/client";
import { ErrorPanel } from "../components/ErrorPanel";
import { PanelTitle } from "../components/PanelTitle";
import { WorkflowTable } from "../components/domainTables";
import { usePolling } from "../hooks/usePolling";

export function WorkflowsPage() {
  const state = usePolling(() => api.workflows(), 1000);
  if (state.error) return <ErrorPanel message={state.error} />;
  return (
    <section className="panel">
      <PanelTitle title="Workflows" action={<a data-nav className="button primary" href="/workflows/new">New</a>} />
      <WorkflowTable rows={state.data?.workflows ?? []} />
    </section>
  );
}
