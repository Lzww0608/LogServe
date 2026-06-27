import { FormEvent, ReactNode, useEffect, useMemo, useState } from "react";
import { api, APIError, getStoredToken, setStoredToken } from "./api/client";
import type { Actor, Dashboard, LLMTrace, ModelInfo, Task, Worker, Workflow } from "./types/logserve";

const addSource = `def add(a: int, b: int) -> int:
    return a + b
`;

const failSource = `def fail() -> None:
    raise RuntimeError("demo failure")
`;

const counterSource = `class Counter:
    def __init__(self, value=0):
        self.value = value

    def inc(self, by=1):
        self.value += by
        return self.value
`;

const workflowTemplate = {
  workflow_name: "simple_add",
  steps: [
    {
      step_id: "add",
      task_name: "add",
      function_name: "add",
      function_source: addSource,
      args_json: { args: [1, 2], kwargs: {} },
      depends_on: []
    }
  ],
  result_step_id: "add",
  max_attempts: 3,
  timeout_ms: 30000
};

type Column<T> = {
  label: string;
  render: (row: T) => ReactNode;
  className?: string;
};

type LoadState<T> = {
  data?: T;
  error?: string;
  loading: boolean;
};

function navigate(path: string) {
  window.history.pushState({}, "", path);
  window.dispatchEvent(new PopStateEvent("popstate"));
}

export function App() {
  const [path, setPath] = useState(window.location.pathname);

  useEffect(() => {
    const onPop = () => setPath(window.location.pathname);
    const onClick = (event: MouseEvent) => {
      const target = event.target as HTMLElement | null;
      const link = target?.closest?.("a[data-nav]") as HTMLAnchorElement | null;
      if (!link || link.origin !== window.location.origin) {
        return;
      }
      event.preventDefault();
      navigate(link.pathname);
    };
    window.addEventListener("popstate", onPop);
    document.addEventListener("click", onClick);
    return () => {
      window.removeEventListener("popstate", onPop);
      document.removeEventListener("click", onClick);
    };
  }, []);

  return (
    <div className="shell">
      <aside className="sidebar">
        <div className="brand">LogServe</div>
        <nav>
          <NavLink path="/" current={path}>Overview</NavLink>
          <NavLink path="/submit/task" current={path}>Submit Task</NavLink>
          <NavLink path="/tasks" current={path}>Tasks</NavLink>
          <NavLink path="/workflows" current={path}>Workflows</NavLink>
          <NavLink path="/workflows/new" current={path}>Workflow Builder</NavLink>
          <NavLink path="/actors" current={path}>Actors</NavLink>
          <NavLink path="/llm" current={path}>LLM Serving</NavLink>
          <NavLink path="/workers" current={path}>Workers</NavLink>
          <NavLink path="/settings" current={path}>Settings</NavLink>
        </nav>
      </aside>
      <main className="content">
        <Header path={path} />
        {route(path)}
      </main>
    </div>
  );
}

function route(path: string) {
  if (path === "/") return <OverviewPage />;
  if (path === "/submit/task") return <SubmitTaskPage />;
  if (path === "/tasks") return <TasksPage />;
  if (path.startsWith("/tasks/")) return <TaskDetailPage taskID={decodeURIComponent(path.split("/")[2] ?? "")} />;
  if (path === "/workflows") return <WorkflowsPage />;
  if (path === "/workflows/new") return <WorkflowBuilderPage />;
  if (path.startsWith("/workflows/")) return <WorkflowDetailPage workflowID={decodeURIComponent(path.split("/")[2] ?? "")} />;
  if (path === "/actors") return <ActorsPage />;
  if (path.startsWith("/actors/")) return <ActorDetailPage actorID={decodeURIComponent(path.split("/")[2] ?? "")} />;
  if (path === "/llm") return <LLMPage />;
  if (path === "/workers") return <WorkersPage />;
  if (path === "/settings") return <SettingsPage />;
  return <NotFoundPage />;
}

