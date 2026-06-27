import { useState, type FormEvent } from "react";
import { api } from "../api/client";
import { CodeEditor } from "../components/CodeEditor";
import { InlineError } from "../components/ErrorPanel";
import { JsonViewer } from "../components/JsonViewer";
import { navigate } from "../utils/navigation";
import { addSource, failSource } from "../utils/examples";
import { defaultID, safePreview } from "../utils/format";
import { errorMessage } from "../utils/status";

export function SubmitTaskPage() {
  const taskParams = new URLSearchParams(window.location.search);
  const initialFunctionHash = taskParams.get("function_hash") ?? "";
  const initialFunctionName = taskParams.get("function_name") ?? "";
  const [mode, setMode] = useState<"source" | "ref" | "hash">(initialFunctionHash ? "hash" : "source");
  const [taskName, setTaskName] = useState(initialFunctionName || "add");
  const [functionName, setFunctionName] = useState(initialFunctionName || "add");
  const [source, setSource] = useState(addSource);
  const [functionRef, setFunctionRef] = useState("");
  const [functionHash, setFunctionHash] = useState(initialFunctionHash);
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
        <CodeEditor label="Python source" value={source} onChange={setSource} disabled={mode !== "source"} />
      </section>
      <section className="panel">
        <h2>Payload</h2>
        <JsonViewer value={payload} />
      </section>
    </form>
  );
}
