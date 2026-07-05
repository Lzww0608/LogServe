// Browser-side DTO types that mirror the web API JSON contract.
// Keep these shapes structural: validation and narrowing live at call sites because many fields are optional by endpoint.

// ConsoleRole is the frontend role ladder returned by /session and used for route/action gating.
export type ConsoleRole = "viewer" | "operator" | "admin";

// ConsoleSession describes the current browser token's console permissions.
export interface ConsoleSession {
  // subject is the authenticated token identity shown in the console, not a user profile object.
  subject: string;
  role: ConsoleRole;
  // permissions contains backend-issued action strings; UI code treats unknown values as absent capabilities.
  permissions: string[];
}

// TemplateInfo is metadata for a runnable built-in scenario exposed by the web API.
export interface TemplateInfo {
  id: string;
  label: string;
  // kind intentionally accepts unknown strings so older frontends can list newer backend templates safely.
  kind: "task" | "workflow" | "actor" | "llm" | string;
  description: string;
  expected_result: string;
  // required_role drives frontend button state; the backend still enforces the same privilege boundary.
  required_role: ConsoleRole;
  // payload is template-kind-specific JSON and should be inspected by the caller before narrowing.
  payload?: unknown;
}

// TemplateListResponse wraps the template catalog endpoint response.
export interface TemplateListResponse {
  templates: TemplateInfo[];
}

// TemplateRunResponse carries both template metadata and the kind-specific execution result.
export interface TemplateRunResponse {
  template: TemplateInfo;
  result: unknown;
}

// TaskStatus mirrors task lifecycle values emitted by list, detail, and SSE endpoints.
// UNSPECIFIED represents an unknown/default backend value and should not be treated as success.
export type TaskStatus = "QUEUED" | "RUNNING" | "SUCCEEDED" | "FAILED" | "UNSPECIFIED";
// WorkflowStatus is the aggregate lifecycle state for workflow metadata.
export type WorkflowStatus = "RUNNING" | "COMPLETED" | "FAILED" | "UNSPECIFIED";
// ActorStatus is the registry-level actor availability state.
export type ActorStatus = "ACTIVE" | "UNAVAILABLE" | "UNSPECIFIED";

