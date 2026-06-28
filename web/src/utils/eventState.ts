import type { LogRecord, LogStreamDetail, StreamStats, Task, Workflow, WorkflowStep } from "../types/logserve.js";

export type LogRecordsEvent = {
  stream_id: string;
  records: LogRecord[];
  next_seq: number;
  stats?: StreamStats | null;
};

export function applyTaskEvent(current: Task | undefined, incoming: Partial<Task> & Pick<Task, "task_id">): Task {
  const base: Task = current ?? { task_id: incoming.task_id, status: incoming.status ?? "UNSPECIFIED" };
  return mergeDefined(base, incoming);
}

export function applyWorkflowEvent(current: Workflow | undefined, incoming: Partial<Workflow> & Pick<Workflow, "workflow_id">): Workflow {
  const base: Workflow = current ?? { workflow_id: incoming.workflow_id, status: incoming.status ?? "UNSPECIFIED" };
  const { steps, ...rest } = incoming;
  const next = mergeDefined(base, rest);
  if (!steps) return next;
  next.steps = mergeSteps(current?.steps ?? [], steps);
  return next;
}

export function applyLogRecordsEvent(current: LogStreamDetail | undefined, incoming: LogRecordsEvent, maxRecords = 500): LogStreamDetail {
  const sameStream = current?.stream_id === incoming.stream_id;
  const recordsBySeq = new Map<number, LogRecord>();
  if (sameStream) {
    for (const record of current.records) recordsBySeq.set(record.seq, record);
  }
  for (const record of incoming.records) {
    if (!recordsBySeq.has(record.seq)) recordsBySeq.set(record.seq, record);
  }
  const records = [...recordsBySeq.values()].sort((left, right) => left.seq - right.seq).slice(-maxRecords);
  return {
    stream_id: incoming.stream_id,
    from_seq: incoming.next_seq,
    limit: current?.limit ?? 100,
    records,
    stats: incoming.stats !== undefined ? incoming.stats : sameStream ? current?.stats : null
  };
}

function mergeSteps(current: WorkflowStep[], incoming: WorkflowStep[]): WorkflowStep[] {
  const byID = new Map<string, WorkflowStep>();
  const order: string[] = [];
  for (const step of current) {
    byID.set(step.step_id, step);
    order.push(step.step_id);
  }
  for (const step of incoming) {
    const previous = byID.get(step.step_id);
    byID.set(step.step_id, previous ? mergeDefined(previous, step) : step);
    if (!order.includes(step.step_id)) order.push(step.step_id);
  }
  return order.map((stepID) => byID.get(stepID)).filter((step): step is WorkflowStep => Boolean(step));
}

function mergeDefined<T extends object>(current: T, incoming: Partial<T>): T {
  const output: Record<string, unknown> = { ...(current as Record<string, unknown>) };
  for (const [key, value] of Object.entries(incoming)) {
    if (value === undefined || value === "") continue;
    output[key] = value;
  }
  return output as T;
}
