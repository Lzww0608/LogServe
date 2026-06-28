import { APIError, getStoredToken, requestID } from "./client.js";

export type EventStreamOptions = {
  taskID?: string;
  stream?: string;
  fromSeq?: number;
  limit?: number;
  intervalMs?: number;
  records?: boolean;
  enabled?: boolean;
};

export type SSEMessage = {
  event: string;
  data: string;
};

export function createEventStreamURL(options: EventStreamOptions = {}): string {
  const params = new URLSearchParams();
  if (options.taskID) params.set("task_id", options.taskID);
  if (options.stream) params.set("stream", options.stream);
  if (options.fromSeq !== undefined) params.set("from_seq", String(options.fromSeq));
  if (options.limit !== undefined) params.set("limit", String(options.limit));
  if (options.intervalMs !== undefined) params.set("interval_ms", String(options.intervalMs));
  if (options.records) params.set("records", "1");
  const query = params.toString();
  return query ? `/api/events?${query}` : "/api/events";
}

export function parseSSEChunk(buffer: string, chunk: string): { buffer: string; messages: SSEMessage[] } {
  let pending = (buffer + chunk).replace(/\r\n/g, "\n");
  const messages: SSEMessage[] = [];
  let boundary = pending.indexOf("\n\n");
  while (boundary >= 0) {
    const rawMessage = pending.slice(0, boundary);
    pending = pending.slice(boundary + 2);
    const message = parseSSEMessage(rawMessage);
    if (message) messages.push(message);
    boundary = pending.indexOf("\n\n");
  }
  return { buffer: pending, messages };
}

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
      data.push(trimSSEValue(line.slice("data:".length)));
    }
  }
  if (!data.length) return null;
  return { event, data: data.join("\n") };
}

function trimSSEValue(value: string): string {
  return value.startsWith(" ") ? value.slice(1) : value;
}

export function parseEventData<T>(message: SSEMessage): T {
  return JSON.parse(message.data) as T;
}

function eventStreamError(message: SSEMessage): Error {
  try {
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

function deliverMessages(messages: SSEMessage[], onMessage: (message: SSEMessage) => void): void {
  for (const message of messages) {
    if (message.event === "error") {
      throw eventStreamError(message);
    }
    onMessage(message);
  }
}

export async function consumeEventStream(
  options: EventStreamOptions,
  onMessage: (message: SSEMessage) => void,
  signal?: AbortSignal
): Promise<void> {
  const headers = new Headers();
  const token = getStoredToken();
  if (token) headers.set("Authorization", `Bearer ${token}`);
  headers.set("X-Request-ID", requestID());
  const response = await fetch(createEventStreamURL(options), { headers, signal });
  if (!response.ok) {
    throw new APIError(response.status, "HTTP_ERROR", response.statusText || "event stream request failed");
  }
  if (!response.body) {
    throw new APIError(response.status, "STREAM_UNAVAILABLE", "event stream response body is unavailable");
  }
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    const parsed = parseSSEChunk(buffer, decoder.decode(value, { stream: true }));
    buffer = parsed.buffer;
    deliverMessages(parsed.messages, onMessage);
  }
  const tail = decoder.decode();
  if (tail) {
    const parsed = parseSSEChunk(buffer, tail);
    deliverMessages(parsed.messages, onMessage);
  }
}
