import type { Actor, AdminConfig, BackpressureConfig, Dashboard, FunctionRegistryEntry, LLMTrace, LogStreamDetail, LogStreamsResponse, ModelInfo, Task, TaskListResponse, Worker, Workflow, WorkflowListResponse } from "../types/logserve";

export class APIError extends Error {
  code: string;
  status: number;

  constructor(status: number, code: string, message: string) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

type FetchInit = RequestInit & { json?: unknown };

type QueryValue = string | number | undefined;

export type TaskListQuery = {
  q?: string;
  status?: string;
  workerID?: string;
  workflowID?: string;
  limit?: number;
  pageToken?: string;
};

export type WorkflowListQuery = {
  status?: string;
  limit?: number;
  pageToken?: string;
};

const tokenKey = "logserve.console.token";

export function getStoredToken(): string {
  return sessionStorage.getItem(tokenKey) ?? "";
}

export function setStoredToken(token: string): void {
  const trimmed = token.trim();
  if (trimmed) {
    sessionStorage.setItem(tokenKey, trimmed);
  } else {
    sessionStorage.removeItem(tokenKey);
  }
}

async function apiFetch<T>(path: string, init: FetchInit = {}): Promise<T> {
  const headers = new Headers(init.headers);
  const token = getStoredToken();
  if (token) {
    headers.set("Authorization", `Bearer ${token}`);
  }
  let body = init.body;
  if (init.json !== undefined) {
    headers.set("Content-Type", "application/json");
    body = JSON.stringify(init.json);
  }
  const response = await fetch(path, { ...init, headers, body });
  const text = await response.text();
  let payload: unknown;
  try {
    payload = text ? JSON.parse(text) : undefined;
  } catch {
    payload = undefined;
  }
  if (!response.ok) {
    const error = isAPIErrorPayload(payload) ? payload.error : undefined;
    throw new APIError(response.status, error?.code ?? "HTTP_ERROR", error?.message ?? response.statusText);
  }
  return payload as T;
}

export type TaskOperation = "retry" | "cancel" | "resubmit";

export function taskActionURL(taskID: string, action: TaskOperation): string {
  return `/api/tasks/${encodeURIComponent(taskID)}/${action}`;
}

export function tasksURL(query: TaskListQuery | string = ""): string {
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

export function workflowsURL(query: WorkflowListQuery | string = ""): string {
  if (typeof query === "string") return `/api/workflows${query}`;
  return `/api/workflows${queryString({
    status: query.status,
    limit: query.limit,
    page_token: query.pageToken
  })}`;
}

export function logStreamURL(streamID: string, fromSeq = 1, limit = 100): string {
  return `/api/logs/streams/${encodeURIComponent(streamID)}${queryString({ from_seq: fromSeq, limit })}`;
}

function queryString(values: Record<string, QueryValue>): string {
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(values)) {
    if (value === undefined) continue;
    const text = String(value).trim();
    if (!text) continue;
    params.set(key, text);
  }
  const encoded = params.toString();
  return encoded ? `?${encoded}` : "";
}
function isAPIErrorPayload(value: unknown): value is { error: { code?: string; message?: string } } {
  return typeof value === "object" && value !== null && "error" in value;
}

export const api = {
  health: () => apiFetch<{ status: string }>("/api/healthz"),
  dashboard: () => apiFetch<Dashboard>("/api/dashboard"),
  tasks: (query: TaskListQuery | string = "") => apiFetch<TaskListResponse>(tasksURL(query)),
  task: (taskID: string) => apiFetch<Task>(`/api/tasks/${encodeURIComponent(taskID)}`),
  retryTask: (taskID: string) => apiFetch<Task>(taskActionURL(taskID, "retry"), { method: "POST" }),
  cancelTask: (taskID: string) => apiFetch<Task>(taskActionURL(taskID, "cancel"), { method: "POST" }),
  resubmitTask: (taskID: string) => apiFetch<Task>(taskActionURL(taskID, "resubmit"), { method: "POST" }),
  submitTask: (payload: unknown) => apiFetch<Task>("/api/tasks", { method: "POST", json: payload }),
  workflows: (query: WorkflowListQuery | string = "") => apiFetch<WorkflowListResponse>(workflowsURL(query)),
  workflow: (workflowID: string) => apiFetch<Workflow>(`/api/workflows/${encodeURIComponent(workflowID)}`),
  submitWorkflow: (payload: unknown) => apiFetch<Workflow>("/api/workflows", { method: "POST", json: payload }),
  validateWorkflow: (payload: unknown) =>
    apiFetch<{ valid: boolean; message?: string; normalized_definition?: unknown; warnings?: string[] }>("/api/workflows/validate", {
      method: "POST",
      json: payload
    }),
  replayWorkflow: (workflowID: string) => apiFetch<{ workflow: Workflow; consistent_with_metadata: boolean }>(`/api/workflows/${encodeURIComponent(workflowID)}/replay`, { method: "POST" }),
  actors: () => apiFetch<{ actors: Actor[] }>("/api/actors"),
  actor: (actorID: string) => apiFetch<Actor>(`/api/actors/${encodeURIComponent(actorID)}`),
  createActor: (payload: unknown) => apiFetch<Actor>("/api/actors", { method: "POST", json: payload }),
  callActor: (actorID: string, payload: unknown) => apiFetch<Actor>(`/api/actors/${encodeURIComponent(actorID)}/calls`, { method: "POST", json: payload }),
  replayActor: (actorID: string) => apiFetch<Actor>(`/api/actors/${encodeURIComponent(actorID)}/replay`, { method: "POST" }),
  models: () => apiFetch<{ models: ModelInfo[] }>("/api/models"),
  registerModel: (payload: unknown) => apiFetch<ModelInfo>("/api/models", { method: "POST", json: payload }),
  submitLLM: (payload: unknown) => apiFetch<LLMTrace>("/api/llm", { method: "POST", json: payload }),
  replayLLM: (taskID: string) => apiFetch<LLMTrace>(`/api/llm/${encodeURIComponent(taskID)}/replay`, { method: "POST" }),
  workers: () => apiFetch<{ workers: Worker[] }>("/api/workers"),
  functions: () => apiFetch<{ functions: FunctionRegistryEntry[] }>("/api/functions"),
  functionByHash: (functionHash: string) => apiFetch<FunctionRegistryEntry>(`/api/functions/${encodeURIComponent(functionHash)}`),
  logStreams: (prefix = "") => {
    const query = prefix ? `?prefix=${encodeURIComponent(prefix)}` : "";
    return apiFetch<LogStreamsResponse>(`/api/logs/streams${query}`);
  },
  logStream: (streamID: string, fromSeq = 1, limit = 100) => apiFetch<LogStreamDetail>(logStreamURL(streamID, fromSeq, limit)),
  adminConfig: () => apiFetch<AdminConfig>("/api/admin/config"),
  setSchedulingPolicy: (policy: string) => apiFetch<{ policy: string }>("/api/admin/scheduling-policy", { method: "POST", json: { policy } }),
  setBackpressure: (payload: BackpressureConfig) => apiFetch<BackpressureConfig>("/api/admin/backpressure", { method: "POST", json: payload })
};