function Header({ path }: { path: string }) {
  const title = pathTitle(path);
  return (
    <header className="topbar">
      <div>
        <h1>{title}</h1>
        <span className="subtle">Control plane: HTTP gateway</span>
      </div>
      <button className="ghost" onClick={() => window.location.reload()}>Refresh</button>
    </header>
  );
}

function NavLink({ path, current, children }: { path: string; current: string; children: ReactNode }) {
  const active = path === "/" ? current === "/" : current === path || current.startsWith(path + "/");
  return (
    <a data-nav href={path} className={active ? "active" : ""}>
      {children}
    </a>
  );
}

function pathTitle(path: string) {
  if (path === "/") return "Overview";
  if (path.startsWith("/tasks/")) return "Task Detail";
  if (path.startsWith("/workflows/") && path !== "/workflows/new") return "Workflow Detail";
  if (path.startsWith("/actors/")) return "Actor Detail";
  if (path === "/submit/task") return "Submit Task";
  return path.slice(1).split("/").map((part) => part.charAt(0).toUpperCase() + part.slice(1)).join(" ");
}

function usePolling<T>(loader: () => Promise<T>, intervalMs: number, deps: unknown[] = []): LoadState<T> {
  const [state, setState] = useState<LoadState<T>>({ loading: true });
  useEffect(() => {
    let cancelled = false;
    let timer: number | undefined;
    const load = async () => {
      try {
        const data = await loader();
        if (!cancelled) setState({ data, loading: false });
      } catch (error) {
        if (!cancelled) setState({ error: errorMessage(error), loading: false });
      }
    };
    void load();
    if (intervalMs > 0) {
      timer = window.setInterval(load, intervalMs);
    }
    return () => {
      cancelled = true;
      if (timer) window.clearInterval(timer);
    };
  }, deps);
  return state;
}

function OverviewPage() {
  const state = usePolling(() => api.dashboard(), 1000);
  if (state.error) return <ErrorPanel message={state.error} />;
  const dashboard = state.data;
  if (!dashboard) return <Loading />;
  const running = dashboard.tasks.filter((task) => task.status === "RUNNING").length;
  const queued = dashboard.tasks.filter((task) => task.status === "QUEUED").length;
  const succeeded = dashboard.tasks.filter((task) => task.status === "SUCCEEDED").length;
  const failed = dashboard.tasks.filter((task) => task.status === "FAILED").length;
  const workerCapacity = dashboard.workers.reduce((sum, worker) => sum + (worker.capacity || 0), 0);
  return (
    <div className="stack">
      <div className="kpi-grid">
        <Kpi label="Queue Depth" value={dashboard.queue_depth} tone={dashboard.queue_depth >= dashboard.queue_high_watermark && dashboard.queue_high_watermark > 0 ? "bad" : "neutral"} />
        <Kpi label="Running Tasks" value={running} tone="info" />
        <Kpi label="Queued Tasks" value={queued} />
        <Kpi label="Succeeded" value={succeeded} tone="good" />
        <Kpi label="Failed" value={failed} tone={failed > 0 ? "bad" : "neutral"} />
        <Kpi label="Active Workers" value={dashboard.workers.length} tone="good" />
        <Kpi label="Worker Capacity" value={workerCapacity} />
        <Kpi label="Models" value={dashboard.models.length} />
        <Kpi label="Scheduling" value={dashboard.scheduling_policy} />
        <Kpi label="Log Append" value={`${dashboard.last_log_append_ms || 0} ms`} tone={dashboard.log_append_slow_ms > 0 && dashboard.last_log_append_ms >= dashboard.log_append_slow_ms ? "bad" : "neutral"} />
        <Kpi label="Materializer Lag" value={`${dashboard.metadata_materializer?.eventual_lag_estimate_ms ?? 0} ms`} />
        <Kpi label="Compactable Bytes" value={dashboard.compactable_log_bytes} />
      </div>
      <section className="panel">
        <PanelTitle title="Recent Tasks" action={<a data-nav className="button ghost" href="/tasks">Open</a>} />
        <TaskTable rows={[...dashboard.tasks].reverse().slice(0, 10)} />
      </section>
      <section className="panel split">
        <div>
          <PanelTitle title="Workflows" action={<a data-nav className="button ghost" href="/workflows">Open</a>} />
          <WorkflowTable rows={dashboard.workflows.slice(0, 6)} />
        </div>
        <div>
          <PanelTitle title="Workers" action={<a data-nav className="button ghost" href="/workers">Open</a>} />
          <WorkerTable rows={dashboard.workers.slice(0, 6)} />
        </div>
      </section>
    </div>
  );
}