// Task is the shared task DTO for tables, detail pages, SSE deltas, and task operations.
export interface Task {
  task_id: string;
  task_name?: string;
  status: TaskStatus;
  // worker_id is absent until a queued task has been claimed or when historical data omits placement.
  worker_id?: string;
  // workflow_id/step_id, actor_id, and llm_model_* identify derived tasks created by higher-level subsystems.
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

// WorkflowStep describes one DAG node as returned in workflow metadata and SSE updates.
export interface WorkflowStep {
  step_id: string;
  depends_on?: string[];
  task_name?: string;
  // Step status is kept as string because step-level states are produced by workflow replay, not only TaskStatus.
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

// Workflow is the aggregate workflow DTO used by lists, detail views, replay, and dashboard snapshots.
export interface Workflow {
  workflow_id: string;
  workflow_name?: string;
  status: WorkflowStatus;
  steps?: WorkflowStep[];
  // result_json and result_ref are alternate result forms: inline JSON or an object-store reference.
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

// Actor covers registry entries, call results, and replay responses for actor state inspection.
export interface Actor {
  actor_id: string;
  call_id?: string;
  class_name?: string;
  // Calls may surface task-like statuses while registry rows use actor availability statuses.
  status: ActorStatus | TaskStatus;
  // epoch changes when ownership is reassigned, so UI code should display it as concurrency metadata only.
  owner_worker_id?: string;
  epoch?: number;
  command_count?: number;
  // Snapshot fields describe the last persisted actor checkpoint used to shorten replay.
  snapshot_ref?: string;
  snapshot_command_count?: number;
  state_json?: unknown;
  result_json?: unknown;
  error?: string;
  // Replay responses can compare reconstructed state against materialized metadata.
  consistent_with_metadata?: boolean;
  full_replay_commands?: number;
  snapshot_replay_commands?: number;
}

// ModelInfo is worker/model-registry metadata for LLM cache and placement views.
export interface ModelInfo {
  name: string;
  version: string;
  // size/path/adapter are registry diagnostics; cache decisions are made on the server.
  size_bytes?: number;
  path?: string;
  adapter?: string;
}

// Worker is the heartbeat/capacity DTO used by worker tables and dashboard health cards.
export interface Worker {
  worker_id: string;
  capacity: number;
  running_tasks: number;
  cached_models?: ModelInfo[];
  last_heartbeat_ms?: number;
}

// MetadataMaterializerStats reports the asynchronous log-to-metadata projection worker.
export interface MetadataMaterializerStats {
  mode?: string;
  // pending_deltas and queued_deltas describe projection backlog at different buffering layers.
  pending_deltas?: number;
  queued_deltas?: number;
  batch_max?: number;
  flush_interval_ms?: number;
  flush_count?: number;
  flush_error_count?: number;
  last_flush_at_ms?: number;
  last_success_at_ms?: number;
  last_error_at_ms?: number;
  last_flush_duration_ms?: number;
  last_flush_deltas?: number;
  last_error?: string;
  eventual_lag_estimate_ms?: number;
}

// FunctionRegistryEntry represents a registered function source identity.
export interface FunctionRegistryEntry {
  function_hash: string;
  source_ref: string;
  entrypoint: string;
  language: string;
  // timestamp_ms is backend wall-clock metadata and should not be used as a uniqueness key.
  timestamp_ms?: number;
}

// BackpressureConfig is the admin-editable control-plane pressure configuration.
export interface BackpressureConfig {
  // queue_high_watermark and timing thresholds are expressed in the same units the backend accepts.
  queue_high_watermark: number;
  redelivery_timeout_ms: number;
  log_append_slow_ms: number;
}

// AdminConfig combines mutable backpressure config with read-only scheduler/log diagnostics.
export interface AdminConfig extends BackpressureConfig {
  scheduling_policy: string;
  metadata_materializer?: MetadataMaterializerStats | null;
  compactable_log_records: number;
  compactable_log_bytes: number;
}

// Dashboard is the live console summary snapshot pushed through the dashboard SSE stream.
export interface Dashboard {
  // Dashboard streams are snapshots, not deltas; arrays replace the previous UI state each tick.
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
  metadata_materializer?: MetadataMaterializerStats | null;
}

// PaginatedPayload is the common opaque-token pagination envelope for list endpoints.
export interface PaginatedPayload {
  limit: number;
  total_count?: number;
  // next_page_token is opaque; callers must store and replay it instead of deriving offsets.
  next_page_token?: string;
}

// TaskListResponse is the paginated task-list payload.
export interface TaskListResponse extends PaginatedPayload {
  tasks: Task[];
}

// WorkflowListResponse is the paginated workflow-list payload.
export interface WorkflowListResponse extends PaginatedPayload {
  workflows: Workflow[];
}

// LLMTrace represents both submit acknowledgements and replay traces, so most fields are optional.
export interface LLMTrace {
  task_id?: string;
  status?: TaskStatus;
  // result_json is provider/adapter specific and remains opaque until rendered by the LLM trace view.
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
  // Cache counters are point-in-time diagnostics and may be absent from simple submit acknowledgements.
  cache_used_bytes?: number;
  cache_capacity_bytes?: number;
  eviction_count?: number;
  // events are flexible backend trace records; UI code narrows individual fields at render time.
  events?: Array<Record<string, unknown>>;
}

// StreamStats summarizes sequence bounds and compaction state for one log stream.
export interface StreamStats {
  stream_id: string;
  first_seq: number;
  // next_seq is the append position, so the highest existing record sequence is next_seq - 1 when not empty.
  next_seq: number;
  // Records before trimmed_before_seq are no longer available for detail pagination.
  trimmed_before_seq: number;
  compactable_records: number;
  compactable_bytes: number;
}

// LogRecord is one shared-log entry with backend-chosen payload representation variants.
export interface LogRecord {
  stream_id: string;
  seq: number;
  // event_type and idempotency_key may be omitted for legacy records or compacted display rows.
  event_type?: string;
  idempotency_key?: string;
  // Payloads may be decoded JSON, displayable text, or base64 bytes depending on event encoding.
  payload_json?: unknown;
  payload_text?: string;
  payload_base64?: string;
  timestamp_ms?: number;
  crc32?: number;
}

// LogStreamsResponse returns discovered stream ids plus any available per-stream stats.
export interface LogStreamsResponse {
  // stream_ids is the discovery list; stats can be empty when detailed per-stream stats were unavailable.
  stream_ids: string[];
  stats: StreamStats[];
}

// LogStreamDetail is a sequence-page response for one selected log stream.
export interface LogStreamDetail {
  stream_id: string;
  from_seq: number;
  limit: number;
  records: LogRecord[];
  stats?: StreamStats | null;
  // next_seq and has_more are hints for sequence pagination; callers still preserve their requested from_seq.
  next_seq?: number;
  has_more?: boolean;
}
