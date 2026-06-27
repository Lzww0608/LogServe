import { api } from "../api/client";
import { DetailGrid } from "../components/DetailGrid";
import { ErrorPanel, Loading } from "../components/ErrorPanel";
import { JsonViewer } from "../components/JsonViewer";
import { PanelTitle } from "../components/PanelTitle";
import { StatusBadge } from "../components/StatusBadge";
import { usePolling } from "../hooks/usePolling";
import { formatTime, modelLabel } from "../utils/format";

export function TaskDetailPage({ taskID }: { taskID: string }) {
  const state = usePolling(() => api.task(taskID), 1000, [taskID]);
  if (state.error) return <ErrorPanel message={state.error} />;
  const task = state.data;
  if (!task) return <Loading />;
  return (
    <div className="stack">
      <section className="panel">
        <PanelTitle title={task.task_id} action={<StatusBadge value={task.status} />} />
        <DetailGrid items={[
          ["Worker", task.worker_id],
          ["Created", formatTime(task.created_at_ms)],
          ["Updated", formatTime(task.updated_at_ms)],
          ["Workflow", task.workflow_id],
          ["Actor", task.actor_id],
          ["Model", modelLabel(task)]
        ]} />
      </section>
      {task.error && <ErrorPanel message={task.error} />}
      <section className="panel">
        <h2>Result</h2>
        <JsonViewer value={task.result_json ?? null} />
      </section>
    </div>
  );
}
