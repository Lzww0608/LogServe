import type { Actor, AdminConfig, BackpressureConfig, Dashboard, FunctionRegistryEntry, LLMTrace, LogStreamDetail, LogStreamsResponse, ModelInfo, Task, Worker, Workflow } from "../types/logserve";

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

function isAPIErrorPayload(value: unknown): value is { error: { code?: string; message?: string } } {
  return typeof value === "object" && value !== null && "error" in value;
}

export const api = {
  health: () => apiFetch<{ status: string }>("/api/healthz"),
  dashboard: () => apiFetch<Dashboard>("/api/dashboard"),
  tasks: (query = "") => apiFetch<{ tasks: Task[] }>(`/api/tasks${query}`),
  task: (taskID: string) => apiFetch<Task>(`/api/tasks/${encodeURIComponent(taskID)}`),
  submitTask: (payload: unknown) => apiFetch<Task>("/api/tasks", { method: "POST", json: payload }),
  workflows: () => apiFetch<{ workflows: Workflow[] }>("/api/workflows"),
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
  logStream: (streamID: string, fromSeq = 1, limit = 100) =>
    apiFetch<LogStreamDetail>(`/api/logs/streams/${encodeURIComponent(streamID)}?from_seq=${fromSeq}&limit=${limit}`),
  adminConfig: () => apiFetch<AdminConfig>("/api/admin/config"),
  setSchedulingPolicy: (policy: string) => apiFetch<{ policy: string }>("/api/admin/scheduling-policy", { method: "POST", json: { policy } }),
  setBackpressure: (payload: BackpressureConfig) => apiFetch<BackpressureConfig>("/api/admin/backpressure", { method: "POST", json: payload })
};
