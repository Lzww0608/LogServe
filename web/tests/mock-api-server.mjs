import http from "node:http";

const port = Number(process.env.LOGSERVE_MOCK_API_PORT ?? 43080);

const dashboard = {
  queue_depth: 2,
  queue_high_watermark: 10,
  redelivery_timeout_ms: 30000,
  scheduling_policy: "LOCALITY_AWARE",
  tasks: [
    { task_id: "task-queued-1", task_name: "queued add", status: "QUEUED", worker_id: "worker-a", created_at_ms: 1782630000000 },
    { task_id: "task-success-1", task_name: "finished add", status: "SUCCEEDED", worker_id: "worker-a", result_json: { value: 3 }, created_at_ms: 1782630001000 },
    { task_id: "task-failed-1", task_name: "failed add", status: "FAILED", worker_id: "worker-b", error: "boom", created_at_ms: 1782630002000 }
  ],
  workflows: [
    { workflow_id: "wf-1", workflow_name: "simple_add", status: "COMPLETED", step_count: 2, succeeded_steps: 2, failed_steps: 0, running_steps: 0 }
  ],
  actors: [],
  workers: [
    { worker_id: "worker-a", capacity: 4, running_tasks: 1, cached_models: [{ name: "model-A", version: "v1", adapter: "mock" }] }
  ],
  models: [
    { name: "model-A", version: "v1", adapter: "mock", path: "/models/model-A" }
  ],
  last_log_append_ms: 4,
  log_append_slow_ms: 250,
  compactable_log_records: 12,
  compactable_log_bytes: 4096,
  metadata_materializer: { mode: "async", eventual_lag_estimate_ms: 12 }
};

const templates = [
  { id: "add_task", label: "Add task", kind: "task", description: "Runs add(a, b).", expected_result: "Task succeeds with result_json 3.", required_role: "operator" },
  { id: "mock_llm_request", label: "Mock LLM request", kind: "llm", description: "Runs a mock LLM request.", expected_result: "LLM task succeeds with mock text.", required_role: "admin" }
];
const logStats = {
  "system:functions": { stream_id: "system:functions", first_seq: 1, next_seq: 5, trimmed_before_seq: 1, compactable_records: 0, compactable_bytes: 0 },
  "system:tasks": { stream_id: "system:tasks", first_seq: 1, next_seq: 2, trimmed_before_seq: 1, compactable_records: 0, compactable_bytes: 0 },
  "wf:workflow-1": { stream_id: "wf:workflow-1", first_seq: 1, next_seq: 3, trimmed_before_seq: 1, compactable_records: 0, compactable_bytes: 0 },
  "actor:actor-1": { stream_id: "actor:actor-1", first_seq: 1, next_seq: 2, trimmed_before_seq: 1, compactable_records: 0, compactable_bytes: 0 }
};