function TasksPage() {
  const [query, setQuery] = useState("");
  const [status, setStatus] = useState("");
  const search = useMemo(() => {
    const params = new URLSearchParams();
    if (query.trim()) params.set("q", query.trim());
    if (status) params.set("status", status);
    const encoded = params.toString();
    return encoded ? `?${encoded}` : "";
  }, [query, status]);
  const state = usePolling(() => api.tasks(search), 1000, [search]);
  if (state.error) return <ErrorPanel message={state.error} />;
  return (
    <section className="panel">
      <div className="toolbar">
        <input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="Search tasks" />
        <select value={status} onChange={(event) => setStatus(event.target.value)}>
          <option value="">All status</option>
          <option value="QUEUED">QUEUED</option>
          <option value="RUNNING">RUNNING</option>
          <option value="SUCCEEDED">SUCCEEDED</option>
          <option value="FAILED">FAILED</option>
        </select>
        <a data-nav className="button primary" href="/submit/task">Submit</a>
      </div>
      <TaskTable rows={state.data?.tasks ?? []} />
    </section>
  );
}

function TaskDetailPage({ taskID }: { taskID: string }) {
  const state = usePolling(() => api.task(taskID), 1000, [taskID]);
  if (state.error) return <ErrorPanel message={state.error} />;
  const task = state.data;
  if (!task) return <Loading />;
  return (
    <div className="stack">
      <section className="panel">
        <PanelTitle title={task.task_id} action={<StatusBadge value={task.status} />} />
        <DetailGrid items={[
          ["Worker", task.worker_id],
          ["Created", formatTime(task.created_at_ms)],
          ["Updated", formatTime(task.updated_at_ms)],
          ["Workflow", task.workflow_id],
          ["Actor", task.actor_id],
          ["Model", modelLabel(task)]
        ]} />
      </section>
      {task.error && <ErrorPanel message={task.error} />}
      <section className="panel">
        <h2>Result</h2>
        <JsonViewer value={task.result_json ?? null} />
      </section>
    </div>
  );
}

