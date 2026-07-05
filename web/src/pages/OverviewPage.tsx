// Overview route for dashboard metrics and live SSE summary updates.

import { useCallback, useState } from "react";
import { parseEventData, type SSEMessage } from "../api/events";
import { Kpi } from "../components/Kpi";
import { ErrorPanel, Loading } from "../components/ErrorPanel";
import { PanelTitle } from "../components/PanelTitle";
import { TaskTable, WorkerTable, WorkflowTable } from "../components/domainTables";
import { useEventStream } from "../hooks/useEventStream";
import type { ConsoleSession, Dashboard } from "../types/logserve";
import { roleAtLeast } from "../utils/roles";

// Render the dashboard snapshot and apply live SSE status updates.
export function OverviewPage({ session }: { session?: ConsoleSession | null }) {
  const [dashboard, setDashboard] = useState<Dashboard>();
  const [error, setError] = useState("");
  const [lastRefreshAt, setLastRefreshAt] = useState<number>();
  // Apply dashboard SSE snapshots and record the last successful refresh time.
  const handleMessage = useCallback((message: SSEMessage) => {
    if (message.event !== "dashboard") return;
    const payload = parseEventData<{ dashboard: Dashboard }>(message);
    setDashboard(payload.dashboard);
    setLastRefreshAt(Date.now());
    setError("");
  }, []);
  // The dashboard stream emits full snapshots, so replacing local state is safe on each event.
  const streamState = useEventStream({ intervalMs: 1000 }, { onMessage: handleMessage, onError: setError }, []);
  if (error && !dashboard) return <ErrorPanel message={error} />;
  if (!dashboard) return <Loading />;

  const running = dashboard.tasks.filter((task) => task.status === "RUNNING").length;
  const queued = dashboard.tasks.filter((task) => task.status === "QUEUED").length;
  const succeeded = dashboard.tasks.filter((task) => task.status === "SUCCEEDED").length;
  const failedTasks = dashboard.tasks.filter((task) => task.status === "FAILED");
  const workerCapacity = dashboard.workers.reduce((sum, worker) => sum + (worker.capacity || 0), 0);
  const workerRunning = dashboard.workers.reduce((sum, worker) => sum + (worker.running_tasks || 0), 0);
  // When the backend does not provide a high watermark, use the current depth as a non-zero denominator.
  const queuePressure = percent(dashboard.queue_depth, dashboard.queue_high_watermark || Math.max(1, dashboard.queue_depth));
  // Empty capacity reports should not produce NaN while workers are still starting.
  const workerUtilization = percent(workerRunning, workerCapacity || Math.max(1, workerRunning));
  const materializer = dashboard.metadata_materializer;
  const materializerLag = materializer?.eventual_lag_estimate_ms ?? 0;
  const workerHealth = dashboard.workers.length ? `${dashboard.workers.length} active` : "No workers";

  return (
    <div className="stack">
      {error && <ErrorPanel message={error} />}
      <section className="health-strip" aria-label="System health">
        <HealthCard label="Connection" value={streamState.connected ? "Connected" : "Reconnecting"} tone={streamState.connected ? "good" : "warn"} />
        <HealthCard label="Authenticated" value={session?.role ?? "unknown"} tone={session ? "good" : "warn"} />
        <HealthCard label="Last refresh" value={lastRefreshAt ? new Date(lastRefreshAt).toLocaleTimeString() : "pending"} tone={lastRefreshAt ? "good" : "warn"} />
        <HealthCard label="Queue pressure" value={`${Math.round(queuePressure)}%`} tone={queuePressure >= 100 ? "bad" : queuePressure >= 70 ? "warn" : "good"} />
        <HealthCard label="Worker health" value={workerHealth} tone={dashboard.workers.length ? "good" : "bad"} />
      </section>

      <div className="kpi-grid">
        <Kpi label="Queue Depth" value={dashboard.queue_depth} tone={dashboard.queue_depth >= dashboard.queue_high_watermark && dashboard.queue_high_watermark > 0 ? "bad" : "neutral"} />
        <Kpi label="Running Tasks" value={running} tone="info" />
        <Kpi label="Queued Tasks" value={queued} />
        <Kpi label="Succeeded" value={succeeded} tone="good" />
        <Kpi label="Failed Tasks" value={failedTasks.length} tone={failedTasks.length > 0 ? "bad" : "neutral"} />
        <Kpi label="Active Workers" value={dashboard.workers.length} tone={dashboard.workers.length ? "good" : "bad"} />
        <Kpi label="Worker Capacity" value={workerCapacity} />
        <Kpi label="Models" value={dashboard.models.length} />
        <Kpi label="Scheduling" value={dashboard.scheduling_policy} />
        <Kpi label="Log Append" value={`${dashboard.last_log_append_ms || 0} ms`} tone={dashboard.log_append_slow_ms > 0 && dashboard.last_log_append_ms >= dashboard.log_append_slow_ms ? "bad" : "neutral"} />
        <Kpi label="Materializer Lag" value={`${materializerLag} ms`} tone={materializerLag > 1000 ? "warn" : "neutral"} />
        <Kpi label="Compactable Bytes" value={dashboard.compactable_log_bytes} />
      </div>

      <section className="progress-grid" aria-label="Runtime diagnostics">
        <ProgressCard label="Queue pressure" value={queuePressure} detail={`${dashboard.queue_depth} / ${dashboard.queue_high_watermark || 0}`} />
        <ProgressCard label="Worker capacity utilization" value={workerUtilization} detail={`${workerRunning} / ${workerCapacity}`} />
        <ProgressCard label="Metadata materializer lag" value={Math.min(100, percent(materializerLag, 1000))} detail={`${materializerLag} ms`} />
      </section>

      <section className="panel">
        <PanelTitle title="Recent Failures" action={<a data-nav className="button ghost" href="/tasks?status=FAILED">Open</a>} />
        {failedTasks.length ? <TaskTable rows={[...failedTasks].reverse().slice(0, 6)} /> : <div className="empty empty-state">No recent failures. Failed tasks will appear here with links to their detail pages.</div>}
      </section>
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
          <PanelTitle title="Workers" action={roleAtLeast(session, "operator") ? <a data-nav className="button ghost" href="/workers">Open</a> : undefined} />
          <WorkerTable rows={dashboard.workers.slice(0, 6)} />
        </div>
      </section>
    </div>
  );
}

// Render control-plane health, session role, and request identity metadata.
function HealthCard({ label, value, tone }: { label: string; value: string; tone: "good" | "warn" | "bad" }) {
  return (
    <div className="health-card">
      <span>{label}</span>
      <strong><span className={`status-dot ${tone}`} /> {value}</strong>
    </div>
  );
}

// Render aggregate workflow/task counters with computed completion progress.
function ProgressCard({ label, value, detail }: { label: string; value: number; detail: string }) {
  const clamped = Math.max(0, Math.min(100, value));
  const tone = clamped >= 100 ? "bad" : clamped >= 70 ? "warn" : "";
  return (
    <div className="progress-card">
      <div className="progress-label"><span>{label}</span><strong>{detail}</strong></div>
      <div className="progress-track" aria-hidden="true"><div className={`progress-fill ${tone}`} style={{ width: `${clamped}%` }} /></div>
    </div>
  );
}

// Compute a percentage while treating non-positive denominators as empty.
function percent(value: number, total: number): number {
  if (total <= 0) return 0;
  return (value / total) * 100;
}