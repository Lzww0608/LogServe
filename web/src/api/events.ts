// SSE client helpers for dashboard, task, workflow, and log streaming.

import { APIError, getStoredToken, requestID } from "./client.js";

// EventStreamOptions are translated directly into /api/events query parameters.
export type EventStreamOptions = {
  taskID?: string;
  stream?: string;
  fromSeq?: number;
  limit?: number;
  intervalMs?: number;
  records?: boolean;
  enabled?: boolean;
};

// SSEMessage is the parsed frame shape consumed by hooks and event-state reducers.
export type SSEMessage = {
  event: string;
  data: string;
};

// Build the SSE subscription URL from supported dashboard/task/log filters.
export function createEventStreamURL(options: EventStreamOptions = {}): string {
  // enabled is intentionally not serialized; it is a React hook control flag, not a backend filter.
  const params = new URLSearchParams();
  if (options.taskID) params.set("task_id", options.taskID);
  if (options.stream) params.set("stream", options.stream);
  if (options.fromSeq !== undefined) params.set("from_seq", String(options.fromSeq));
  if (options.limit !== undefined) params.set("limit", String(options.limit));
  if (options.intervalMs !== undefined) params.set("interval_ms", String(options.intervalMs));
  // records is encoded as 1 to match the backend query parser used by the web API.
  if (options.records) params.set("records", "1");
  const query = params.toString();
  return query ? `/api/events?${query}` : "/api/events";
}

// Accumulate partial SSE chunks and emit only complete blank-line-delimited messages.
export function parseSSEChunk(buffer: string, chunk: string): { buffer: string; messages: SSEMessage[] } {
  // Normalize CRLF from proxies before searching for SSE blank-line boundaries.
  let pending = (buffer + chunk).replace(/\r\n/g, "\n");
  const messages: SSEMessage[] = [];
  // Keep the incomplete tail in buffer; SSE frames are complete only after a blank line.
  let boundary = pending.indexOf("\n\n");
  while (boundary >= 0) {
    const rawMessage = pending.slice(0, boundary);
    pending = pending.slice(boundary + 2);
    // Empty/comment-only frames are ignored by parseSSEMessage and never reach consumers.
    const message = parseSSEMessage(rawMessage);
    if (message) messages.push(message);
    boundary = pending.indexOf("\n\n");
  }
  return { buffer: pending, messages };
}

// Parse one SSE frame, preserving multi-line data fields and ignoring comments.
function parseSSEMessage(rawMessage: string): SSEMessage | null {
  let event = "message";
  const data: string[] = [];
  for (const line of rawMessage.split("\n")) {
    if (!line || line.startsWith(":")) continue;
    if (line.startsWith("event:")) {
      event = trimSSEValue(line.slice("event:".length));
      continue;
    }
    if (line.startsWith("data:")) {
      // Multiple data lines are joined with newlines per the SSE wire format.
      data.push(trimSSEValue(line.slice("data:".length)));
    }
  }
  // SSE comments or event-only frames are not useful to state reducers without data.
  if (!data.length) return null;
  return { event, data: data.join("\n") };
}

// Remove the optional single leading space allowed after SSE field colons.
function trimSSEValue(value: string): string {
  return value.startsWith(" ") ? value.slice(1) : value;
}

// Decode a typed JSON payload from one parsed SSE message.
export function parseEventData<T>(message: SSEMessage): T {
  // Let JSON.parse throw here; consumers treat malformed event payloads as stream failures.
  return JSON.parse(message.data) as T;
}

// Convert backend error events into the same APIError shape used by fetch calls.
function eventStreamError(message: SSEMessage): Error {
  try {
    // Backend handlers may emit either {message} or the normal {error:{message}} envelope.
    const payload = JSON.parse(message.data) as { message?: unknown; error?: { message?: unknown } };
    const detail = typeof payload.message === "string"
      ? payload.message
      : typeof payload.error?.message === "string"
        ? payload.error.message
        : "event stream reported an error";
    return new APIError(0, "EVENT_STREAM_ERROR", detail);
  } catch {
    return new APIError(0, "EVENT_STREAM_ERROR", message.data || "event stream reported an error");
  }
}

// Dispatch parsed SSE messages, stopping immediately on backend error events.
function deliverMessages(messages: SSEMessage[], onMessage: (message: SSEMessage) => void): void {
  // Deliver in wire order so incremental reducers see the same ordering as the stream.
  for (const message of messages) {
    if (message.event === "error") {
      throw eventStreamError(message);
    }
    onMessage(message);
  }
}

// Open an authenticated SSE stream and feed decoded messages until EOF or abort.
export async function consumeEventStream(
  options: EventStreamOptions,
  onMessage: (message: SSEMessage) => void,
  signal?: AbortSignal
): Promise<void> {
  // EventSource cannot attach bearer headers, so this consumer uses fetch and ReadableStream.
  const headers = new Headers();
  const token = getStoredToken();
  // Token is read at stream creation time; token-change listeners abort and reopen streams.
  if (token) headers.set("Authorization", `Bearer ${token}`);
  headers.set("X-Request-ID", requestID());
  const response = await fetch(createEventStreamURL(options), { headers, signal });
  if (!response.ok) {
    throw new APIError(response.status, "HTTP_ERROR", response.statusText || "event stream request failed");
  }
  if (!response.body) {
    throw new APIError(response.status, "STREAM_UNAVAILABLE", "event stream response body is unavailable");
  }
  // ReadableStream gives this client explicit chunk boundaries for the incremental SSE parser.
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    // TextDecoder stream mode preserves partial UTF-8 code points across chunks.
    const parsed = parseSSEChunk(buffer, decoder.decode(value, { stream: true }));
    buffer = parsed.buffer;
    deliverMessages(parsed.messages, onMessage);
  }
  // Flush decoder state after EOF in case the final chunk ended mid-codepoint.
  // decoder.decode() without args flushes any buffered bytes after the stream closes.
  const tail = decoder.decode();
  if (tail) {
    const parsed = parseSSEChunk(buffer, tail);
    deliverMessages(parsed.messages, onMessage);
  }
}