function SubmitTaskPage() {
  const [mode, setMode] = useState<"source" | "ref" | "hash">("source");
  const [taskName, setTaskName] = useState("add");
  const [functionName, setFunctionName] = useState("add");
  const [source, setSource] = useState(addSource);
  const [functionRef, setFunctionRef] = useState("");
  const [functionHash, setFunctionHash] = useState("");
  const [args, setArgs] = useState("[1, 2]");
  const [kwargs, setKwargs] = useState("{}");
  const [idempotencyKey, setIdempotencyKey] = useState(defaultID("ui-task"));
  const [message, setMessage] = useState("");
  const [submitting, setSubmitting] = useState(false);

  const payload = {
    task_name: taskName,
    function_name: functionName,
    function_source: mode === "source" ? source : "",
    function_ref: mode === "ref" ? functionRef : "",
    function_hash: mode === "hash" ? functionHash : "",
    args: safePreview(args),
    kwargs: safePreview(kwargs),
    idempotency_key: idempotencyKey
  };

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setMessage("");
    try {
      setSubmitting(true);
      const result = await api.submitTask({
        ...payload,
        args: JSON.parse(args || "[]"),
        kwargs: JSON.parse(kwargs || "{}")
      });
      navigate(`/tasks/${result.task_id}`);
    } catch (error) {
      setMessage(errorMessage(error));
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <form className="stack" onSubmit={submit}>
      <section className="panel two-col">
        <div className="form-grid">
          <label>Mode<select value={mode} onChange={(event) => setMode(event.target.value as "source" | "ref" | "hash")}>
            <option value="source">Python source</option>
            <option value="ref">Function ref</option>
            <option value="hash">Function hash</option>
          </select></label>
          <label>Task name<input value={taskName} onChange={(event) => setTaskName(event.target.value)} /></label>
          <label>Function name<input value={functionName} onChange={(event) => setFunctionName(event.target.value)} /></label>
          {mode === "ref" && <label>Function ref<input value={functionRef} onChange={(event) => setFunctionRef(event.target.value)} /></label>}
          {mode === "hash" && <label>Function hash<input value={functionHash} onChange={(event) => setFunctionHash(event.target.value)} /></label>}
          <label>Args JSON<textarea className="short" value={args} onChange={(event) => setArgs(event.target.value)} /></label>
          <label>Kwargs JSON<textarea className="short" value={kwargs} onChange={(event) => setKwargs(event.target.value)} /></label>
          <label>Idempotency key<input value={idempotencyKey} onChange={(event) => setIdempotencyKey(event.target.value)} /></label>
          <div className="button-row">
            <button type="button" className="ghost" onClick={() => setSource(addSource)}>Add</button>
            <button type="button" className="ghost" onClick={() => setSource(failSource)}>Fail</button>
            <button type="button" className="ghost" onClick={() => setIdempotencyKey(defaultID("ui-task"))}>New key</button>
            <button type="submit" className="primary" disabled={submitting}>Submit</button>
          </div>
          {message && <InlineError message={message} />}
        </div>
        <label className="code-field">Python source<textarea value={source} onChange={(event) => setSource(event.target.value)} disabled={mode !== "source"} /></label>
      </section>
      <section className="panel">
        <h2>Payload</h2>
        <JsonViewer value={payload} />
      </section>
    </form>
  );
}

function WorkflowsPage() {
  const state = usePolling(() => api.workflows(), 1000);
  if (state.error) return <ErrorPanel message={state.error} />;
  return (
    <section className="panel">
      <PanelTitle title="Workflows" action={<a data-nav className="button primary" href="/workflows/new">New</a>} />
      <WorkflowTable rows={state.data?.workflows ?? []} />
    </section>
  );
}

function WorkflowBuilderPage() {
  const [workflowName, setWorkflowName] = useState("simple_add");
  const [definition, setDefinition] = useState(JSON.stringify(workflowTemplate, null, 2));
  const [validation, setValidation] = useState<unknown>(null);
  const [message, setMessage] = useState("");

  const validate = async () => {
    setMessage("");
    try {
      const parsed = JSON.parse(definition);
      setValidation(await api.validateWorkflow({ workflow_name: workflowName, definition: parsed }));
    } catch (error) {
      setMessage(errorMessage(error));
    }
  };

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setMessage("");
    try {
      const parsed = JSON.parse(definition);
      const result = await api.submitWorkflow({ workflow_name: workflowName, definition: parsed, idempotency_key: defaultID("ui-wf") });
      navigate(`/workflows/${result.workflow_id}`);
    } catch (error) {
      setMessage(errorMessage(error));
    }
  };

  return (
    <form className="stack" onSubmit={submit}>
      <section className="panel two-col">
        <div className="form-grid">
          <label>Workflow name<input value={workflowName} onChange={(event) => setWorkflowName(event.target.value)} /></label>
          <div className="button-row">
            <button type="button" className="ghost" onClick={validate}>Validate</button>
            <button type="submit" className="primary">Submit</button>
          </div>
          {message && <InlineError message={message} />}
          <div>
            <h2>Validation</h2>
            <JsonViewer value={validation ?? { valid: false }} />
          </div>
        </div>
        <label className="code-field">Definition JSON<textarea value={definition} onChange={(event) => setDefinition(event.target.value)} /></label>
      </section>
    </form>
  );
}

