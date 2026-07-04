// Unit tests for SSE parsing and event-state merge helpers.

import assert from "node:assert/strict";
import test from "node:test";
import { consumeEventStream, createEventStreamURL, parseSSEChunk } from "../.tmp-event-tests/src/api/events.js";
import { applyLogRecordsEvent, applyTaskEvent, applyWorkflowEvent } from "../.tmp-event-tests/src/utils/eventState.js";

// Verifies parseSSEChunk buffers partial messages and emits completed events.
test("parseSSEChunk buffers partial messages and emits completed events", () => {
  let parsed = parseSSEChunk("", "event: task\ndata: {\"task\":{\"task_id\":\"task-");
  assert.deepEqual(parsed.messages, []);
  assert.equal(parsed.buffer, "event: task\ndata: {\"task\":{\"task_id\":\"task-");

  parsed = parseSSEChunk(parsed.buffer, "1\",\"status\":\"RUNNING\"}}\n\n");
  assert.equal(parsed.buffer, "");
  assert.deepEqual(parsed.messages, [{ event: "task", data: "{\"task\":{\"task_id\":\"task-1\",\"status\":\"RUNNING\"}}" }]);
});

// Verifies createEventStreamURL encodes supported filters.
test("createEventStreamURL encodes supported filters", () => {
  assert.equal(createEventStreamURL({ taskID: "task 1", intervalMs: 1000 }), "/api/events?task_id=task+1&interval_ms=1000");
  assert.equal(createEventStreamURL({ stream: "wf:wf-1", fromSeq: 2, limit: 10 }), "/api/events?stream=wf%3Awf-1&from_seq=2&limit=10");
});

// Verifies applyTaskEvent updates status without dropping existing metadata.
test("applyTaskEvent updates status without dropping existing metadata", () => {
  const current = { task_id: "task-1", task_name: "add", status: "RUNNING", worker_id: "worker-1" };
  const next = applyTaskEvent(current, { task_id: "task-1", status: "SUCCEEDED", result_json: { ok: true } });

  assert.deepEqual(next, { task_id: "task-1", task_name: "add", status: "SUCCEEDED", worker_id: "worker-1", result_json: { ok: true } });
});

// Verifies applyWorkflowEvent merges step updates by step_id and keeps stable order.
test("applyWorkflowEvent merges step updates by step_id and keeps stable order", () => {
  const current = {
    workflow_id: "wf-1",
    workflow_name: "rag",
    status: "RUNNING",
    steps: [
      { step_id: "search", status: "STARTED", task_id: "task-search" }
    ]
  };
  const next = applyWorkflowEvent(current, {
    workflow_id: "wf-1",
    status: "RUNNING",
    steps: [
      { step_id: "search", status: "SUCCEEDED", result_json: ["doc"] },
      { step_id: "answer", status: "SCHEDULED" }
    ]
  });

  assert.deepEqual(next.steps?.map((step) => [step.step_id, step.status, step.task_id]), [
    ["search", "SUCCEEDED", "task-search"],
    ["answer", "SCHEDULED", undefined]
  ]);
  assert.deepEqual(next.steps?.[0].result_json, ["doc"]);
});

// Verifies applyLogRecordsEvent appends new records once by sequence.
test("applyLogRecordsEvent appends new records once by sequence", () => {
  const current = {
    stream_id: "system:functions",
    from_seq: 1,
    limit: 100,
    stats: { stream_id: "system:functions", first_seq: 1, next_seq: 3, trimmed_before_seq: 0, compactable_records: 0, compactable_bytes: 0 },
    records: [
      { stream_id: "system:functions", seq: 1, event_type: "A" },
      { stream_id: "system:functions", seq: 2, event_type: "B" }
    ]
  };

  const next = applyLogRecordsEvent(current, {
    stream_id: "system:functions",
    next_seq: 4,
    records: [
      { stream_id: "system:functions", seq: 2, event_type: "B-duplicate" },
      { stream_id: "system:functions", seq: 3, event_type: "C" }
    ]
  }, 3);

  assert.deepEqual(next.records.map((record) => [record.seq, record.event_type]), [[1, "A"], [2, "B"], [3, "C"]]);
  assert.equal(next.from_seq, 4);
  assert.equal(next.stats?.next_seq, 3);
});

// Verifies consumeEventStream rejects backend error events.
test("consumeEventStream rejects backend error events", async () => {
  const previousFetch = globalThis.fetch;
  const previousSessionStorage = globalThis.sessionStorage;
  Object.defineProperty(globalThis, "sessionStorage", {
    configurable: true,
    value: { getItem: () => "", setItem: () => {}, removeItem: () => {} }
  });
  globalThis.fetch = async () => new Response(new ReadableStream({
    start(controller) {
      controller.enqueue(new TextEncoder().encode('event: error\ndata: {"message":"backend stream failed"}\n\n'));
      controller.close();
    }
  }), { status: 200 });

  const messages = [];
  try {
    await assert.rejects(
      () => consumeEventStream({ taskID: "task-1" }, (message) => messages.push(message)),
      (error) => error?.code === "EVENT_STREAM_ERROR" && /backend stream failed/.test(error.message)
    );
    assert.deepEqual(messages, []);
  } finally {
    if (previousFetch === undefined) {
      delete globalThis.fetch;
    } else {
      globalThis.fetch = previousFetch;
    }
    if (previousSessionStorage === undefined) {
      delete globalThis.sessionStorage;
    } else {
      Object.defineProperty(globalThis, "sessionStorage", { configurable: true, value: previousSessionStorage });
    }
  }
});