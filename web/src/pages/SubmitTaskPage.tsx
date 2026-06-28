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
import type { ConsoleSession } from "../types/logserve";
import { roleAtLeast } from "../utils/roles";

const sleepSource = `import time

def sleep_ms(ms: int) -> str:
    time.sleep(ms / 1000)
    return f"slept {ms} ms"
`;

const taskTemplates = {
  add: { label: "Add", taskName: "add", functionName: "add", source: addSource, args: "[1, 2]", kwargs: "{}" },
  fail: { label: "Fail", taskName: "fail", functionName: "fail", source: failSource, args: "[]", kwargs: "{}" },
  sleep: { label: "Sleep", taskName: "sleep_ms", functionName: "sleep_ms", source: sleepSource, args: "[250]", kwargs: "{}" }
} as const;

type TemplateID = keyof typeof taskTemplates | "custom";
type FormMessage = { tone: "error" | "info"; text: string };

export function SubmitTaskPage({ session }: { session?: ConsoleSession | null }) {
  const taskParams = new URLSearchParams(window.location.search);
  const initialFunctionHash = taskParams.get("function_hash") ?? "";
  const initialFunctionName = taskParams.get("function_name") ?? "";
  const [mode, setMode] = useState<"source" | "ref" | "hash">(initialFunctionHash ? "hash" : "source");
  const [templateID, setTemplateID] = useState<TemplateID>(initialFunctionHash ? "custom" : "add");
  const [taskName, setTaskName] = useState(initialFunctionName || "add");
  const [functionName, setFunctionName] = useState(initialFunctionName || "add");
  const [source, setSource] = useState(addSource);
  const [functionRef, setFunctionRef] = useState("");
  const [functionHash, setFunctionHash] = useState(initialFunctionHash);
  const [args, setArgs] = useState("[1, 2]");
  const [kwargs, setKwargs] = useState("{}");
  const [idempotencyKey, setIdempotencyKey] = useState(defaultID("ui-task"));
  const [stayAfterSubmit, setStayAfterSubmit] = useState(false);
  const [message, setMessage] = useState<FormMessage | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const canSubmit = roleAtLeast(session, "operator");

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
    function_hash: mode === "ref" || mode === "hash" ? functionHash : "",
    args: validation.parsedArgs ?? safePreview(args),
    kwargs: validation.parsedKwargs ?? safePreview(kwargs),
    idempotency_key: idempotencyKey
  };

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setMessage(null);
    if (!canSubmit) {
      setMessage({ tone: "error", text: "Operator role is required to submit tasks." });
      return;
    }
    if (!validation.valid) {
      setMessage({ tone: "error", text: firstValidationError(validation.errors) ?? "Fix the highlighted fields." });
      return;
    }
    try {
      setSubmitting(true);
      const result = await api.submitTask({
        ...payload,
        task_name: taskName.trim(),
        function_name: functionName.trim(),
        function_ref: mode === "ref" ? functionRef.trim() : "",
        function_hash: mode === "ref" || mode === "hash" ? functionHash.trim() : "",
        args: validation.parsedArgs ?? [],
        kwargs: validation.parsedKwargs ?? {}
      });
      if (stayAfterSubmit) {
        setMessage({ tone: "info", text: `Submitted ${result.task_id}` });
      } else {
        navigate(`/tasks/${result.task_id}`);
      }
    } catch (error) {
      setMessage({ tone: "error", text: errorMessage(error) });
    } finally {
      setSubmitting(false);
    }
  };

  const applyTemplate = (nextTemplateID: TemplateID) => {
    setTemplateID(nextTemplateID);
    if (nextTemplateID === "custom") return;
    const template = taskTemplates[nextTemplateID];
    setMode("source");
    setTaskName(template.taskName);
    setFunctionName(template.functionName);
    setSource(template.source);
    setArgs(template.args);
    setKwargs(template.kwargs);
    setMessage(null);
  };

  const formatJSON = (kind: "args" | "kwargs") => {
    const value = kind === "args" ? args : kwargs;
    const setter = kind === "args" ? setArgs : setKwargs;
    try {
      setter(JSON.stringify(JSON.parse(value || "null"), null, 2));
      setMessage({ tone: "info", text: `${kind === "args" ? "Args" : "Kwargs"} JSON formatted.` });
    } catch (error) {
      setMessage({ tone: "error", text: `${kind === "args" ? "Args" : "Kwargs"} JSON is invalid: ${errorMessage(error)}` });
    }
  };

  const validateJSON = (kind: "args" | "kwargs") => {
    const fieldError = kind === "args" ? validation.errors.args : validation.errors.kwargs;
    setMessage(fieldError ? { tone: "error", text: fieldError } : { tone: "info", text: `${kind === "args" ? "Args" : "Kwargs"} JSON is valid.` });
  };

  return (
    <form className="stack" onSubmit={submit}>
      <section className="panel two-col">
        <div className="form-grid">
          <label>Template<select value={templateID} onChange={(event) => applyTemplate(event.target.value as TemplateID)}>
            {Object.entries(taskTemplates).map(([id, template]) => <option key={id} value={id}>{template.label}</option>)}
            <option value="custom">Custom</option>
          </select></label>
          <label>Mode<select value={mode} onChange={(event) => setMode(event.target.value as "source" | "ref" | "hash")}>
            <option value="source">Python source</option>
            <option value="ref">Function ref</option>
            <option value="hash">Function hash</option>
          </select></label>
          <label>Task name<input value={taskName} onChange={(event) => { setTaskName(event.target.value); setTemplateID("custom"); }} aria-invalid={Boolean(validation.errors.taskName)} className={validation.errors.taskName ? "input-invalid" : undefined} /><FieldError message={validation.errors.taskName} /></label>
          <label>Function name<input value={functionName} onChange={(event) => { setFunctionName(event.target.value); setTemplateID("custom"); }} aria-invalid={Boolean(validation.errors.functionName)} className={validation.errors.functionName ? "input-invalid" : undefined} /><FieldError message={validation.errors.functionName} /></label>
          {mode === "ref" && <div className="workflow-subgrid"><label>Function ref<input value={functionRef} onChange={(event) => setFunctionRef(event.target.value)} aria-invalid={Boolean(validation.errors.functionRef)} className={validation.errors.functionRef ? "input-invalid" : undefined} /><FieldError message={validation.errors.functionRef} /></label><label>Function hash<input value={functionHash} onChange={(event) => setFunctionHash(event.target.value)} aria-invalid={Boolean(validation.errors.functionHash)} className={validation.errors.functionHash ? "input-invalid" : undefined} /><FieldError message={validation.errors.functionHash} /></label></div>}
          {mode === "hash" && <label>Function hash<input value={functionHash} onChange={(event) => setFunctionHash(event.target.value)} aria-invalid={Boolean(validation.errors.functionHash)} className={validation.errors.functionHash ? "input-invalid" : undefined} /><FieldError message={validation.errors.functionHash} /></label>}
          <JSONField label="Args JSON" value={args} error={validation.errors.args} onChange={setArgs} onFormat={() => formatJSON("args")} onValidate={() => validateJSON("args")} />
          <JSONField label="Kwargs JSON" value={kwargs} error={validation.errors.kwargs} onChange={setKwargs} onFormat={() => formatJSON("kwargs")} onValidate={() => validateJSON("kwargs")} />
          <label>Idempotency key<input value={idempotencyKey} onChange={(event) => setIdempotencyKey(event.target.value)} /></label>
          <label className="checkbox-row"><input type="checkbox" checked={stayAfterSubmit} onChange={(event) => setStayAfterSubmit(event.target.checked)} /> Stay on this page after submit</label>
          <div className="button-row">
            <button type="button" className="ghost" onClick={() => applyTemplate("add")}>Add</button>
            <button type="button" className="ghost" onClick={() => applyTemplate("fail")}>Fail</button>
            <button type="button" className="ghost" onClick={() => setIdempotencyKey(defaultID("ui-task"))}>New key</button>
            <button type="button" className="ghost" onClick={() => void copyToClipboard(idempotencyKey)}>Copy key</button>
            <button type="submit" className="primary" disabled={!canSubmit || submitting || !validation.valid}>Submit</button>
          </div>
          <div className="notice warn">Python source submitted here is executed by the worker executor. Use reviewed code or a registered function hash for safer repeated runs.</div>
          {message && (message.tone === "error" ? <InlineError message={message.text} /> : <div className="notice">{message.text}</div>)}
        </div>
        {mode === "source"
          ? <CodeEditor label="Python source" value={source} onChange={(value) => { setSource(value); setTemplateID("custom"); }} error={validation.errors.functionSource} />
          : <div className="notice">Source editor is hidden for function ref/hash mode. The task will call the registered function reference from the fields on the left.</div>}
      </section>
      <section className="panel">
        <h2>Payload</h2>
        <JsonViewer title="Task Payload" value={payload} />
      </section>
    </form>
  );
}

function JSONField({ label, value, error, onChange, onFormat, onValidate }: { label: string; value: string; error?: string; onChange: (value: string) => void; onFormat: () => void; onValidate: () => void }) {
  return (
    <div className="form-grid compact-field">
      <div className="field-action-row">
        <label>{label}</label>
        <div className="button-row">
          <button type="button" className="ghost compact-button" onClick={onFormat}>Format JSON</button>
          <button type="button" className="ghost compact-button" onClick={onValidate}>Validate JSON</button>
        </div>
      </div>
      <textarea className={`short${error ? " input-invalid" : ""}`} value={value} onChange={(event) => onChange(event.target.value)} aria-invalid={Boolean(error)} aria-label={label} />
      <FieldError message={error} />
    </div>
  );
}