function WorkflowDetailPage({ workflowID }: { workflowID: string }) {
  const state = usePolling(() => api.workflow(workflowID), 1000, [workflowID]);
  const [replay, setReplay] = useState<unknown>(null);
  const [message, setMessage] = useState("");
  if (state.error) return <ErrorPanel message={state.error} />;
  const workflow = state.data;
  if (!workflow) return <Loading />;
  return (
    <div className="stack">
      <section className="panel">
        <PanelTitle title={workflow.workflow_name || workflow.workflow_id} action={<StatusBadge value={workflow.status} />} />
        <DetailGrid items={[
          ["Workflow ID", workflow.workflow_id],
          ["Latency", workflow.latency_ms ? `${workflow.latency_ms} ms` : "-"],
          ["Created", formatTime(workflow.created_at_ms)],
          ["Updated", formatTime(workflow.updated_at_ms)]
        ]} />
      </section>
      <section className="panel">
        <PanelTitle title="Step Flow" action={<button className="ghost" onClick={async () => {
          try {
            setReplay(await api.replayWorkflow(workflowID));
            setMessage("");
          } catch (error) {
            setMessage(errorMessage(error));
          }
        }}>Replay</button>} />
        <Dag steps={workflow.steps ?? []} />
        {message && <InlineError message={message} />}
        {replay !== null && <JsonViewer value={replay} />}
      </section>
      <section className="panel">
        <h2>Steps</h2>
        <StepTable rows={workflow.steps ?? []} />
      </section>
      <section className="panel">
        <h2>Result</h2>
        <JsonViewer value={workflow.result_json ?? workflow.error ?? null} />
      </section>
    </div>
  );
}

function ActorsPage() {
  const state = usePolling(() => api.actors(), 1000);
  const [className, setClassName] = useState("Counter");
  const [classSource, setClassSource] = useState(counterSource);
  const [initArgs, setInitArgs] = useState("[0]");
  const [message, setMessage] = useState("");

  const create = async (event: FormEvent) => {
    event.preventDefault();
    setMessage("");
    try {
      const actor = await api.createActor({
        class_name: className,
        class_source: classSource,
        init_args: JSON.parse(initArgs || "[]"),
        init_kwargs: {},
        idempotency_key: defaultID("ui-actor"),
        snapshot_every: 25
      });
      navigate(`/actors/${actor.actor_id}`);
    } catch (error) {
      setMessage(errorMessage(error));
    }
  };

  return (
    <div className="stack">
      <section className="panel">
        <PanelTitle title="Actors" />
        <ActorTable rows={state.data?.actors ?? []} />
      </section>
      <form className="panel two-col" onSubmit={create}>
        <div className="form-grid">
          <label>Class name<input value={className} onChange={(event) => setClassName(event.target.value)} /></label>
          <label>Init args<textarea className="short" value={initArgs} onChange={(event) => setInitArgs(event.target.value)} /></label>
          <button className="primary">Create</button>
          {message && <InlineError message={message} />}
        </div>
        <label className="code-field">Class source<textarea value={classSource} onChange={(event) => setClassSource(event.target.value)} /></label>
      </form>
    </div>
  );
}

function ActorDetailPage({ actorID }: { actorID: string }) {
  const state = usePolling(() => api.actor(actorID), 1000, [actorID]);
  const [method, setMethod] = useState("inc");
  const [args, setArgs] = useState("[1]");
  const [callResult, setCallResult] = useState<unknown>(null);
  const [message, setMessage] = useState("");

  const call = async (event: FormEvent) => {
    event.preventDefault();
    setMessage("");
    try {
      setCallResult(await api.callActor(actorID, { method_name: method, args: JSON.parse(args || "[]"), kwargs: {}, timeout_ms: 30000 }));
    } catch (error) {
      setMessage(errorMessage(error));
    }
  };

  const actor = state.data;
  if (state.error) return <ErrorPanel message={state.error} />;
  if (!actor) return <Loading />;
  return (
    <div className="stack">
      <section className="panel">
        <PanelTitle title={actor.actor_id} action={<StatusBadge value={actor.status} />} />
        <DetailGrid items={[
          ["Class", actor.class_name],
          ["Owner", actor.owner_worker_id],
          ["Epoch", actor.epoch],
          ["Commands", actor.command_count],
          ["Snapshot", actor.snapshot_command_count]
        ]} />
      </section>
      <form className="panel form-grid compact" onSubmit={call}>
        <label>Method<input value={method} onChange={(event) => setMethod(event.target.value)} /></label>
        <label>Args<textarea className="short" value={args} onChange={(event) => setArgs(event.target.value)} /></label>
        <div className="button-row">
          <button className="primary">Call</button>
          <button type="button" className="ghost" onClick={async () => setCallResult(await api.replayActor(actorID))}>Replay</button>
        </div>
        {message && <InlineError message={message} />}
      </form>
      <section className="panel split">
        <div>
          <h2>State</h2>
          <JsonViewer value={actor.state_json ?? null} />
        </div>
        <div>
          <h2>Call Result</h2>
          <JsonViewer value={callResult ?? null} />
        </div>
      </section>
    </div>
  );
}

