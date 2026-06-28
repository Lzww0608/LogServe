import assert from "node:assert/strict";
import test from "node:test";
import { taskActionState } from "../.tmp-task-action-tests/src/utils/taskActions.js";
import { logStreamURL, taskActionURL, tasksURL, workflowsURL } from "../.tmp-task-action-tests/src/api/client.js";

const standaloneFailed = { task_id: "task-1", status: "FAILED", task_name: "add" };

test("taskActionState enables retry only for failed standalone tasks", () => {
  assert.deepEqual(taskActionState(standaloneFailed, "retry"), { enabled: true });
  assert.equal(taskActionState({ task_id: "task-2", status: "RUNNING" }, "retry").enabled, false);
  assert.match(taskActionState({ task_id: "task-2", status: "RUNNING" }, "retry").reason ?? "", /failed standalone/i);
});

test("taskActionState enables resubmit for standalone tasks and blocks derived tasks", () => {
  assert.deepEqual(taskActionState({ task_id: "task-1", status: "SUCCEEDED" }, "resubmit"), { enabled: true });

  const derived = taskActionState({ task_id: "task-step", status: "FAILED", workflow_id: "wf-1", step_id: "step-1" }, "resubmit");
  assert.equal(derived.enabled, false);
  assert.match(derived.reason ?? "", /derived/i);
});

test("taskActionState keeps cancel disabled because backend cancellation is unsupported", () => {
  const state = taskActionState({ task_id: "task-1", status: "QUEUED" }, "cancel");
  assert.equal(state.enabled, false);
  assert.equal(state.unsupported, true);
  assert.match(state.reason ?? "", /not supported/i);
});

test("taskActionURL encodes task ids and action suffixes", () => {
  assert.equal(taskActionURL("task 1/child", "retry"), "/api/tasks/task%201%2Fchild/retry");
  assert.equal(taskActionURL("task:abc", "resubmit"), "/api/tasks/task%3Aabc/resubmit");
});
test("list URL builders encode pagination and filters", () => {
  assert.equal(tasksURL({ q: "add fn", status: "RUNNING", workerID: "worker/1", workflowID: "wf:1", limit: 25, pageToken: "25" }), "/api/tasks?q=add+fn&status=RUNNING&worker_id=worker%2F1&workflow_id=wf%3A1&limit=25&page_token=25");
  assert.equal(workflowsURL({ status: "FAILED", limit: 100, pageToken: "next page" }), "/api/workflows?status=FAILED&limit=100&page_token=next+page");
  assert.equal(logStreamURL("wf:wf-1", 51, 25), "/api/logs/streams/wf%3Awf-1?from_seq=51&limit=25");
});
