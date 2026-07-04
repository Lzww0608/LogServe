// Formatting and preview helpers shared across LogServe console tables and forms.

import type { FunctionRegistryEntry, LogRecord, Task } from "../types/logserve";

// Format millisecond timestamps for table display, using dash for missing values.
export function formatTime(value?: number) {
  if (!value) return "-";
  return new Date(value).toLocaleString();
}

// Render an LLM model label while defaulting empty versions to v1.
export function modelLabel(task: Pick<Task, "llm_model_name" | "llm_model_version">) {
  if (!task.llm_model_name) return "-";
  return `${task.llm_model_name}:${task.llm_model_version || "v1"}`;
}

// Create a timestamp-based idempotency key prefix for console submissions.
export function defaultID(prefix: string) {
  return `${prefix}-${Date.now()}`;
}

// Best-effort parse JSON preview text without throwing during form edits.
export function safePreview(value: string) {
  try {
    return JSON.parse(value || "null");
  } catch {
    return value;
  }
}

// Choose the log payload display form in JSON, text, base64 priority order.
export function payloadPreview(record: LogRecord) {
  if (record.payload_json !== undefined) return JSON.stringify(record.payload_json);
  if (record.payload_text !== undefined) return record.payload_text;
  if (record.payload_base64 !== undefined) return `base64:${record.payload_base64}`;
  return "-";
}

// Build a submit-task deep link from a function registry entry.
export function submitTaskURLForFunction(functionEntry: FunctionRegistryEntry) {
  const params = new URLSearchParams();
  params.set("function_hash", functionEntry.function_hash);
  const functionName = functionNameFromEntrypoint(functionEntry.entrypoint);
  if (functionName) params.set("function_name", functionName);
  return `/submit/task?${params.toString()}`;
}

// Extract the callable name from module:function style entrypoints.
export function functionNameFromEntrypoint(entrypoint?: string) {
  const trimmed = (entrypoint ?? "").trim();
  if (!trimmed) return "";
  const colon = trimmed.lastIndexOf(":");
  if (colon < 0) return trimmed;
  return trimmed.slice(colon + 1).trim();
}