function LLMPage() {
  const modelsState = usePolling(() => api.models(), 1000);
  const [modelName, setModelName] = useState("model-A");
  const [modelVersion, setModelVersion] = useState("v1");
  const [adapter, setAdapter] = useState("mock");
  const [prompt, setPrompt] = useState("Summarize LogServe in one sentence.");
  const [taskID, setTaskID] = useState("");
  const [trace, setTrace] = useState<LLMTrace | null>(null);
  const [policy, setPolicy] = useState("LOCALITY_AWARE");
  const [message, setMessage] = useState("");

  const register = async () => {
    setMessage("");
    try {
      await api.registerModel({ name: modelName, version: modelVersion, adapter, path: `/models/${modelName}` });
    } catch (error) {
      setMessage(errorMessage(error));
    }
  };

  const submit = async () => {
    setMessage("");
    try {
      const result = await api.submitLLM({ model_name: modelName, model_version: modelVersion, adapter, prompt, max_tokens: 64, idempotency_key: defaultID("ui-llm") });
      if (result.task_id) setTaskID(result.task_id);
      setTrace(result);
    } catch (error) {
      setMessage(errorMessage(error));
    }
  };

  const replay = async () => {
    if (!taskID.trim()) return;
    setMessage("");
    try {
      setTrace(await api.replayLLM(taskID.trim()));
    } catch (error) {
      setMessage(errorMessage(error));
    }
  };

  return (
    <div className="stack">
      <section className="panel split">
        <div>
          <PanelTitle title="Models" />
          <ModelTable rows={modelsState.data?.models ?? []} />
        </div>
        <div className="form-grid">
          <label>Model name<input value={modelName} onChange={(event) => setModelName(event.target.value)} /></label>
          <label>Version<input value={modelVersion} onChange={(event) => setModelVersion(event.target.value)} /></label>
          <label>Adapter<input value={adapter} onChange={(event) => setAdapter(event.target.value)} /></label>
          <label>Scheduling<select value={policy} onChange={(event) => setPolicy(event.target.value)}>
            <option>LOCALITY_AWARE</option>
            <option>RESOURCE_ONLY</option>
            <option>PREDICTED_LATENCY</option>
          </select></label>
          <div className="button-row">
            <button className="ghost" onClick={register}>Register</button>
            <button className="ghost" onClick={async () => setMessage((await api.setSchedulingPolicy(policy)).policy)}>Set Policy</button>
          </div>
        </div>
      </section>
      <section className="panel two-col">
        <div className="form-grid">
          <label>Prompt<textarea value={prompt} onChange={(event) => setPrompt(event.target.value)} /></label>
          <label>Trace task<input value={taskID} onChange={(event) => setTaskID(event.target.value)} /></label>
          <div className="button-row">
            <button className="primary" onClick={submit}>Submit</button>
            <button className="ghost" onClick={replay}>Replay</button>
          </div>
          {message && <InlineError message={message} />}
        </div>
        <div>
          <h2>LLM Trace</h2>
          <JsonViewer value={trace ?? null} />
        </div>
      </section>
    </div>
  );
}

function WorkersPage() {
  const state = usePolling(() => api.workers(), 1000);
  if (state.error) return <ErrorPanel message={state.error} />;
  return (
    <section className="panel">
      <PanelTitle title="Workers" />
      <WorkerTable rows={state.data?.workers ?? []} />
    </section>
  );
}

