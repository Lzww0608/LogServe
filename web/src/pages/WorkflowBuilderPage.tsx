import { useMemo, useState, type FormEvent } from "react";
import { api } from "../api/client";
import { CodeEditor } from "../components/CodeEditor";
import { FieldError, InlineError } from "../components/ErrorPanel";
import { JsonViewer } from "../components/JsonViewer";
import { workflowTemplate } from "../utils/examples";
import { defaultID } from "../utils/format";
import { copyToClipboard } from "../utils/clipboard";
import { firstValidationError, validateWorkflowForm } from "../utils/formValidation";
import { navigate } from "../utils/navigation";
import { errorMessage } from "../utils/status";

export function WorkflowBuilderPage() {
  const [workflowName, setWorkflowName] = useState("simple_add");
  const [definition, setDefinition] = useState(JSON.stringify(workflowTemplate, null, 2));
  const [idempotencyKey, setIdempotencyKey] = useState(defaultID("ui-wf"));
  const [validationResult, setValidationResult] = useState<unknown>(null);
  const [message, setMessage] = useState("");

  const validation = useMemo(() => validateWorkflowForm(workflowName, definition), [workflowName, definition]);
  const validationPreview = validation.valid
    ? (validationResult ?? { valid: true })
    : { valid: false, message: firstValidationError(validation.errors) };

  const validate = async () => {
    setMessage("");
    if (!validation.valid) {
      setValidationResult({ valid: false, message: firstValidationError(validation.errors) });
      return;
    }
    try {
      setValidationResult(await api.validateWorkflow({ workflow_name: workflowName.trim(), definition: validation.parsedDefinition }));
    } catch (error) {
      setMessage(errorMessage(error));
    }
  };

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setMessage("");
    if (!validation.valid) {
      setMessage(firstValidationError(validation.errors));
      return;
    }
    try {
      const result = await api.submitWorkflow({ workflow_name: workflowName.trim(), definition: validation.parsedDefinition, idempotency_key: idempotencyKey });
      navigate(`/workflows/${result.workflow_id}`);
    } catch (error) {
      setMessage(errorMessage(error));
    }
  };

  return (
    <form className="stack" onSubmit={submit}>
      <section className="panel two-col">
        <div className="form-grid">
          <label>Workflow name<input value={workflowName} onChange={(event) => setWorkflowName(event.target.value)} aria-invalid={Boolean(validation.errors.workflowName)} className={validation.errors.workflowName ? "input-invalid" : undefined} /><FieldError message={validation.errors.workflowName} /></label>
          <label>Idempotency key<input value={idempotencyKey} onChange={(event) => setIdempotencyKey(event.target.value)} /></label>
          <div className="button-row">
            <button type="button" className="ghost" onClick={validate} disabled={!validation.valid}>Validate</button>
            <button type="button" className="ghost" onClick={() => setIdempotencyKey(defaultID("ui-wf"))}>New key</button>
            <button type="button" className="ghost" onClick={() => void copyToClipboard(idempotencyKey)}>Copy key</button>
            <button type="submit" className="primary" disabled={!validation.valid}>Submit</button>
          </div>
          {message && <InlineError message={message} />}
          <div>
            <h2>Validation</h2>
            <JsonViewer value={validationPreview} />
          </div>
        </div>
        <CodeEditor label="Definition JSON" value={definition} onChange={setDefinition} error={validation.errors.definition} />
      </section>
    </form>
  );
}
