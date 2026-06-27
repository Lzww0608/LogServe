import type { Actor, FunctionRegistryEntry, LogRecord, ModelInfo, StreamStats, Task, Worker, Workflow } from "../types/logserve";
import { formatTime, modelLabel, payloadPreview, submitTaskURLForFunction } from "../utils/format";
import { DetailGrid } from "./DetailGrid";
import { StatusBadge } from "./StatusBadge";
import { Table } from "./Table";

export function FunctionTable({ rows, onCopy }: { rows: FunctionRegistryEntry[]; onCopy: (functionHash: string) => void }) {
  return <Table rows={rows} empty="No functions" columns={[
    { label: "Function hash", className: "hash-cell", render: (row) => <code className="payload-cell">{row.function_hash}</code> },
    { label: "Source ref", className: "hash-cell", render: (row) => row.source_ref ? <code className="payload-cell">{row.source_ref}</code> : "-" },
    { label: "Entrypoint", render: (row) => row.entrypoint || "-" },
    { label: "Language", render: (row) => row.language || "-" },
    { label: "Timestamp", render: (row) => formatTime(row.timestamp_ms) },
    { label: "Actions", className: "actions-cell", render: (row) => <div className="button-row table-actions">
      <button type="button" className="ghost compact-button" onClick={() => onCopy(row.function_hash)}>Copy hash</button>
      <a data-nav className="button primary compact-button" href={submitTaskURLForFunction(row)}>Use in Submit Task</a>
    </div> }
  ]} />;
}

export function TaskTable({ rows }: { rows: Task[] }) {
  return <Table rows={rows} empty="No tasks" columns={[
    { label: "Task", render: (row) => <a data-nav href={`/tasks/${row.task_id}`}>{row.task_id}</a> },
    { label: "Name", render: (row) => row.task_name || "-" },
    { label: "Status", render: (row) => <StatusBadge value={row.status} /> },
    { label: "Worker", render: (row) => row.worker_id || "-" },
    { label: "Workflow", render: (row) => row.workflow_id ? <a data-nav href={`/workflows/${row.workflow_id}`}>{row.workflow_id}</a> : "-" },
    { label: "Actor", render: (row) => row.actor_id ? <a data-nav href={`/actors/${row.actor_id}`}>{row.actor_id}</a> : "-" },
    { label: "Model", render: modelLabel }
  ]} />;
}

export function WorkflowTable({ rows }: { rows: Workflow[] }) {
  return <Table rows={rows} empty="No workflows" columns={[
    { label: "Workflow", render: (row) => <a data-nav href={`/workflows/${row.workflow_id}`}>{row.workflow_name || row.workflow_id}</a> },
    { label: "Status", render: (row) => <StatusBadge value={row.status} /> },
    { label: "Steps", render: (row) => row.step_count ?? row.steps?.length ?? 0 },
    { label: "Succeeded", render: (row) => row.succeeded_steps ?? 0 },
    { label: "Failed", render: (row) => row.failed_steps ?? 0 },
    { label: "Running", render: (row) => row.running_steps ?? 0 }
  ]} />;
}

export function StepTable({ rows }: { rows: NonNullable<Workflow["steps"]> }) {
  return <Table rows={rows} empty="No steps" columns={[
    { label: "Step", render: (row) => row.step_id },
    { label: "Task", render: (row) => row.task_id ? <a data-nav href={`/tasks/${row.task_id}`}>{row.task_id}</a> : "-" },
    { label: "Status", render: (row) => <StatusBadge value={row.status} /> },
    { label: "Attempts", render: (row) => row.attempts ?? 0 },
    { label: "Latency", render: (row) => row.latency_ms ? `${row.latency_ms} ms` : "-" },
    { label: "Error", render: (row) => row.error || "-" }
  ]} />;
}

export function ActorTable({ rows }: { rows: Actor[] }) {
  return <Table rows={rows} empty="No actors" columns={[
    { label: "Actor", render: (row) => <a data-nav href={`/actors/${row.actor_id}`}>{row.actor_id}</a> },
    { label: "Class", render: (row) => row.class_name || "-" },
    { label: "Status", render: (row) => <StatusBadge value={row.status} /> },
    { label: "Owner", render: (row) => row.owner_worker_id || "-" },
    { label: "Epoch", render: (row) => row.epoch ?? 0 },
    { label: "Commands", render: (row) => row.command_count ?? 0 }
  ]} />;
}

export function ModelTable({ rows }: { rows: ModelInfo[] }) {
  return <Table rows={rows} empty="No models" columns={[
    { label: "Model", render: (row) => `${row.name}:${row.version || "v1"}` },
    { label: "Adapter", render: (row) => row.adapter || "mock" },
    { label: "Size", render: (row) => row.size_bytes ?? "-" },
    { label: "Path", render: (row) => row.path || "-" }
  ]} />;
}

export function WorkerTable({ rows }: { rows: Worker[] }) {
  return <Table rows={rows} empty="No workers" columns={[
    { label: "Worker", render: (row) => row.worker_id },
    { label: "Capacity", render: (row) => row.capacity },
    { label: "Running", render: (row) => row.running_tasks },
    { label: "Cached Models", render: (row) => row.cached_models?.map((model) => `${model.name}:${model.version || "v1"}`).join(", ") || "-" },
    { label: "Heartbeat", render: (row) => formatTime(row.last_heartbeat_ms) }
  ]} />;
}

export function LogStreamTable({ streamIDs, stats, selected, onSelect }: { streamIDs: string[]; stats: Map<string, StreamStats>; selected: string; onSelect: (streamID: string) => void }) {
  return <Table rows={streamIDs} empty="No streams" columns={[
    { label: "Stream", render: (streamID) => <button type="button" className={streamID === selected ? "primary compact-button" : "ghost compact-button"} onClick={() => onSelect(streamID)}>{streamID}</button> },
    { label: "First", render: (streamID) => stats.get(streamID)?.first_seq ?? "-" },
    { label: "Next", render: (streamID) => stats.get(streamID)?.next_seq ?? "-" },
    { label: "Trimmed", render: (streamID) => stats.get(streamID)?.trimmed_before_seq ?? "-" },
    { label: "Compactable", render: (streamID) => stats.get(streamID)?.compactable_records ?? 0 }
  ]} />;
}

export function LogRecordTable({ rows }: { rows: LogRecord[] }) {
  return <Table rows={rows} empty="No records" columns={[
    { label: "Seq", render: (row) => row.seq },
    { label: "Event", render: (row) => row.event_type || "-" },
    { label: "Idempotency", render: (row) => row.idempotency_key || "-" },
    { label: "Timestamp", render: (row) => formatTime(row.timestamp_ms) },
    { label: "CRC32", render: (row) => row.crc32 ?? "-" },
    { label: "Payload", render: (row) => <code className="payload-cell">{payloadPreview(row)}</code> }
  ]} />;
}

export function StreamStatsPanel({ stats }: { stats?: StreamStats | null }) {
  if (!stats) return <div className="empty">No stream stats</div>;
  return <DetailGrid items={[
    ["First seq", stats.first_seq],
    ["Next seq", stats.next_seq],
    ["Trimmed before", stats.trimmed_before_seq],
    ["Compactable records", stats.compactable_records],
    ["Compactable bytes", stats.compactable_bytes]
  ]} />;
}
