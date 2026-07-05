// LLM route for model registration, request submission, scheduler policy, and replay traces.

import { useMemo, useState } from "react";
import { api } from "../api/client";
import { FieldError, InlineError } from "../components/ErrorPanel";
import { JsonViewer } from "../components/JsonViewer";
import { PanelTitle } from "../components/PanelTitle";
import { StatusBadge } from "../components/StatusBadge";
import { ModelTable } from "../components/domainTables";
import { usePolling } from "../hooks/usePolling";
import type { ConsoleSession, LLMTrace } from "../types/logserve";
import { defaultID } from "../utils/format";
import { copyToClipboard } from "../utils/clipboard";
import { firstValidationError, validateLLMForm } from "../utils/formValidation";
import { roleAtLeast } from "../utils/roles";
import { errorMessage } from "../utils/status";

// Render model registry, LLM submit form, scheduling policy, and replay trace tools.
export function LLMPage({ session }: { session?: ConsoleSession | null }) {
  // Poll model metadata so cache/registration changes made by other sessions become visible.
  const modelsState = usePolling(() => api.models(), 1000);
  const [modelName, setModelName] = useState("model-A");
  const [modelVersion, setModelVersion] = useState("v1");
  const [adapter, setAdapter] = useState("mock");
  const [prompt, setPrompt] = useState("Summarize LogServe in one sentence.");
  const [idempotencyKey, setIdempotencyKey] = useState(defaultID("ui-llm"));
  const [taskID, setTaskID] = useState("");
  const [trace, setTrace] = useState<LLMTrace | null>(null);
  const [submittedTrace, setSubmittedTrace] = useState<LLMTrace | null>(null);
  const [policy, setPolicy] = useState("LOCALITY_AWARE");
  const [message, setMessage] = useState("");
  // Model submission/replay is operator-level, while model registration is admin-only.
  const canOperate = roleAtLeast(session, "operator");
  const canAdmin = roleAtLeast(session, "admin");

  const validation = useMemo(() => validateLLMForm(modelName, prompt), [modelName, prompt]);

  // Register a model descriptor through the admin-only backend path.
  const register = async () => {
    setMessage("");
    if (!canAdmin) {
      setMessage("Admin role is required to register models.");
      return;
    }
    if (validation.errors.modelName) {
      setMessage(validation.errors.modelName);
      return;
    }
    try {
      await api.registerModel({ name: modelName.trim(), version: modelVersion, adapter, path: `/models/${modelName.trim()}` });
    } catch (error) {
      setMessage(errorMessage(error));
    }
  };

  // Submit an LLM prompt and keep the returned task metadata visible for replay.
  const submit = async () => {
    setMessage("");
    if (!canOperate) {
      setMessage("Operator role is required to submit LLM requests.");
      return;
    }
    if (!validation.valid) {
      setMessage(firstValidationError(validation.errors));
      return;
    }
    try {
      const result = await api.submitLLM({
        model_name: modelName.trim(),
        model_version: modelVersion,
        adapter,
        prompt: prompt.trim(),
        max_tokens: 64,
        idempotency_key: idempotencyKey
      });
      if (result.task_id) setTaskID(result.task_id);
      setSubmittedTrace(result);
      // Submit can return only an acknowledgement; defer trace cards until timing fields exist.
      setTrace(hasTimingTrace(result) ? result : null);
    } catch (error) {
      setMessage(errorMessage(error));
    }
  };

  // Replay an LLM task and render its event timeline alongside raw metadata.
  const replay = async () => {
    if (!taskID.trim()) return;
    setMessage("");
    if (!canOperate) {
      setMessage("Operator role is required to replay LLM traces.");
      return;
    }
    try {
      const result = await api.replayLLM(taskID.trim());
      setTrace(result);
      setSubmittedTrace(null);
    } catch (error) {
      setMessage(errorMessage(error));
    }
  };

  // Submit the selected scheduling policy through the operator control path.
  const setSchedulingPolicy = async () => {
    setMessage("");
    if (!canOperate) {
      setMessage("Operator role is required to set scheduling policy.");
      return;
    }
    try {
      setMessage((await api.setSchedulingPolicy(policy)).policy);
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
          <label>Model name<input value={modelName} onChange={(event) => setModelName(event.target.value)} aria-invalid={Boolean(validation.errors.modelName)} className={validation.errors.modelName ? "input-invalid" : undefined} /><FieldError message={validation.errors.modelName} /></label>
          <label>Version<input value={modelVersion} onChange={(event) => setModelVersion(event.target.value)} /></label>
          <label>Adapter<input value={adapter} onChange={(event) => setAdapter(event.target.value)} /></label>
          <label>Scheduling<select value={policy} onChange={(event) => setPolicy(event.target.value)}>
            <option>LOCALITY_AWARE</option>
            <option>RESOURCE_ONLY</option>
            <option>PREDICTED_LATENCY</option>
          </select></label>
          <div className="button-row">
            <button type="button" className="ghost" onClick={register} disabled={!canAdmin || Boolean(validation.errors.modelName)}>Register</button>
            <button type="button" className="ghost" onClick={() => void setSchedulingPolicy()} disabled={!canOperate}>Set Policy</button>
          </div>
        </div>
      </section>
      <section className="panel two-col">
        <div className="form-grid">
          <label>Prompt<textarea value={prompt} onChange={(event) => setPrompt(event.target.value)} aria-invalid={Boolean(validation.errors.prompt)} className={validation.errors.prompt ? "input-invalid" : undefined} /><FieldError message={validation.errors.prompt} /></label>
          <label>Idempotency key<input value={idempotencyKey} onChange={(event) => setIdempotencyKey(event.target.value)} /></label>
          <label>Trace task<input value={taskID} onChange={(event) => setTaskID(event.target.value)} /></label>
          <div className="button-row">
            <button type="button" className="primary" onClick={submit} disabled={!canOperate || !validation.valid}>Submit</button>
            <button type="button" className="ghost" onClick={() => setIdempotencyKey(defaultID("ui-llm"))}>New key</button>
            <button type="button" className="ghost" onClick={() => void copyToClipboard(idempotencyKey)}>Copy key</button>
            <button type="button" className="ghost" onClick={replay} disabled={!canOperate || !taskID.trim()}>Replay</button>
          </div>
          {message && <InlineError message={message} />}
        </div>
        <LLMTracePanel trace={trace} submitted={submittedTrace} />
      </section>
    </div>
  );
}

// Render replay output with trace cards and a raw JSON fallback.
function LLMTracePanel({ trace, submitted }: { trace: LLMTrace | null; submitted: LLMTrace | null }) {
  if (!trace) {
    return (
      <div>
        <PanelTitle title="LLM Trace" action={submitted?.status ? <StatusBadge value={submitted.status} /> : undefined} />
        {submitted ? (
          <div className="trace-result">
            <span className="subtle">Submitted task</span>
            <strong>{submitted.task_id || "pending"}</strong>
            <p className="subtle">Submit confirms the task. Replay the task after it completes to inspect cache, timing, and timeline events.</p>
          </div>
        ) : <div className="empty empty-state">No trace yet. Submit a request or replay an existing trace task to inspect model timing.</div>}
      </div>
    );
  }
  const resultText = traceResultText(trace);
  return (
    <div className="stack">
      <PanelTitle title="LLM Trace" action={<StatusBadge value={trace.status} />} />
      <div className="trace-summary-grid">
        <TraceCard label="Cache hit" value={trace.cache_hit === undefined ? "-" : trace.cache_hit ? "yes" : "no"} />
        <TraceCard label="Worker" value={trace.worker_id || "-"} />
        <TraceCard label="Model load" value={ms(trace.model_load_ms)} />
        <TraceCard label="Checkpoint fetch" value={ms(trace.checkpoint_fetch_ms)} />
        <TraceCard label="First token" value={ms(trace.first_token_ms)} />
        <TraceCard label="Total latency" value={ms(trace.total_latency_ms)} />
      </div>
      {resultText && <div className="trace-result"><span className="subtle">Result</span><strong>{resultText}</strong></div>}
      <div className="trace-result">
        <div className="panel-title"><h2>Timeline</h2><span className="badge info">{trace.events?.length ?? 0} events</span></div>
        {trace.events?.length ? <div className="trace-timeline">{trace.events.map((event, index) => <TimelineItem key={index} event={event} />)}</div> : <div className="empty">No timeline events in this trace.</div>}
      </div>
      <JsonViewer title="Raw Trace JSON" value={trace} collapsible defaultCollapsed />
    </div>
  );
}

// Render one grouped replay trace section with stable summary fields.
function TraceCard({ label, value }: { label: string; value: string }) {
  return <div className="trace-card"><span>{label}</span><strong>{value}</strong></div>;
}

// Render one replay event with a compact label plus raw event payload.
function TimelineItem({ event }: { event: Record<string, unknown> }) {
  return (
    <div className="timeline-item">
      <div><strong>{eventLabel(event)}</strong><div className="subtle">{eventDetail(event)}</div></div>
    </div>
  );
}

// Choose the most descriptive event-name field from a flexible replay event object.
function eventLabel(event: Record<string, unknown>): string {
  for (const key of ["event_type", "type", "name", "event"]) {
    const value = event[key];
    if (typeof value === "string" && value.trim()) return value;
  }
  return "Trace event";
}

// Summarize stable trace fields before falling back to raw JSON.
function eventDetail(event: Record<string, unknown>): string {
  const parts = ["worker_id", "model_name", "timestamp_ms", "latency_ms"]
    .map((key) => event[key] === undefined ? "" : `${key}: ${String(event[key])}`)
    .filter(Boolean);
  return parts.join(" | ") || JSON.stringify(event);
}

// Extract display text from either string results or {text} JSON results.
function traceResultText(trace: LLMTrace): string {
  const result = trace.result_json;
  if (typeof result === "string") return result;
  if (typeof result === "object" && result !== null && "text" in result) {
    // Narrow only the optional text field; other result JSON shapes remain valid raw trace data.
    const text = (result as { text?: unknown }).text;
    return typeof text === "string" ? text : "";
  }
  return "";
}

// Format an optional duration in milliseconds for trace summary cards.
function ms(value?: number): string {
  return value === undefined ? "-" : `${value} ms`;
}
// Distinguish a completed replay trace from a submit acknowledgement.
function hasTimingTrace(trace: LLMTrace): boolean {
  return Boolean(trace.events?.length) || trace.cache_hit !== undefined || trace.model_load_ms !== undefined || trace.checkpoint_fetch_ms !== undefined || trace.first_token_ms !== undefined || trace.total_latency_ms !== undefined;
}