const server = http.createServer(async (request, response) => {
  const url = new URL(request.url ?? "/", `http://${request.headers.host ?? "127.0.0.1"}`);
  if (!url.pathname.startsWith("/api")) {
    writeJSON(response, 404, { error: { code: "NOT_FOUND", message: "unknown mock endpoint" } });
    return;
  }

  if (url.pathname === "/api/session" && request.method === "GET") {
    const role = roleFromRequest(request);
    writeJSON(response, 200, {
      subject: `${role}:browser-token`,
      role,
      permissions: permissionsForRole(role)
    });
    return;
  }

  if (url.pathname === "/api/templates" && request.method === "GET") {
    writeJSON(response, 200, { templates });
    return;
  }

  if (url.pathname.startsWith("/api/templates/") && url.pathname.endsWith("/run") && request.method === "POST") {
    const templateID = decodeURIComponent(url.pathname.split("/")[3] ?? "add_task");
    const template = templates.find((item) => item.id === templateID) ?? templates[0];
    if (!roleAtLeast(roleFromRequest(request), template.required_role)) {
      writeJSON(response, 403, { error: { code: "PERMISSION_DENIED", message: "insufficient role for this template" } });
      return;
    }
    writeJSON(response, 200, { template, result: templateID === "mock_llm_request" ? { task_id: "llm-task-1", status: "QUEUED" } : taskDetail("task-template-1", "QUEUED") });
    return;
  }
  if (url.pathname === "/api/events") {
    const taskID = url.searchParams.get("task_id");
    const streamID = url.searchParams.get("stream") ?? "";
    if (taskID) {
      writeSSE(response, "task", { task: taskDetail(taskID) });
      return;
    }
    if (streamID.startsWith("wf:")) {
      writeSSE(response, "workflow", { workflow: workflowDetail(streamID.slice("wf:".length)) });
      return;
    }
    writeSSE(response, "dashboard", { dashboard });
    return;
  }

  if (url.pathname === "/api/tasks" && request.method === "GET") {
    const status = url.searchParams.get("status") ?? "";
    const query = (url.searchParams.get("q") ?? "").toLowerCase();
    const workerID = url.searchParams.get("worker_id") ?? "";
    const workflowID = url.searchParams.get("workflow_id") ?? "";
    const rows = dashboard.tasks.filter((task) => {
      if (status && task.status !== status) return false;
      if (workerID && task.worker_id !== workerID) return false;
      if (workflowID && task.workflow_id !== workflowID) return false;
      if (!query) return true;
      const haystack = `${task.task_id} ${task.task_name} ${task.worker_id ?? ""} ${task.workflow_id ?? ""}`.toLowerCase();
      return haystack.includes(query);
    });
    writeJSON(response, 200, paginated(url, rows, "tasks"));
    return;
  }
  if (url.pathname === "/api/tasks" && request.method === "POST") {
    writeJSON(response, 200, taskDetail("task-ui-1", "QUEUED"));
    return;
  }

  if (url.pathname === "/api/workflows" && request.method === "GET") {
    const status = url.searchParams.get("status") ?? "";
    const rows = dashboard.workflows.filter((workflow) => !status || workflow.status === status);
    writeJSON(response, 200, paginated(url, rows, "workflows"));
    return;
  }
  if (url.pathname.startsWith("/api/workflows/") && url.pathname.endsWith("/replay") && request.method === "POST") {
    const workflowID = decodeURIComponent(url.pathname.split("/")[3] ?? "wf-1");
    const workflow = workflowDetail(workflowID);
    workflow.steps = [...workflow.steps, { step_id: "verify", task_name: "verify_result", status: "SUCCEEDED", attempts: 1, task_id: "task-verify-1", latency_ms: 7, depends_on: ["add"], started_at_ms: 1782630003000, completed_at_ms: 1782630004000, result_json: { consistent: true } }];
    workflow.step_count = workflow.steps.length;
    workflow.completed_at_ms = 1782630004000;
    workflow.updated_at_ms = 1782630004000;
    workflow.latency_ms = 40;
    writeJSON(response, 200, { workflow, consistent_with_metadata: true });
    return;
  }

  if (url.pathname === "/api/workflows/validate" && request.method === "POST") {
    const payload = await readJSON(request);
    writeJSON(response, 200, {
      valid: true,
      message: "ok",
      normalized_definition: payload?.definition ?? null,
      warnings: []
    });
    return;
  }

  if (url.pathname === "/api/models" && request.method === "GET") {
    writeJSON(response, 200, { models: dashboard.models });
    return;
  }

  if (url.pathname === "/api/models" && request.method === "POST") {
    const payload = await readJSON(request);
    writeJSON(response, 200, {
      name: payload?.name ?? "model-A",
      version: payload?.version ?? "v1",
      adapter: payload?.adapter ?? "mock",
      path: payload?.path ?? "/models/model-A"
    });
    return;
  }

  if (url.pathname === "/api/admin/scheduling-policy" && request.method === "POST") {
    const payload = await readJSON(request);
    writeJSON(response, 200, { policy: payload?.policy ?? "LOCALITY_AWARE" });
    return;
  }

  if (url.pathname === "/api/llm" && request.method === "POST") {
    writeJSON(response, 200, {
      task_id: "llm-task-1",
      status: "SUCCEEDED",
      result_json: { text: "LogServe records workflow events in an auditable log." },
      model_name: "model-A",
      model_version: "v1"
    });
    return;
  }

  if (url.pathname.startsWith("/api/llm/") && url.pathname.endsWith("/replay") && request.method === "POST") {
    writeJSON(response, 200, {
      task_id: decodeURIComponent(url.pathname.split("/")[3] ?? "llm-task-1"),
      status: "SUCCEEDED",
      result_json: { text: "Replay matched the recorded trace." },
      worker_id: "worker-a",
      model_name: "model-A",
      model_version: "v1",
      cache_hit: true,
      model_load_ms: 12,
      checkpoint_fetch_ms: 3,
      first_token_ms: 18,
      total_latency_ms: 42,
      events: [{ event_type: "MODEL_LOADED", worker_id: "worker-a", latency_ms: 12 }, { event_type: "TOKEN_STREAM", model_name: "model-A", latency_ms: 42 }]
    });
    return;
  }

  if (url.pathname === "/api/logs/streams" && request.method === "GET") {
    const prefix = url.searchParams.get("prefix") ?? "";
    const streamIDs = Object.keys(logStats).filter((streamID) => streamID.startsWith(prefix));
    writeJSON(response, 200, { stream_ids: streamIDs, stats: streamIDs.map((streamID) => logStats[streamID]) });
    return;
  }

  if (url.pathname.startsWith("/api/logs/streams/") && request.method === "GET") {
    const streamID = decodeURIComponent(url.pathname.slice("/api/logs/streams/".length));
    const fromSeq = Number(url.searchParams.get("from_seq") ?? 1);
    const limit = Number(url.searchParams.get("limit") ?? 50);
    if (limit < 1 || limit > 1000) {
      writeJSON(response, 400, { error: { code: "INVALID_ARGUMENT", message: "limit must be between 1 and 1000" } });
      return;
    }
    const stats = logStats[streamID] ?? { stream_id: streamID, first_seq: 1, next_seq: 1, trimmed_before_seq: 1, compactable_records: 0, compactable_bytes: 0 };
    const records = [
      { stream_id: streamID, seq: fromSeq, event_type: "APPEND", idempotency_key: `${streamID}-append`, payload_json: { stream_id: streamID }, timestamp_ms: 1782630000000, crc32: 123 },
      { stream_id: streamID, seq: fromSeq + 1, event_type: "COMMIT", idempotency_key: `${streamID}-commit`, payload_json: { ok: true }, timestamp_ms: 1782630001000, crc32: 456 }
    ].slice(0, limit);
    writeJSON(response, 200, {
      stream_id: streamID,
      from_seq: fromSeq,
      limit,
      records,
      stats,
      next_seq: records.at(-1)?.seq ? records.at(-1).seq + 1 : fromSeq,
      has_more: false
    });
    return;
  }

  writeJSON(response, 404, { error: { code: "NOT_FOUND", message: `unhandled mock endpoint ${request.method} ${url.pathname}` } });
});

