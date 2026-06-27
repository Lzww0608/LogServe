import { useState } from "react";
import { api } from "../api/client";
import { InlineError } from "../components/ErrorPanel";
import { JsonViewer } from "../components/JsonViewer";
import { PanelTitle } from "../components/PanelTitle";
import { ModelTable } from "../components/domainTables";
import { usePolling } from "../hooks/usePolling";
import type { LLMTrace } from "../types/logserve";
import { defaultID } from "../utils/format";
import { errorMessage } from "../utils/status";

export function LLMPage() {
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
