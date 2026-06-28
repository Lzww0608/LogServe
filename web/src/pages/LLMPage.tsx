import { useMemo, useState } from "react";
import { api } from "../api/client";
import { FieldError, InlineError } from "../components/ErrorPanel";
import { JsonViewer } from "../components/JsonViewer";
import { PanelTitle } from "../components/PanelTitle";
import { ModelTable } from "../components/domainTables";
import { usePolling } from "../hooks/usePolling";
import type { ConsoleSession, LLMTrace } from "../types/logserve";
import { defaultID } from "../utils/format";
import { copyToClipboard } from "../utils/clipboard";
import { firstValidationError, validateLLMForm } from "../utils/formValidation";
import { roleAtLeast } from "../utils/roles";
import { errorMessage } from "../utils/status";

export function LLMPage({ session }: { session?: ConsoleSession | null }) {
  const modelsState = usePolling(() => api.models(), 1000);
  const [modelName, setModelName] = useState("model-A");
  const [modelVersion, setModelVersion] = useState("v1");
  const [adapter, setAdapter] = useState("mock");
  const [prompt, setPrompt] = useState("Summarize LogServe in one sentence.");
  const [idempotencyKey, setIdempotencyKey] = useState(defaultID("ui-llm"));
  const [taskID, setTaskID] = useState("");
  const [trace, setTrace] = useState<LLMTrace | null>(null);
  const [policy, setPolicy] = useState("LOCALITY_AWARE");
  const [message, setMessage] = useState("");
  const canOperate = roleAtLeast(session, "operator");
  const canAdmin = roleAtLeast(session, "admin");

  const validation = useMemo(() => validateLLMForm(modelName, prompt), [modelName, prompt]);

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
      setTrace(result);
    } catch (error) {
      setMessage(errorMessage(error));
    }
  };

  const replay = async () => {
    if (!taskID.trim()) return;
    setMessage("");
    if (!canOperate) {
      setMessage("Operator role is required to replay LLM traces.");
      return;
    }
    try {
      setTrace(await api.replayLLM(taskID.trim()));
    } catch (error) {
      setMessage(errorMessage(error));
    }
  };

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
        <div>
          <h2>LLM Trace</h2>
          <JsonViewer value={trace ?? null} />
        </div>
      </section>
    </div>
  );
}