server.listen(port, "127.0.0.1", () => {
  console.log(`LogServe mock API listening on http://127.0.0.1:${port}`);
});

process.on("SIGTERM", () => server.close(() => process.exit(0)));
process.on("SIGINT", () => server.close(() => process.exit(0)));

function taskDetail(taskID, status = "SUCCEEDED") {
  return {
    task_id: taskID,
    task_name: "browser add",
    status,
    worker_id: "worker-a",
    created_at_ms: 1782630000000,
    updated_at_ms: 1782630002000,
    result_json: { value: 3 }
  };
}

function workflowDetail(workflowID) {
  return {
    workflow_id: workflowID || "wf-1",
    workflow_name: "simple_add",
    status: "COMPLETED",
    created_at_ms: 1782630000000,
    updated_at_ms: 1782630003000,
    completed_at_ms: 1782630003000,
    latency_ms: 33,
    step_count: 2,
    succeeded_steps: 2,
    failed_steps: 0,
    running_steps: 0,
    result_json: { value: 3 },
    steps: [
      { step_id: "load", task_name: "load_input", status: "SUCCEEDED", attempts: 1, task_id: "task-load-1", latency_ms: 11, depends_on: [], started_at_ms: 1782630000000, completed_at_ms: 1782630001000, result_json: { value: 1 } },
      { step_id: "add", task_name: "add", status: "SUCCEEDED", attempts: 1, task_id: "task-add-1", latency_ms: 22, depends_on: ["load"], started_at_ms: 1782630001000, completed_at_ms: 1782630003000, result_json: { value: 3 } }
    ]
  };
}

function paginated(url, rows, key) {
  const limit = positiveInt(url.searchParams.get("limit"), 50);
  const offset = positiveInt(url.searchParams.get("page_token"), 0);
  const page = rows.slice(offset, offset + limit);
  const nextOffset = offset + limit < rows.length ? String(offset + limit) : "";
  return {
    [key]: page,
    limit,
    total_count: rows.length,
    next_page_token: nextOffset
  };
}

function positiveInt(value, fallback) {
  const parsed = Number(value ?? "");
  return Number.isInteger(parsed) && parsed > 0 ? parsed : fallback;
}
function writeJSON(response, status, payload) {
  response.writeHead(status, {
    "Content-Type": "application/json; charset=utf-8",
    "Cache-Control": "no-store"
  });
  response.end(JSON.stringify(payload));
}

function writeSSE(response, event, payload) {
  response.writeHead(200, {
    "Content-Type": "text/event-stream; charset=utf-8",
    "Cache-Control": "no-store",
    Connection: "close"
  });
  response.end(`event: ${event}\ndata: ${JSON.stringify(payload)}\n\n`);
}

function readJSON(request) {
  return new Promise((resolve) => {
    let body = "";
    request.setEncoding("utf8");
    request.on("data", (chunk) => {
      body += chunk;
    });
    request.on("end", () => {
      try {
        resolve(body ? JSON.parse(body) : {});
      } catch {
        resolve({});
      }
    });
  });
}
function roleFromRequest(request) {
  const value = String(request.headers.authorization ?? "").replace(/^Bearer\s+/i, "").trim();
  if (value === "viewer-token") return "viewer";
  if (value === "operator-token") return "operator";
  return "admin";
}

function permissionsForRole(role) {
  const permissions = ["read:dashboard", "read:tasks", "read:workflows", "read:logs", "read:templates"];
  if (roleAtLeast(role, "operator")) permissions.push("submit:tasks", "submit:workflows", "call:actors", "replay", "set:scheduling", "run:templates");
  if (roleAtLeast(role, "admin")) permissions.push("set:backpressure", "register:models", "dangerous:actions");
  return permissions;
}

function roleAtLeast(role, minimum) {
  const ranks = { viewer: 1, operator: 2, admin: 3 };
  return (ranks[role] ?? 0) >= (ranks[minimum] ?? 0);
}