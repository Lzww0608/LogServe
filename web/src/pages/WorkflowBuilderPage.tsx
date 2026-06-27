import { useState, type FormEvent } from "react";
import { api } from "../api/client";
import { CodeEditor } from "../components/CodeEditor";
import { InlineError } from "../components/ErrorPanel";
import { JsonViewer } from "../components/JsonViewer";
import { workflowTemplate } from "../utils/examples";
import { defaultID } from "../utils/format";
import { navigate } from "../utils/navigation";
import { errorMessage } from "../utils/status";

export function WorkflowBuilderPage() {
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
        <CodeEditor label="Definition JSON" value={definition} onChange={setDefinition} />
      </section>
    </form>
  );
}
