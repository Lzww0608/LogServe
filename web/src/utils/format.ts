// Formatting and preview helpers shared across LogServe console tables and forms.

import type { FunctionRegistryEntry, LogRecord, Task } from "../types/logserve";

// Format millisecond timestamps for table display, using dash for missing values.
export function formatTime(value?: number) {
  // Zero is not a meaningful persisted timestamp in this UI, so treat it as missing.
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
  // Keep invalid in-progress JSON visible as raw text instead of hiding form input.
  } catch {
    return value;
  }
}

// Choose the log payload display form in JSON, text, base64 priority order.
export function payloadPreview(record: LogRecord) {
  // payload_json is already decoded by the API layer; stringify it for compact table cells.
  if (record.payload_json !== undefined) return JSON.stringify(record.payload_json);
  // payload_text is already decoded server-side and should not be JSON-stringified again.
  if (record.payload_text !== undefined) return record.payload_text;
  if (record.payload_base64 !== undefined) return `base64:${record.payload_base64}`;
  return "-";
}

// Build a submit-task deep link from a function registry entry.
export function submitTaskURLForFunction(functionEntry: FunctionRegistryEntry) {
  const params = new URLSearchParams();
  // Function hash is the durable identity used by submit pages to avoid resending source.
  params.set("function_hash", functionEntry.function_hash);
  const functionName = functionNameFromEntrypoint(functionEntry.entrypoint);
  if (functionName) params.set("function_name", functionName);
  return `/submit/task?${params.toString()}`;
}

// Extract the callable name from module:function style entrypoints.
export function functionNameFromEntrypoint(entrypoint?: string) {
  const trimmed = (entrypoint ?? "").trim();
  if (!trimmed) return "";
  // Use the last colon so module paths containing colons still expose the final callable segment.
  const colon = trimmed.lastIndexOf(":");
  // Plain function names are accepted for entries that are not module-qualified.
  if (colon < 0) return trimmed;
  return trimmed.slice(colon + 1).trim();
}
