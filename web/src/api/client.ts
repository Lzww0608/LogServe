// Shared HTTP client and URL builder layer for the LogServe console.

import type { Actor, AdminConfig, BackpressureConfig, ConsoleSession, Dashboard, FunctionRegistryEntry, LLMTrace, LogStreamDetail, LogStreamsResponse, ModelInfo, Task, TaskListResponse, TemplateInfo, TemplateListResponse, TemplateRunResponse, Worker, Workflow, WorkflowListResponse } from "../types/logserve";

// APIError is the console-wide typed error shape for failed backend calls.
export class APIError extends Error {
  code: string;
  status: number;

  // Preserve HTTP status and backend code while keeping Error.message usable in UI components.
  constructor(status: number, code: string, message: string) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

// FetchInit adds a json shortcut that apiFetch serializes after auth headers are prepared.
type FetchInit = RequestInit & { json?: unknown };

// QueryValue captures the filter types accepted by URLSearchParams builders.
type QueryValue = string | number | undefined;

// TaskListQuery mirrors backend task-list query parameters with UI-friendly field names.
export type TaskListQuery = {
  q?: string;
  status?: string;
  workerID?: string;
  workflowID?: string;
  limit?: number;
  pageToken?: string;
};

// WorkflowListQuery mirrors backend workflow-list query parameters with UI-friendly field names.
export type WorkflowListQuery = {
  status?: string;
  limit?: number;
  pageToken?: string;
};

// tokenKey is session-scoped so browser tabs can hold independent console tokens.
const tokenKey = "logserve.console.token";

// Read the browser session token used for console API authorization.
export function getStoredToken(): string {
  return sessionStorage.getItem(tokenKey) ?? "";
}

// Persist or clear the console token and notify session/SSE consumers to reconnect.
export function setStoredToken(token: string): void {
  // Trim before storage so accidental whitespace does not create an unusable bearer token.
  const trimmed = token.trim();
  if (trimmed) {
    sessionStorage.setItem(tokenKey, trimmed);
  } else {
    sessionStorage.removeItem(tokenKey);
  }
  if (typeof window !== "undefined") {
    // Hooks that own SSE connections listen for this event and reconnect with the new token.
    window.dispatchEvent(new Event("logserve:token-change"));
  }
}

// Send one JSON-aware API request with bearer auth, request-id propagation, and APIError normalization.
async function apiFetch<T>(path: string, init: FetchInit = {}): Promise<T> {
  // Clone caller headers first so apiFetch can add auth and request metadata without mutating init.
  const headers = new Headers(init.headers);
  const token = getStoredToken();
  if (token) {
    headers.set("Authorization", `Bearer ${token}`);
  }
  let body = init.body;
  // The json shortcut is mutually exclusive with a caller-supplied raw body in normal use.
  if (init.json !== undefined) {
    headers.set("Content-Type", "application/json");
    body = JSON.stringify(init.json);
  }
  // Request ids are generated in the browser so audit/log correlation survives proxy hops.
  if (!headers.has("X-Request-ID")) {
    headers.set("X-Request-ID", requestID());
  }
  const response = await fetch(path, { ...init, headers, body });
  // Read text first so empty success bodies and structured error bodies follow one decode path.
  const text = await response.text();
  let payload: unknown;
  try {
    payload = text ? JSON.parse(text) : undefined;
    // Non-JSON success bodies are treated as undefined; callers type endpoints that return JSON.
  } catch {
    payload = undefined;
  }
  if (!response.ok) {
    const error = isAPIErrorPayload(payload) ? payload.error : undefined;
    throw new APIError(response.status, error?.code ?? "HTTP_ERROR", error?.message ?? response.statusText);
  }
  return payload as T;
}

// TaskOperation is constrained to backend task operation route suffixes.
export type TaskOperation = "retry" | "cancel" | "resubmit";

// Build the encoded endpoint for a task operation such as retry or resubmit.
export function taskActionURL(taskID: string, action: TaskOperation): string {
  return `/api/tasks/${encodeURIComponent(taskID)}/${action}`;
}

// Build the task-list URL from either a raw query string or typed filters.
export function tasksURL(query: TaskListQuery | string = ""): string {
  // Raw query strings are accepted for call sites that already own encoding and leading ?.
  if (typeof query === "string") return `/api/tasks${query}`;
  return `/api/tasks${queryString({
    q: query.q,
    status: query.status,
    worker_id: query.workerID,
    workflow_id: query.workflowID,
    limit: query.limit,
    page_token: query.pageToken
  })}`;
}

// Build the workflow-list URL from either a raw query string or typed filters.
export function workflowsURL(query: WorkflowListQuery | string = ""): string {
  if (typeof query === "string") return `/api/workflows${query}`;
  return `/api/workflows${queryString({
    status: query.status,
    limit: query.limit,
    page_token: query.pageToken
  })}`;
}

// Build the encoded log stream page URL with sequence and limit bounds.
export function logStreamURL(streamID: string, fromSeq = 1, limit = 100): string {
  return `/api/logs/streams/${encodeURIComponent(streamID)}${queryString({ from_seq: fromSeq, limit })}`;
}

// Build the template execution URL, preserving the optional wait flag.
export function templateRunURL(templateID: string, wait = false): string {
  return `/api/templates/${encodeURIComponent(templateID)}/run${wait ? "?wait=1" : ""}`;
}

// Create a request id for audit correlation, falling back when crypto.randomUUID is unavailable.
export function requestID(): string {
  // Bind randomUUID because some browser implementations require the crypto receiver.
  const randomUUID = globalThis.crypto?.randomUUID?.bind(globalThis.crypto);
  if (randomUUID) return randomUUID();
  return `ui-${Date.now()}-${Math.random().toString(16).slice(2)}`;
}

// Build a compact query string while omitting empty filters so list URLs stay stable.
function queryString(values: Record<string, QueryValue>): string {
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(values)) {
    // Undefined means omit the filter; empty strings are also skipped after trimming.
    if (value === undefined) continue;
    const text = String(value).trim();
    if (!text) continue;
    params.set(key, text);
  }
  const encoded = params.toString();
  return encoded ? `?${encoded}` : "";
}
// Recognize the backend error envelope before throwing a typed APIError.
function isAPIErrorPayload(value: unknown): value is { error: { code?: string; message?: string } } {
  // The guard is intentionally loose: apiFetch still tolerates missing code/message fields.
  return typeof value === "object" && value !== null && "error" in value;
}

