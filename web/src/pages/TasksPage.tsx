import { useMemo, useState } from "react";
import { api } from "../api/client";
import { ErrorPanel } from "../components/ErrorPanel";
import { TaskTable } from "../components/domainTables";
import { usePolling } from "../hooks/usePolling";

export function TasksPage() {
  const [query, setQuery] = useState("");
  const [status, setStatus] = useState("");
  const search = useMemo(() => {
    const params = new URLSearchParams();
    if (query.trim()) params.set("q", query.trim());
    if (status) params.set("status", status);
    const encoded = params.toString();
    return encoded ? `?${encoded}` : "";
  }, [query, status]);
  const state = usePolling(() => api.tasks(search), 1000, [search]);
  if (state.error) return <ErrorPanel message={state.error} />;
  return (
    <section className="panel">
      <div className="toolbar">
        <input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="Search tasks" />
        <select value={status} onChange={(event) => setStatus(event.target.value)}>
          <option value="">All status</option>
          <option value="QUEUED">QUEUED</option>
          <option value="RUNNING">RUNNING</option>
          <option value="SUCCEEDED">SUCCEEDED</option>
          <option value="FAILED">FAILED</option>
        </select>
        <a data-nav className="button primary" href="/submit/task">Submit</a>
      </div>
      <TaskTable rows={state.data?.tasks ?? []} />
    </section>
  );
}