function SettingsPage() {
  const [token, setToken] = useState(getStoredToken());
  const [message, setMessage] = useState("");
  return (
    <section className="panel form-grid compact">
      <label>API token<input type="password" value={token} onChange={(event) => setToken(event.target.value)} /></label>
      <div className="button-row">
        <button className="primary" onClick={() => {
          setStoredToken(token);
          setMessage("Saved");
        }}>Save</button>
        <button className="ghost" onClick={() => {
          setToken("");
          setStoredToken("");
          setMessage("Cleared");
        }}>Clear</button>
      </div>
      {message && <span className="subtle">{message}</span>}
    </section>
  );
}

function NotFoundPage() {
  return <ErrorPanel message="Page not found" />;
}

function TaskTable({ rows }: { rows: Task[] }) {
  return <Table rows={rows} empty="No tasks" columns={[
    { label: "Task", render: (row) => <a data-nav href={`/tasks/${row.task_id}`}>{row.task_id}</a> },
    { label: "Name", render: (row) => row.task_name || "-" },
    { label: "Status", render: (row) => <StatusBadge value={row.status} /> },
    { label: "Worker", render: (row) => row.worker_id || "-" },
    { label: "Workflow", render: (row) => row.workflow_id ? <a data-nav href={`/workflows/${row.workflow_id}`}>{row.workflow_id}</a> : "-" },
    { label: "Actor", render: (row) => row.actor_id ? <a data-nav href={`/actors/${row.actor_id}`}>{row.actor_id}</a> : "-" },
    { label: "Model", render: modelLabel }
  ]} />;
}

function WorkflowTable({ rows }: { rows: Workflow[] }) {
  return <Table rows={rows} empty="No workflows" columns={[
    { label: "Workflow", render: (row) => <a data-nav href={`/workflows/${row.workflow_id}`}>{row.workflow_name || row.workflow_id}</a> },
    { label: "Status", render: (row) => <StatusBadge value={row.status} /> },
    { label: "Steps", render: (row) => row.step_count ?? row.steps?.length ?? 0 },
    { label: "Succeeded", render: (row) => row.succeeded_steps ?? 0 },
    { label: "Failed", render: (row) => row.failed_steps ?? 0 },
    { label: "Running", render: (row) => row.running_steps ?? 0 }
  ]} />;
}

function StepTable({ rows }: { rows: NonNullable<Workflow["steps"]> }) {
  return <Table rows={rows} empty="No steps" columns={[
    { label: "Step", render: (row) => row.step_id },
    { label: "Task", render: (row) => row.task_id ? <a data-nav href={`/tasks/${row.task_id}`}>{row.task_id}</a> : "-" },
    { label: "Status", render: (row) => <StatusBadge value={row.status} /> },
    { label: "Attempts", render: (row) => row.attempts ?? 0 },
    { label: "Latency", render: (row) => row.latency_ms ? `${row.latency_ms} ms` : "-" },
    { label: "Error", render: (row) => row.error || "-" }
  ]} />;
}

function ActorTable({ rows }: { rows: Actor[] }) {
  return <Table rows={rows} empty="No actors" columns={[
    { label: "Actor", render: (row) => <a data-nav href={`/actors/${row.actor_id}`}>{row.actor_id}</a> },
    { label: "Class", render: (row) => row.class_name || "-" },
    { label: "Status", render: (row) => <StatusBadge value={row.status} /> },
    { label: "Owner", render: (row) => row.owner_worker_id || "-" },
    { label: "Epoch", render: (row) => row.epoch ?? 0 },
    { label: "Commands", render: (row) => row.command_count ?? 0 }
  ]} />;
}

function ModelTable({ rows }: { rows: ModelInfo[] }) {
  return <Table rows={rows} empty="No models" columns={[
    { label: "Model", render: (row) => `${row.name}:${row.version || "v1"}` },
    { label: "Adapter", render: (row) => row.adapter || "mock" },
    { label: "Size", render: (row) => row.size_bytes ?? "-" },
    { label: "Path", render: (row) => row.path || "-" }
  ]} />;
}

