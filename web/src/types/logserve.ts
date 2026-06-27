export type TaskStatus = "QUEUED" | "RUNNING" | "SUCCEEDED" | "FAILED" | "UNSPECIFIED";
export type WorkflowStatus = "RUNNING" | "COMPLETED" | "FAILED" | "UNSPECIFIED";
export type ActorStatus = "ACTIVE" | "UNAVAILABLE" | "UNSPECIFIED";

export interface Task {
  task_id: string;
  task_name?: string;
  status: TaskStatus;
  worker_id?: string;
  workflow_id?: string;
  step_id?: string;
  actor_id?: string;
  llm_model_name?: string;
  llm_model_version?: string;
  created_at_ms?: number;
  updated_at_ms?: number;
  result_json?: unknown;
  error?: string;
}

export interface WorkflowStep {
  step_id: string;
  depends_on?: string[];
  task_name?: string;
  status: string;
  attempts?: number;
  task_id?: string;
  result_json?: unknown;
  result_ref?: string;
  error?: string;
  started_at_ms?: number;
  completed_at_ms?: number;
  latency_ms?: number;
}

export interface Workflow {
  workflow_id: string;
  workflow_name?: string;
  status: WorkflowStatus;
  steps?: WorkflowStep[];
  result_json?: unknown;
  result_ref?: string;
  error?: string;
  created_at_ms?: number;
  updated_at_ms?: number;
  completed_at_ms?: number;
  latency_ms?: number;
  step_count?: number;
  succeeded_steps?: number;
  failed_steps?: number;
  running_steps?: number;
}

export interface Actor {
  actor_id: string;
  call_id?: string;
  class_name?: string;
  status: ActorStatus | TaskStatus;
  owner_worker_id?: string;
  epoch?: number;
  command_count?: number;
  snapshot_ref?: string;
  snapshot_command_count?: number;
  state_json?: unknown;
  result_json?: unknown;
  error?: string;
  consistent_with_metadata?: boolean;
  full_replay_commands?: number;
  snapshot_replay_commands?: number;
}

export interface ModelInfo {
  name: string;
  version: string;
  size_bytes?: number;
  path?: string;
  adapter?: string;
}

export interface Worker {
  worker_id: string;
  capacity: number;
  running_tasks: number;
  cached_models?: ModelInfo[];
  last_heartbeat_ms?: number;
}

export interface Dashboard {
  queue_depth: number;
  queue_high_watermark: number;
  redelivery_timeout_ms: number;
  scheduling_policy: string;
  tasks: Task[];
  workflows: Workflow[];
  actors: Actor[];
  workers: Worker[];
  models: ModelInfo[];
  last_log_append_ms: number;
  log_append_slow_ms: number;
  compactable_log_records: number;
  compactable_log_bytes: number;
  metadata_materializer?: {
    mode?: string;
    pending_deltas?: number;
    queued_deltas?: number;
    flush_count?: number;
    flush_error_count?: number;
    eventual_lag_estimate_ms?: number;
    last_error?: string;
  };
}

export interface LLMTrace {
  task_id?: string;
  status?: TaskStatus;
  result_json?: unknown;
  error?: string;
  worker_id?: string;
  model_name?: string;
  model_version?: string;
  cache_hit?: boolean;
  model_load_ms?: number;
  checkpoint_fetch_ms?: number;
  first_token_ms?: number;
  total_latency_ms?: number;
  cache_used_bytes?: number;
  cache_capacity_bytes?: number;
  eviction_count?: number;
  events?: Array<Record<string, unknown>>;
}

export interface StreamStats {
  stream_id: string;
  first_seq: number;
  next_seq: number;
  trimmed_before_seq: number;
  compactable_records: number;
  compactable_bytes: number;
}

export interface LogRecord {
  stream_id: string;
  seq: number;
  event_type?: string;
  idempotency_key?: string;
  payload_json?: unknown;
  payload_text?: string;
  payload_base64?: string;
  timestamp_ms?: number;
  crc32?: number;
}

export interface LogStreamsResponse {
  stream_ids: string[];
  stats: StreamStats[];
}

export interface LogStreamDetail {
  stream_id: string;
  from_seq: number;
  limit: number;
  records: LogRecord[];
  stats?: StreamStats | null;
}
