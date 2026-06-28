import { useCallback, useState } from "react";
import { parseEventData, type SSEMessage } from "../api/events";
import { Kpi } from "../components/Kpi";
import { ErrorPanel, Loading } from "../components/ErrorPanel";
import { PanelTitle } from "../components/PanelTitle";
import { TaskTable, WorkerTable, WorkflowTable } from "../components/domainTables";
import { useEventStream } from "../hooks/useEventStream";
import type { Dashboard } from "../types/logserve";

export function OverviewPage() {
  const [dashboard, setDashboard] = useState<Dashboard>();
  const [error, setError] = useState("");
  const handleMessage = useCallback((message: SSEMessage) => {
    if (message.event !== "dashboard") return;
    const payload = parseEventData<{ dashboard: Dashboard }>(message);
    setDashboard(payload.dashboard);
    setError("");
  }, []);
  useEventStream({ intervalMs: 1000 }, { onMessage: handleMessage, onError: setError }, []);
  if (error && !dashboard) return <ErrorPanel message={error} />;
  if (!dashboard) return <Loading />;
  const running = dashboard.tasks.filter((task) => task.status === "RUNNING").length;
  const queued = dashboard.tasks.filter((task) => task.status === "QUEUED").length;
  const succeeded = dashboard.tasks.filter((task) => task.status === "SUCCEEDED").length;
  const failed = dashboard.tasks.filter((task) => task.status === "FAILED").length;
  const workerCapacity = dashboard.workers.reduce((sum, worker) => sum + (worker.capacity || 0), 0);
  return (
    <div className="stack">
      {error && <ErrorPanel message={error} />}
      <div className="kpi-grid">
        <Kpi label="Queue Depth" value={dashboard.queue_depth} tone={dashboard.queue_depth >= dashboard.queue_high_watermark && dashboard.queue_high_watermark > 0 ? "bad" : "neutral"} />
        <Kpi label="Running Tasks" value={running} tone="info" />
        <Kpi label="Queued Tasks" value={queued} />
        <Kpi label="Succeeded" value={succeeded} tone="good" />
        <Kpi label="Failed" value={failed} tone={failed > 0 ? "bad" : "neutral"} />
        <Kpi label="Active Workers" value={dashboard.workers.length} tone="good" />
        <Kpi label="Worker Capacity" value={workerCapacity} />
        <Kpi label="Models" value={dashboard.models.length} />
        <Kpi label="Scheduling" value={dashboard.scheduling_policy} />
        <Kpi label="Log Append" value={`${dashboard.last_log_append_ms || 0} ms`} tone={dashboard.log_append_slow_ms > 0 && dashboard.last_log_append_ms >= dashboard.log_append_slow_ms ? "bad" : "neutral"} />
        <Kpi label="Materializer Lag" value={`${dashboard.metadata_materializer?.eventual_lag_estimate_ms ?? 0} ms`} />
        <Kpi label="Compactable Bytes" value={dashboard.compactable_log_bytes} />
      </div>
      <section className="panel">
        <PanelTitle title="Recent Tasks" action={<a data-nav className="button ghost" href="/tasks">Open</a>} />
        <TaskTable rows={[...dashboard.tasks].reverse().slice(0, 10)} />
      </section>
      <section className="panel split">
        <div>
          <PanelTitle title="Workflows" action={<a data-nav className="button ghost" href="/workflows">Open</a>} />
          <WorkflowTable rows={dashboard.workflows.slice(0, 6)} />
        </div>
        <div>
          <PanelTitle title="Workers" action={<a data-nav className="button ghost" href="/workers">Open</a>} />
          <WorkerTable rows={dashboard.workers.slice(0, 6)} />
        </div>
      </section>
    </div>
  );
}
