import { useMemo, useState, type FormEvent } from "react";
import { api } from "../api/client";
import { CodeEditor } from "../components/CodeEditor";
import { FieldError, InlineError } from "../components/ErrorPanel";
import { JsonViewer } from "../components/JsonViewer";
import { navigate } from "../utils/navigation";
import { addSource, failSource } from "../utils/examples";
import { defaultID, safePreview } from "../utils/format";
import { copyToClipboard } from "../utils/clipboard";
import { firstValidationError, validateTaskForm } from "../utils/formValidation";
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

  const validation = useMemo(() => validateTaskForm({
    mode,
    taskName,
    functionName,
    functionSource: source,
    functionRef,
    functionHash,
    argsText: args,
    kwargsText: kwargs
  }), [mode, taskName, functionName, source, functionRef, functionHash, args, kwargs]);

  const payload = {
    task_name: taskName,
    function_name: functionName,
    function_source: mode === "source" ? source : "",
    function_ref: mode === "ref" ? functionRef : "",
    function_hash: mode === "hash" ? functionHash : "",
    args: validation.parsedArgs ?? safePreview(args),
    kwargs: validation.parsedKwargs ?? safePreview(kwargs),
    idempotency_key: idempotencyKey
  };

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setMessage("");
    if (!validation.valid) {
      setMessage(firstValidationError(validation.errors));
      return;
    }
    try {
      setSubmitting(true);
      const result = await api.submitTask({
        ...payload,
        task_name: taskName.trim(),
        function_name: functionName.trim(),
        function_ref: mode === "ref" ? functionRef.trim() : "",
        function_hash: mode === "hash" ? functionHash.trim() : "",
        args: validation.parsedArgs ?? [],
        kwargs: validation.parsedKwargs ?? {}
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
          <label>Task name<input value={taskName} onChange={(event) => setTaskName(event.target.value)} aria-invalid={Boolean(validation.errors.taskName)} className={validation.errors.taskName ? "input-invalid" : undefined} /><FieldError message={validation.errors.taskName} /></label>
          <label>Function name<input value={functionName} onChange={(event) => setFunctionName(event.target.value)} aria-invalid={Boolean(validation.errors.functionName)} className={validation.errors.functionName ? "input-invalid" : undefined} /><FieldError message={validation.errors.functionName} /></label>
          {mode === "ref" && <label>Function ref<input value={functionRef} onChange={(event) => setFunctionRef(event.target.value)} aria-invalid={Boolean(validation.errors.functionRef)} className={validation.errors.functionRef ? "input-invalid" : undefined} /><FieldError message={validation.errors.functionRef} /></label>}
          {mode === "hash" && <label>Function hash<input value={functionHash} onChange={(event) => setFunctionHash(event.target.value)} aria-invalid={Boolean(validation.errors.functionHash)} className={validation.errors.functionHash ? "input-invalid" : undefined} /><FieldError message={validation.errors.functionHash} /></label>}
          <label>Args JSON<textarea className={`short${validation.errors.args ? " input-invalid" : ""}`} value={args} onChange={(event) => setArgs(event.target.value)} aria-invalid={Boolean(validation.errors.args)} /><FieldError message={validation.errors.args} /></label>
          <label>Kwargs JSON<textarea className={`short${validation.errors.kwargs ? " input-invalid" : ""}`} value={kwargs} onChange={(event) => setKwargs(event.target.value)} aria-invalid={Boolean(validation.errors.kwargs)} /><FieldError message={validation.errors.kwargs} /></label>
          <label>Idempotency key<input value={idempotencyKey} onChange={(event) => setIdempotencyKey(event.target.value)} /></label>
          <div className="button-row">
            <button type="button" className="ghost" onClick={() => setSource(addSource)}>Add</button>
            <button type="button" className="ghost" onClick={() => setSource(failSource)}>Fail</button>
            <button type="button" className="ghost" onClick={() => setIdempotencyKey(defaultID("ui-task"))}>New key</button>
            <button type="button" className="ghost" onClick={() => void copyToClipboard(idempotencyKey)}>Copy key</button>
            <button type="submit" className="primary" disabled={submitting || !validation.valid}>Submit</button>
          </div>
          {message && <InlineError message={message} />}
        </div>
        <CodeEditor label="Python source" value={source} onChange={setSource} disabled={mode !== "source"} error={mode === "source" ? validation.errors.functionSource : undefined} />
      </section>
      <section className="panel">
        <h2>Payload</h2>
        <JsonViewer value={payload} />
      </section>
    </form>
  );
}