function WorkerTable({ rows }: { rows: Worker[] }) {
  return <Table rows={rows} empty="No workers" columns={[
    { label: "Worker", render: (row) => row.worker_id },
    { label: "Capacity", render: (row) => row.capacity },
    { label: "Running", render: (row) => row.running_tasks },
    { label: "Cached Models", render: (row) => row.cached_models?.map((model) => `${model.name}:${model.version || "v1"}`).join(", ") || "-" },
    { label: "Heartbeat", render: (row) => formatTime(row.last_heartbeat_ms) }
  ]} />;
}

function Table<T>({ rows, columns, empty }: { rows: T[]; columns: Column<T>[]; empty: string }) {
  if (!rows.length) return <div className="empty">{empty}</div>;
  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>{columns.map((column) => <th key={column.label} className={column.className}>{column.label}</th>)}</tr>
        </thead>
        <tbody>
          {rows.map((row, index) => (
            <tr key={index}>{columns.map((column) => <td key={column.label} className={column.className}>{column.render(row)}</td>)}</tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function Kpi({ label, value, tone = "neutral" }: { label: string; value: ReactNode; tone?: "neutral" | "good" | "bad" | "info" }) {
  return (
    <div className={`kpi ${tone}`}>
      <span>{label}</span>
      <strong>{value}</strong>
    </div>
  );
}

function StatusBadge({ value }: { value?: string }) {
  const normalized = value || "UNSPECIFIED";
  let tone = "neutral";
  if (["SUCCEEDED", "COMPLETED", "ACTIVE"].includes(normalized)) tone = "good";
  if (["FAILED", "UNAVAILABLE"].includes(normalized)) tone = "bad";
  if (["RUNNING", "STARTED"].includes(normalized)) tone = "info";
  if (["QUEUED", "SCHEDULED"].includes(normalized)) tone = "warn";
  return <span className={`badge ${tone}`}>{normalized}</span>;
}

function PanelTitle({ title, action }: { title: string; action?: ReactNode }) {
  return (
    <div className="panel-title">
      <h2>{title}</h2>
      {action}
    </div>
  );
}

function JsonViewer({ value }: { value: unknown }) {
  return <pre className="json">{JSON.stringify(value, null, 2)}</pre>;
}

function ErrorPanel({ message }: { message: string }) {
  return <section className="panel error-panel">{message}</section>;
}

function InlineError({ message }: { message: string }) {
  return <div className="inline-error">{message}</div>;
}

function Loading() {
  return <section className="panel">Loading</section>;
}

function DetailGrid({ items }: { items: Array<[string, ReactNode]> }) {
  return (
    <dl className="detail-grid">
      {items.map(([label, value]) => (
        <div key={label}>
          <dt>{label}</dt>
          <dd>{value || "-"}</dd>
        </div>
      ))}
    </dl>
  );
}

function Dag({ steps }: { steps: NonNullable<Workflow["steps"]> }) {
  if (!steps.length) return <div className="empty">No steps</div>;
  return (
    <div className="dag">
      {steps.map((step, index) => (
        <div className="dag-piece" key={step.step_id}>
          {index > 0 && <span className="dag-arrow">{"->"}</span>}
          <div className="dag-node">
            <strong>{step.step_id}</strong>
            <StatusBadge value={step.status} />
          </div>
        </div>
      ))}
    </div>
  );
}

function formatTime(value?: number) {
  if (!value) return "-";
  return new Date(value).toLocaleString();
}

function modelLabel(task: Pick<Task, "llm_model_name" | "llm_model_version">) {
  if (!task.llm_model_name) return "-";
  return `${task.llm_model_name}:${task.llm_model_version || "v1"}`;
}

function defaultID(prefix: string) {
  return `${prefix}-${Date.now()}`;
}

function safePreview(value: string) {
  try {
    return JSON.parse(value || "null");
  } catch {
    return value;
  }
}

function errorMessage(error: unknown) {
  if (error instanceof APIError) return `${error.code}: ${error.message}`;
  if (error instanceof Error) return error.message;
  return String(error);
}
