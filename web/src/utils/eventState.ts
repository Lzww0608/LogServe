// State merge helpers for applying SSE deltas without dropping existing metadata.

import type { LogRecord, LogStreamDetail, StreamStats, Task, Workflow, WorkflowStep } from "../types/logserve.js";

// LogRecordsEvent is the compact SSE payload for appending records to one log stream.
export type LogRecordsEvent = {
  stream_id: string;
  records: LogRecord[];
  next_seq: number;
  stats?: StreamStats | null;
};

// Merge a task SSE delta into the current row without losing existing metadata.
export function applyTaskEvent(current: Task | undefined, incoming: Partial<Task> & Pick<Task, "task_id">): Task {
  // A missing current row can be created from the event id so list/detail views converge.
  const base: Task = current ?? { task_id: incoming.task_id, status: incoming.status ?? "UNSPECIFIED" };
  return mergeDefined(base, incoming);
}

// Merge a workflow SSE delta and reconcile step updates by step id.
export function applyWorkflowEvent(current: Workflow | undefined, incoming: Partial<Workflow> & Pick<Workflow, "workflow_id">): Workflow {
  const base: Workflow = current ?? { workflow_id: incoming.workflow_id, status: incoming.status ?? "UNSPECIFIED" };
  const { steps, ...rest } = incoming;
  const next = mergeDefined(base, rest);
  // Workflow events without steps still update aggregate fields like status or result_json.
  if (!steps) return next;
  next.steps = mergeSteps(current?.steps ?? [], steps);
  return next;
}

// Append log-record SSE deltas by sequence while bounding retained rows.
export function applyLogRecordsEvent(current: LogStreamDetail | undefined, incoming: LogRecordsEvent, maxRecords = 500): LogStreamDetail {
  const sameStream = current?.stream_id === incoming.stream_id;
  // Sequence is the stable log identity, so repeated SSE deliveries cannot duplicate rows.
  const recordsBySeq = new Map<number, LogRecord>();
  if (sameStream) {
    for (const record of current.records) recordsBySeq.set(record.seq, record);
  }
  for (const record of incoming.records) {
    if (!recordsBySeq.has(record.seq)) recordsBySeq.set(record.seq, record);
  }
  // Sorting after merge preserves log order even when SSE batches arrive with overlapping records.
  const records = [...recordsBySeq.values()].sort((left, right) => left.seq - right.seq).slice(-maxRecords);
  return {
    stream_id: incoming.stream_id,
    from_seq: incoming.next_seq,
    limit: current?.limit ?? 100,
    records,
    // Undefined stats means "no update"; null is a real reset from the backend or stream switch.
    stats: incoming.stats !== undefined ? incoming.stats : sameStream ? current?.stats : null
  };
}

// Preserve existing workflow step order while applying incoming step deltas.
function mergeSteps(current: WorkflowStep[], incoming: WorkflowStep[]): WorkflowStep[] {
  const byID = new Map<string, WorkflowStep>();
  const order: string[] = [];
  for (const step of current) {
    byID.set(step.step_id, step);
    order.push(step.step_id);
  }
  for (const step of incoming) {
    const previous = byID.get(step.step_id);
    // Sparse step events update only fields they carry, preserving earlier task ids/results.
    byID.set(step.step_id, previous ? mergeDefined(previous, step) : step);
    if (!order.includes(step.step_id)) order.push(step.step_id);
  }
  // The type guard drops impossible missing map entries while preserving the stable step order type.
  return order.map((stepID) => byID.get(stepID)).filter((step): step is WorkflowStep => Boolean(step));
}

// Overlay only meaningful fields so sparse event deltas do not erase known values.
function mergeDefined<T extends object>(current: T, incoming: Partial<T>): T {
  const output: Record<string, unknown> = { ...(current as Record<string, unknown>) };
  for (const [key, value] of Object.entries(incoming)) {
    // Empty strings are treated like omitted fields because backend deltas often leave optional text unset.
    if (value === undefined || value === "") continue;
    output[key] = value;
  }
  return output as T;
}
