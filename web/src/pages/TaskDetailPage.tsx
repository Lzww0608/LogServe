import { useCallback, useEffect, useState } from "react";
import { api, type TaskOperation } from "../api/client";
import { parseEventData, type SSEMessage } from "../api/events";
import { DetailGrid } from "../components/DetailGrid";
import { ErrorPanel, InlineError, Loading } from "../components/ErrorPanel";
import { JsonViewer } from "../components/JsonViewer";
import { PanelTitle } from "../components/PanelTitle";
import { StatusBadge } from "../components/StatusBadge";
import { useEventStream } from "../hooks/useEventStream";
import type { ConsoleSession, Task } from "../types/logserve";
import { applyTaskEvent } from "../utils/eventState";
import { formatTime, modelLabel } from "../utils/format";
import { navigate } from "../utils/navigation";
import { errorMessage } from "../utils/status";
import { taskActionState } from "../utils/taskActions";
import { roleAtLeast } from "../utils/roles";

export function TaskDetailPage({ taskID, session }: { taskID: string; session?: ConsoleSession | null }) {
  const [task, setTask] = useState<Task>();
  const [error, setError] = useState("");
  const [actionError, setActionError] = useState("");
  const [busyAction, setBusyAction] = useState<TaskOperation | "">("");
  useEffect(() => {
    setTask(undefined);
    setError("");
    setActionError("");
    setBusyAction("");
  }, [taskID]);
  const handleMessage = useCallback((message: SSEMessage) => {
    if (message.event !== "task") return;
    const payload = parseEventData<{ task: Task }>(message);
    setTask((current) => applyTaskEvent(current, payload.task));
    setError("");
  }, []);
  useEventStream({ taskID, intervalMs: 1000 }, { onMessage: handleMessage, onError: setError }, [taskID]);

  const runAction = async (action: TaskOperation) => {
    if (!task) return;
    const requiredRole = action === "cancel" ? "admin" : "operator";
    if (!roleAtLeast(session, requiredRole)) {
      setActionError(`${requiredRole.charAt(0).toUpperCase()}${requiredRole.slice(1)} role is required for ${action}.`);
      return;
    }
    const state = taskActionState(task, action);
    if (!state.enabled) {
      setActionError(state.reason ?? "Task action is unavailable.");
      return;
    }
    setBusyAction(action);
    setActionError("");
    try {
      const next = action === "retry"
        ? await api.retryTask(task.task_id)
        : action === "resubmit"
          ? await api.resubmitTask(task.task_id)
          : await api.cancelTask(task.task_id);
      setTask((current) => applyTaskEvent(current, next));
      if ((action === "retry" || action === "resubmit") && next.task_id) {
        navigate(`/tasks/${encodeURIComponent(next.task_id)}`);
      }
    } catch (error) {
      setActionError(errorMessage(error));
    } finally {
      setBusyAction("");
    }
  };

  if (error && !task) return <ErrorPanel message={error} />;
  if (!task) return <Loading />;
  return (
    <div className="stack">
      {error && <ErrorPanel message={error} />}
      <section className="panel">
        <PanelTitle title={task.task_id} action={<TaskActions task={task} busyAction={busyAction} session={session} onRun={runAction} />} />
        <DetailGrid items={[
          ["Worker", task.worker_id],
          ["Created", formatTime(task.created_at_ms)],
          ["Updated", formatTime(task.updated_at_ms)],
          ["Workflow", task.workflow_id],
          ["Actor", task.actor_id],
          ["Model", modelLabel(task)]
        ]} />
        {actionError && <InlineError message={actionError} />}
      </section>
      {task.error && <ErrorPanel message={task.error} />}
      <section className="panel">
        <h2>Result</h2>
        <JsonViewer value={task.result_json ?? null} />
      </section>
    </div>
  );
}

function TaskActions({ task, busyAction, session, onRun }: { task: Task; busyAction: TaskOperation | ""; session?: ConsoleSession | null; onRun: (action: TaskOperation) => void }) {
  return (
    <div className="task-header-actions">
      <StatusBadge value={task.status} />
      <div className="button-row task-action-row">
        {(["retry", "resubmit", "cancel"] as TaskOperation[]).map((action) => (
          <TaskActionButton key={action} task={task} action={action} busyAction={busyAction} session={session} onRun={onRun} />
        ))}
      </div>
    </div>
  );
}

function TaskActionButton({ task, action, busyAction, session, onRun }: { task: Task; action: TaskOperation; busyAction: TaskOperation | ""; session?: ConsoleSession | null; onRun: (action: TaskOperation) => void }) {
  const state = taskActionState(task, action);
  const busy = busyAction === action;
  const requiredRole = action === "cancel" ? "admin" : "operator";
  const roleBlocked = !roleAtLeast(session, requiredRole);
  const disabled = roleBlocked || !state.enabled || busyAction !== "";
  return (
    <span className="task-action-control" title={state.reason ?? undefined}>
      <button
        type="button"
        className={action === "retry" ? "primary" : "ghost"}
        disabled={disabled}
        aria-label={roleBlocked ? `${taskActionLabel(action)}: ${requiredRole} role required` : state.reason ? `${taskActionLabel(action)}: ${state.reason}` : taskActionLabel(action)}
        onClick={() => onRun(action)}
      >
        {busy ? "Working" : taskActionLabel(action)}
      </button>
    </span>
  );
}

function taskActionLabel(action: TaskOperation): string {
  switch (action) {
    case "retry":
      return "Retry";
    case "resubmit":
      return "Resubmit";
    case "cancel":
      return "Cancel";
  }
}