// api is the stable frontend facade; pages should use these helpers rather than hard-coding paths.
export const api = {
  // Call the health-check endpoint used by smoke and proxy checks.
  health: () => apiFetch<{ status: string }>("/api/healthz"),
  // Call the session endpoint to resolve the current browser token role.
  session: () => apiFetch<ConsoleSession>("/api/session"),
  // Call the dashboard snapshot endpoint.
  dashboard: () => apiFetch<Dashboard>("/api/dashboard"),
  // Call the paginated task-list endpoint.
  tasks: (query: TaskListQuery | string = "") => apiFetch<TaskListResponse>(tasksURL(query)),
  // Call the task detail endpoint by encoded task id.
  task: (taskID: string) => apiFetch<Task>(`/api/tasks/${encodeURIComponent(taskID)}`),
  // Request backend retry for a standalone failed task.
  retryTask: (taskID: string) => apiFetch<Task>(taskActionURL(taskID, "retry"), { method: "POST" }),
  // Request backend task cancellation when the server supports it.
  cancelTask: (taskID: string) => apiFetch<Task>(taskActionURL(taskID, "cancel"), { method: "POST" }),
  // Request a fresh submission from a standalone task record.
  resubmitTask: (taskID: string) => apiFetch<Task>(taskActionURL(taskID, "resubmit"), { method: "POST" }),
  // Submit a Python task payload to the control plane.
  submitTask: (payload: unknown) => apiFetch<Task>("/api/tasks", { method: "POST", json: payload }),
  // Call the paginated workflow-list endpoint.
  workflows: (query: WorkflowListQuery | string = "") => apiFetch<WorkflowListResponse>(workflowsURL(query)),
  // Call the workflow detail endpoint by encoded workflow id.
  workflow: (workflowID: string) => apiFetch<Workflow>(`/api/workflows/${encodeURIComponent(workflowID)}`),
  // Submit a workflow definition payload to the control plane.
  submitWorkflow: (payload: unknown) => apiFetch<Workflow>("/api/workflows", { method: "POST", json: payload }),
  // Ask the backend to normalize and validate a workflow definition.
  validateWorkflow: (payload: unknown) =>
    apiFetch<{ valid: boolean; message?: string; normalized_definition?: unknown; warnings?: string[] }>("/api/workflows/validate", {
      method: "POST",
      json: payload
    }),
  // Request workflow replay and metadata consistency from the backend.
  replayWorkflow: (workflowID: string) => apiFetch<{ workflow: Workflow; consistent_with_metadata: boolean }>(`/api/workflows/${encodeURIComponent(workflowID)}/replay`, { method: "POST" }),
  // Call the actor list endpoint.
  actors: () => apiFetch<{ actors: Actor[] }>("/api/actors"),
  // Call the actor detail endpoint by encoded actor id.
  actor: (actorID: string) => apiFetch<Actor>(`/api/actors/${encodeURIComponent(actorID)}`),
  // Create an actor instance from class source and init args.
  createActor: (payload: unknown) => apiFetch<Actor>("/api/actors", { method: "POST", json: payload }),
  // Submit one actor method call against the encoded actor id.
  callActor: (actorID: string, payload: unknown) => apiFetch<Actor>(`/api/actors/${encodeURIComponent(actorID)}/calls`, { method: "POST", json: payload }),
  // Request actor replay from persisted log state.
  replayActor: (actorID: string) => apiFetch<Actor>(`/api/actors/${encodeURIComponent(actorID)}/replay`, { method: "POST" }),
  // Call the model registry endpoint.
  models: () => apiFetch<{ models: ModelInfo[] }>("/api/models"),
  // Register an LLM model descriptor through the admin path.
  registerModel: (payload: unknown) => apiFetch<ModelInfo>("/api/models", { method: "POST", json: payload }),
  // Submit an LLM request payload.
  submitLLM: (payload: unknown) => apiFetch<LLMTrace>("/api/llm", { method: "POST", json: payload }),
  // Request replay details for an LLM task id.
  replayLLM: (taskID: string) => apiFetch<LLMTrace>(`/api/llm/${encodeURIComponent(taskID)}/replay`, { method: "POST" }),
  // Call the worker list endpoint.
  workers: () => apiFetch<{ workers: Worker[] }>("/api/workers"),
  // Call the function registry list endpoint.
  functions: () => apiFetch<{ functions: FunctionRegistryEntry[] }>("/api/functions"),
  // Fetch one registered function by source hash.
  functionByHash: (functionHash: string) => apiFetch<FunctionRegistryEntry>(`/api/functions/${encodeURIComponent(functionHash)}`),
  // List log streams, optionally filtered by stream id prefix.
  logStreams: (prefix = "") => {
    // Prefix is the only log-stream filter here; detailed pagination belongs to logStreamURL.
    const query = prefix ? `?prefix=${encodeURIComponent(prefix)}` : "";
    return apiFetch<LogStreamsResponse>(`/api/logs/streams${query}`);
  },
  // Fetch one page of records from a log stream.
  logStream: (streamID: string, fromSeq = 1, limit = 100) => apiFetch<LogStreamDetail>(logStreamURL(streamID, fromSeq, limit)),
  // List built-in console templates.
  templates: () => apiFetch<TemplateListResponse>("/api/templates"),
  // Fetch one template definition by id.
  template: (templateID: string) => apiFetch<TemplateInfo>(`/api/templates/${encodeURIComponent(templateID)}`),
  // Run a built-in template with an idempotency payload.
  runTemplate: (templateID: string, payload: unknown = {}, wait = false) => apiFetch<TemplateRunResponse>(templateRunURL(templateID, wait), { method: "POST", json: payload }),
  // Fetch scheduling, backpressure, and materializer admin state.
  adminConfig: () => apiFetch<AdminConfig>("/api/admin/config"),
  // Update the control-plane scheduling policy.
  setSchedulingPolicy: (policy: string) => apiFetch<{ policy: string }>("/api/admin/scheduling-policy", { method: "POST", json: { policy } }),
  // Update backend backpressure thresholds from admin input.
  setBackpressure: (payload: BackpressureConfig) => apiFetch<BackpressureConfig>("/api/admin/backpressure", { method: "POST", json: payload })
};
