// Visual workflow builder route for editing DAG steps and generating definitions.

import { useMemo, useRef, useState, type FormEvent, type PointerEvent } from "react";
import { api } from "../api/client";
import { FieldError, InlineError } from "../components/ErrorPanel";
import { JsonViewer } from "../components/JsonViewer";
import { PanelTitle } from "../components/PanelTitle";
import { workflowTemplate, workflowTemplates } from "../utils/examples";
import { defaultID } from "../utils/format";
import { copyToClipboard } from "../utils/clipboard";
import { analyzeWorkflowDefinition, firstValidationError, parseJSONField, validateWorkflowForm, type TaskMode } from "../utils/formValidation";
import { navigate } from "../utils/navigation";
import { errorMessage } from "../utils/status";
import type { ConsoleSession } from "../types/logserve";
import { roleAtLeast } from "../utils/roles";

// StepPosition stores canvas coordinates for a draft DAG node.
type StepPosition = { x: number; y: number };

// WorkflowStepDraft is the editable UI model; it intentionally keeps numeric and JSON fields as strings.
type WorkflowStepDraft = {
  uid: string;
  stepId: string;
  taskName: string;
  functionName: string;
  mode: TaskMode;
  functionSource: string;
  functionRef: string;
  functionHash: string;
  argsJson: string;
  dependsOn: string[];
  maxAttempts: string;
  timeoutMs: string;
  llmModelName: string;
  llmModelVersion: string;
  llmAdapter: string;
  llmMaxTokens: string;
  position: StepPosition;
};

// BuilderErrors is keyed by either global field names or `${step.uid}.${field}` for per-step errors.
type BuilderErrors = Record<string, string | undefined>;

// StepDraftErrors is the selected-step projection consumed by the form controls.
type StepDraftErrors = {
  stepId?: string;
  taskName?: string;
  functionName?: string;
  functionSource?: string;
  functionRef?: string;
  functionHash?: string;
  argsJson?: string;
  maxAttempts?: string;
  timeoutMs?: string;
  llmMaxTokens?: string;
};

// Reuse one empty object to avoid allocating placeholder error maps when no step is selected.
const emptyStepDraftErrors: StepDraftErrors = {};

// BuiltWorkflowDefinition mirrors the backend workflow definition JSON emitted by the builder.
type BuiltWorkflowDefinition = {
  workflow_name: string;
  steps: Array<Record<string, unknown>>;
  result_step_id: string;
  max_attempts: number;
  timeout_ms: number;
};

// nextStepUID is a process-local React key source; user-editable step ids cannot serve as stable keys.
let nextStepUID = 0;

// Render the visual workflow DAG editor and generated definition preview.
export function WorkflowBuilderPage({ session }: { session?: ConsoleSession | null }) {
  // Hydrate the template once so stepUID allocation and initial canvas positions stay stable across renders.
  const initialSteps = useMemo(() => stepsFromDefinition(workflowTemplate), []);
  const [workflowName, setWorkflowName] = useState(stringValue(workflowTemplate.workflow_name) || "simple_add");
  const [maxAttempts, setMaxAttempts] = useState(numberText(workflowTemplate.max_attempts) || "3");
  const [timeoutMs, setTimeoutMs] = useState(numberText(workflowTemplate.timeout_ms) || "30000");
  const [steps, setSteps] = useState(initialSteps);
  const [selectedStepUid, setSelectedStepUid] = useState(initialSteps[0]?.uid ?? "");
  const [resultStepId, setResultStepId] = useState(stringValue(workflowTemplate.result_step_id) || initialSteps[0]?.stepId || "");
  const [idempotencyKey, setIdempotencyKey] = useState(defaultID("ui-wf"));
  const [validationResult, setValidationResult] = useState<unknown>(null);
  const [message, setMessage] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [connectFrom, setConnectFrom] = useState(initialSteps[0]?.stepId ?? "");
  const [connectTo, setConnectTo] = useState(initialSteps[1]?.stepId ?? initialSteps[0]?.stepId ?? "");
  const [dragging, setDragging] = useState<{ uid: string; offsetX: number; offsetY: number } | null>(null);
  const canSubmit = roleAtLeast(session, "operator");
  const dagCanvasRef = useRef<HTMLDivElement>(null);

  // Rebuild the emitted definition only when draft inputs change; validation and preview read from this value.
  const built = useMemo(() => buildWorkflowDefinition(workflowName, steps, resultStepId, maxAttempts, timeoutMs), [workflowName, steps, resultStepId, maxAttempts, timeoutMs]);
  const definitionJSON = useMemo(() => JSON.stringify(built.definition, null, 2), [built.definition]);
  const validation = useMemo(() => validateWorkflowForm(workflowName, definitionJSON), [workflowName, definitionJSON]);
  const topology = useMemo(() => analyzeWorkflowDefinition(built.definition), [built.definition]);
  // Merge builder-level errors with JSON-schema validation so the UI can show one first error.
  const allErrors = { ...built.errors, ...validation.errors };
  const firstError = firstValidationError(allErrors);
  const formValid = built.valid && validation.valid;
  const validationPreview = formValid
    ? (validationResult ?? { valid: true, order: topology.order })
    : { valid: false, message: firstError, order: topology.order };
  // Fall back to the first step if the selected uid was removed by template changes or delete actions.
  const currentStep = steps.find((step) => step.uid === selectedStepUid) ?? steps[0];
  const currentErrors = currentStep ? stepErrorsFor(built.errors, currentStep.uid) : emptyStepDraftErrors;
  const stepIDs = steps.map((step) => step.stepId.trim()).filter(Boolean);
  // Connector selects recover from renamed/deleted steps by falling back to currently valid step ids.
  const effectiveConnectFrom = stepIDs.includes(connectFrom) ? connectFrom : stepIDs[0] ?? "";
  const effectiveConnectTo = stepIDs.includes(connectTo) ? connectTo : stepIDs.find((stepID) => stepID !== effectiveConnectFrom) ?? stepIDs[0] ?? "";
  // Prevent self-edges and duplicate dependencies before the backend DAG validator runs.
  const canConnect = Boolean(effectiveConnectFrom && effectiveConnectTo && effectiveConnectFrom !== effectiveConnectTo && !stepByID(steps, effectiveConnectTo)?.dependsOn.includes(effectiveConnectFrom));
  const edges = workflowEdges(steps);

  // Ask the backend validator to normalize the generated workflow definition.
  const validate = async () => {
    setMessage("");
    if (!canSubmit) {
      setValidationResult({ valid: false, message: "Operator role is required to validate workflows.", order: topology.order });
      return;
    }
    if (!formValid) {
      setValidationResult({ valid: false, message: firstError, order: topology.order });
      return;
    }
    try {
      setValidationResult(await api.validateWorkflow({ workflow_name: workflowName.trim(), definition: validation.parsedDefinition }));
    } catch (error) {
      setMessage(errorMessage(error));
    }
  };

  // Submit the generated workflow definition after local validation succeeds.
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setMessage("");
    if (!canSubmit) {
      setMessage("Operator role is required to submit workflows.");
      return;
    }
    if (!formValid) {
      setMessage(firstError);
      return;
    }
    try {
      setSubmitting(true);
      const result = await api.submitWorkflow({ workflow_name: workflowName.trim(), definition: validation.parsedDefinition, idempotency_key: idempotencyKey });
      navigate(`/workflows/${result.workflow_id}`);
    } catch (error) {
      setMessage(errorMessage(error));
    } finally {
      setSubmitting(false);
    }
  };

  // Replace the draft workflow with a built-in template and reset editor selections.
  const applyTemplate = (definition: unknown) => {
    const templateSteps = stepsFromDefinition(definition);
    const record = objectValue(definition);
    setWorkflowName(stringValue(record.workflow_name) || "workflow");
    setMaxAttempts(numberText(record.max_attempts) || "3");
    setTimeoutMs(numberText(record.timeout_ms) || "30000");
    setSteps(templateSteps);
    setSelectedStepUid(templateSteps[0]?.uid ?? "");
    setResultStepId(stringValue(record.result_step_id) || templateSteps.at(-1)?.stepId || "");
    setConnectFrom(templateSteps[0]?.stepId ?? "");
    setConnectTo(templateSteps[1]?.stepId ?? templateSteps[0]?.stepId ?? "");
    setValidationResult(null);
    setMessage("");
  };

  // Patch a draft step and rewrite dependent references when its step id changes.
  const updateStep = (uid: string, patch: Partial<WorkflowStepDraft>) => {
    setSteps((current) => {
      const target = current.find((step) => step.uid === uid);
      const oldStepID = target?.stepId ?? "";
      const next = current.map((step) => step.uid === uid ? { ...step, ...patch } : step);
      // Step ids are user-editable, so dependent draft edges must follow renames.
      if (patch.stepId === undefined || !oldStepID || oldStepID === patch.stepId) return next;
      return next.map((step) => step.uid === uid ? step : { ...step, dependsOn: step.dependsOn.map((dep) => dep === oldStepID ? patch.stepId ?? dep : dep) });
    });
    setValidationResult(null);
  };

  // Rename the selected step and keep result/connector selections aligned.
  const updateSelectedStepID = (value: string) => {
    if (!currentStep) return;
    const oldStepID = currentStep.stepId;
    updateStep(currentStep.uid, { stepId: value });
    setResultStepId((current) => current === oldStepID ? value : current);
    setConnectFrom((current) => current === oldStepID ? value : current);
    setConnectTo((current) => current === oldStepID ? value : current);
  };

  // Append a new draft step and select it for immediate editing.
  const addStep = () => {
    const step = newStepDraft(steps.length);
    setSteps((current) => [...current, step]);
    setSelectedStepUid(step.uid);
    setResultStepId((current) => current || step.stepId);
    setConnectTo(step.stepId);
    setValidationResult(null);
  };

  // Remove the selected draft step and drop dependencies that pointed to it.
  const removeCurrentStep = () => {
    if (!currentStep || steps.length <= 1) return;
    const removedID = currentStep.stepId;
    const remaining = steps.filter((step) => step.uid !== currentStep.uid).map((step) => ({ ...step, dependsOn: step.dependsOn.filter((dep) => dep !== removedID) }));
    setSteps(remaining);
    setSelectedStepUid(remaining[0]?.uid ?? "");
    setResultStepId((current) => current === removedID ? remaining.at(-1)?.stepId ?? "" : current);
    setConnectFrom(remaining[0]?.stepId ?? "");
    setConnectTo(remaining[1]?.stepId ?? remaining[0]?.stepId ?? "");
    setValidationResult(null);
  };

  // Connect two draft steps when the edge is valid and not already present.
  const addDependency = () => {
    if (!canConnect) return;
    const target = stepByID(steps, effectiveConnectTo);
    if (!target) return;
    updateStep(target.uid, { dependsOn: [...target.dependsOn, effectiveConnectFrom] });
  };

  // Remove one dependency edge from the target draft step.
  const removeDependency = (targetStepID: string, dependency: string) => {
    const target = stepByID(steps, targetStepID);
    if (!target) return;
    updateStep(target.uid, { dependsOn: target.dependsOn.filter((dep) => dep !== dependency) });
  };

  // Start node dragging with a pointer offset relative to the DAG canvas.
  const beginDrag = (event: PointerEvent<HTMLButtonElement>, step: WorkflowStepDraft) => {
    const rect = dagCanvasRef.current?.getBoundingClientRect();
    if (!rect) return;
    setSelectedStepUid(step.uid);
    setDragging({ uid: step.uid, offsetX: event.clientX - rect.left - step.position.x, offsetY: event.clientY - rect.top - step.position.y });
    // Capture the pointer so dragging continues even when the cursor leaves the button.
    event.currentTarget.setPointerCapture(event.pointerId);
  };

  // Move the dragged workflow node while clamping it inside the canvas.
  const dragNode = (event: PointerEvent<HTMLButtonElement>) => {
    if (!dragging) return;
    const rect = dagCanvasRef.current?.getBoundingClientRect();
    if (!rect) return;
    // Clamp by the node footprint so the visible button stays inside the canvas.
    const x = Math.max(10, Math.min(rect.width - 170, event.clientX - rect.left - dragging.offsetX));
    const y = Math.max(10, Math.min(rect.height - 102, event.clientY - rect.top - dragging.offsetY));
    updateStep(dragging.uid, { position: { x, y } });
  };

  return (
    <form className="stack" onSubmit={submit}>
      <section className="panel">
        <PanelTitle title="Template Library" />
        <div className="template-grid">
          {workflowTemplates.map((template) => (
            <button type="button" className="ghost" key={template.id} onClick={() => applyTemplate(template.definition)}>{template.label}</button>
          ))}
        </div>
      </section>

      <section className="panel two-col workflow-builder-layout">
        <div className="form-grid">
          <label>Workflow name<input value={workflowName} onChange={(event) => setWorkflowName(event.target.value)} aria-invalid={Boolean(validation.errors.workflowName)} className={validation.errors.workflowName ? "input-invalid" : undefined} /><FieldError message={validation.errors.workflowName} /></label>
          <div className="workflow-subgrid">
            <label>Max attempts<input type="number" min="1" value={maxAttempts} onChange={(event) => setMaxAttempts(event.target.value)} aria-invalid={Boolean(built.errors.maxAttempts)} className={built.errors.maxAttempts ? "input-invalid" : undefined} /><FieldError message={built.errors.maxAttempts} /></label>
            <label>Timeout ms<input type="number" min="1" value={timeoutMs} onChange={(event) => setTimeoutMs(event.target.value)} aria-invalid={Boolean(built.errors.timeoutMs)} className={built.errors.timeoutMs ? "input-invalid" : undefined} /><FieldError message={built.errors.timeoutMs} /></label>
          </div>
          <label>Result step<select value={resultStepId} onChange={(event) => setResultStepId(event.target.value)} aria-invalid={Boolean(validation.errors.definition)}>
            {steps.map((step) => <option key={step.uid} value={step.stepId}>{step.stepId || "unnamed step"}</option>)}
          </select></label>
          <label>Idempotency key<input value={idempotencyKey} onChange={(event) => setIdempotencyKey(event.target.value)} /></label>
          <div className="button-row">
            <button type="button" className="ghost" onClick={validate} disabled={!canSubmit || !formValid}>Validate</button>
            <button type="button" className="ghost" onClick={() => setIdempotencyKey(defaultID("ui-wf"))}>New key</button>
            <button type="button" className="ghost" onClick={() => void copyToClipboard(idempotencyKey)}>Copy key</button>
            <button type="button" className="ghost" onClick={() => void copyToClipboard(definitionJSON)}>Copy JSON</button>
            <button type="submit" className="primary" disabled={!canSubmit || submitting || !formValid}>Submit</button>
          </div>
          {message && <InlineError message={message} />}
          <div>
            <PanelTitle title="Validation" action={<span className={`badge ${formValid ? "good" : "bad"}`}>{formValid ? "Valid" : "Invalid"}</span>} />
            <JsonViewer value={validationPreview} />
          </div>
        </div>
        <div className="workflow-json-preview">
          <PanelTitle title="Definition JSON" />
          <JsonViewer value={built.definition} />
        </div>
      </section>

      <section className="panel split workflow-builder-split">
        <div className="form-grid">
          <PanelTitle title="Step Form" action={<div className="button-row"><button type="button" className="ghost" onClick={addStep}>Add step</button><button type="button" className="ghost" onClick={removeCurrentStep} disabled={steps.length <= 1}>Remove</button></div>} />
          <label>Step<select value={currentStep?.uid ?? ""} onChange={(event) => setSelectedStepUid(event.target.value)}>
            {steps.map((step) => <option key={step.uid} value={step.uid}>{step.stepId || "unnamed step"}</option>)}
          </select></label>
          {currentStep && <>
            <div className="workflow-subgrid">
              <label>Step ID<input value={currentStep.stepId} onChange={(event) => updateSelectedStepID(event.target.value)} aria-invalid={Boolean(currentErrors.stepId)} className={currentErrors.stepId ? "input-invalid" : undefined} /><FieldError message={currentErrors.stepId} /></label>
              <label>Task name<input value={currentStep.taskName} onChange={(event) => updateStep(currentStep.uid, { taskName: event.target.value })} aria-invalid={Boolean(currentErrors.taskName)} className={currentErrors.taskName ? "input-invalid" : undefined} /><FieldError message={currentErrors.taskName} /></label>
              <label>Function name<input value={currentStep.functionName} onChange={(event) => updateStep(currentStep.uid, { functionName: event.target.value })} aria-invalid={Boolean(currentErrors.functionName)} className={currentErrors.functionName ? "input-invalid" : undefined} /><FieldError message={currentErrors.functionName} /></label>
              <label>Source/ref/hash<select value={currentStep.mode} onChange={(event) => updateStep(currentStep.uid, { mode: event.target.value as TaskMode })}>
                <option value="source">Python source</option>
                <option value="ref">Function ref</option>
                <option value="hash">Function hash</option>
              </select></label>
            </div>
            {currentStep.mode === "source" && <label>Python source<textarea value={currentStep.functionSource} onChange={(event) => updateStep(currentStep.uid, { functionSource: event.target.value })} aria-invalid={Boolean(currentErrors.functionSource)} className={`workflow-source${currentErrors.functionSource ? " input-invalid" : ""}`} /><FieldError message={currentErrors.functionSource} /></label>}
            {currentStep.mode === "ref" && <div className="workflow-subgrid"><label>Function ref<input value={currentStep.functionRef} onChange={(event) => updateStep(currentStep.uid, { functionRef: event.target.value })} aria-invalid={Boolean(currentErrors.functionRef)} className={currentErrors.functionRef ? "input-invalid" : undefined} /><FieldError message={currentErrors.functionRef} /></label><label>Function hash<input value={currentStep.functionHash} onChange={(event) => updateStep(currentStep.uid, { functionHash: event.target.value })} aria-invalid={Boolean(currentErrors.functionHash)} className={currentErrors.functionHash ? "input-invalid" : undefined} /><FieldError message={currentErrors.functionHash} /></label></div>}
            {currentStep.mode === "hash" && <label>Function hash<input value={currentStep.functionHash} onChange={(event) => updateStep(currentStep.uid, { functionHash: event.target.value })} aria-invalid={Boolean(currentErrors.functionHash)} className={currentErrors.functionHash ? "input-invalid" : undefined} /><FieldError message={currentErrors.functionHash} /></label>}
            <label>Args JSON<textarea className={`short${currentErrors.argsJson ? " input-invalid" : ""}`} value={currentStep.argsJson} onChange={(event) => updateStep(currentStep.uid, { argsJson: event.target.value })} aria-invalid={Boolean(currentErrors.argsJson)} /><FieldError message={currentErrors.argsJson} /></label>
            <label>Depends on<select multiple className="multi-select" value={currentStep.dependsOn} onChange={(event) => updateStep(currentStep.uid, { dependsOn: selectedValues(event.currentTarget) })}>
              {steps.filter((step) => step.uid !== currentStep.uid && step.stepId.trim()).map((step) => <option key={step.uid} value={step.stepId}>{step.stepId}</option>)}
            </select></label>
            <div className="workflow-subgrid">
              <label>Step max attempts<input type="number" min="1" value={currentStep.maxAttempts} onChange={(event) => updateStep(currentStep.uid, { maxAttempts: event.target.value })} aria-invalid={Boolean(currentErrors.maxAttempts)} className={currentErrors.maxAttempts ? "input-invalid" : undefined} placeholder={maxAttempts} /><FieldError message={currentErrors.maxAttempts} /></label>
              <label>Step timeout ms<input type="number" min="1" value={currentStep.timeoutMs} onChange={(event) => updateStep(currentStep.uid, { timeoutMs: event.target.value })} aria-invalid={Boolean(currentErrors.timeoutMs)} className={currentErrors.timeoutMs ? "input-invalid" : undefined} placeholder={timeoutMs} /><FieldError message={currentErrors.timeoutMs} /></label>
            </div>
            <div className="workflow-subgrid">
              <label>LLM model<input value={currentStep.llmModelName} onChange={(event) => updateStep(currentStep.uid, { llmModelName: event.target.value })} /></label>
              <label>LLM version<input value={currentStep.llmModelVersion} onChange={(event) => updateStep(currentStep.uid, { llmModelVersion: event.target.value })} /></label>
              <label>LLM adapter<input value={currentStep.llmAdapter} onChange={(event) => updateStep(currentStep.uid, { llmAdapter: event.target.value })} /></label>
              <label>LLM max tokens<input type="number" min="1" value={currentStep.llmMaxTokens} onChange={(event) => updateStep(currentStep.uid, { llmMaxTokens: event.target.value })} aria-invalid={Boolean(currentErrors.llmMaxTokens)} className={currentErrors.llmMaxTokens ? "input-invalid" : undefined} /><FieldError message={currentErrors.llmMaxTokens} /></label>
            </div>
          </>}
        </div>

        <div className="workflow-dag-editor">
          <PanelTitle title="DAG Editor" action={<span className={`badge ${topology.valid ? "good" : "bad"}`}>{topology.valid ? "Topological" : "Needs fix"}</span>} />
          <div className="workflow-connector">
            <select value={effectiveConnectFrom} onChange={(event) => setConnectFrom(event.target.value)}>{stepIDs.map((stepID) => <option key={stepID} value={stepID}>{stepID}</option>)}</select>
            <span className="subtle">to</span>
            <select value={effectiveConnectTo} onChange={(event) => setConnectTo(event.target.value)}>{stepIDs.map((stepID) => <option key={stepID} value={stepID}>{stepID}</option>)}</select>
            <button type="button" className="ghost" onClick={addDependency} disabled={!canConnect}>Connect</button>
          </div>
          <div className="workflow-dag-canvas" ref={dagCanvasRef}>
            <svg className="workflow-dag-lines" aria-hidden="true">
              {edges.map((edge) => {
                const from = stepByID(steps, edge.from);
                const to = stepByID(steps, edge.to);
                // Draft edges can briefly reference renamed steps before validation rewrites the selection state.
                if (!from || !to) return null;
                return <line key={`${edge.from}->${edge.to}`} x1={from.position.x + 80} y1={from.position.y + 44} x2={to.position.x + 80} y2={to.position.y + 44} />;
              })}
            </svg>
            {steps.map((step) => (
              <button
                type="button"
                key={step.uid}
                className={`workflow-dag-node${step.uid === currentStep?.uid ? " selected" : ""}`}
                style={{ left: step.position.x, top: step.position.y }}
                onClick={() => setSelectedStepUid(step.uid)}
                onPointerDown={(event) => beginDrag(event, step)}
                onPointerMove={dragNode}
                onPointerUp={() => setDragging(null)}
              >
                <strong>{step.stepId || "unnamed"}</strong>
                <span>{step.taskName || "task"}</span>
                <small>{step.dependsOn.length ? `after ${step.dependsOn.join(", ")}` : "root"}</small>
              </button>
            ))}
          </div>
          <div className="dag-edges">
            {edges.length ? edges.map((edge) => <span key={`${edge.from}-${edge.to}`}>{edge.from} -&gt; {edge.to}<button type="button" className="edge-remove" onClick={() => removeDependency(edge.to, edge.from)}>x</button></span>) : <span>No dependencies</span>}
          </div>
          <div className="topology-order"><span className="subtle">Order</span><code>{topology.order.join(" -> ") || "-"}</code></div>
        </div>
      </section>
    </form>
  );
}

// Create a blank draft step with stable defaults and canvas position.
function newStepDraft(index: number): WorkflowStepDraft {
  const stepID = `step_${index + 1}`;
  return {
    uid: stepUID(),
    stepId: stepID,
    taskName: stepID,
    functionName: stepID,
    mode: "source",
    functionSource: `def ${stepID}():\n    return None\n`,
    functionRef: "",
    functionHash: "",
    argsJson: JSON.stringify({ args: [], kwargs: {} }, null, 2),
    dependsOn: [],
    maxAttempts: "",
    timeoutMs: "",
    llmModelName: "",
    llmModelVersion: "",
    llmAdapter: "",
    llmMaxTokens: "",
    position: defaultPosition(index)
  };
}

// Hydrate editable step drafts from an existing workflow definition.
function stepsFromDefinition(definition: unknown): WorkflowStepDraft[] {
  const record = objectValue(definition);
  const rawSteps = Array.isArray(record.steps) ? record.steps : [];
  const steps = rawSteps.map((rawStep, index) => {
    const step = objectValue(rawStep);
    // Function refs take precedence over hashes because ref mode requires both fields.
    const mode: TaskMode = stringValue(step.function_ref) ? "ref" : stringValue(step.function_hash) ? "hash" : "source";
    return {
      uid: stepUID(),
      stepId: stringValue(step.step_id) || `step_${index + 1}`,
      taskName: stringValue(step.task_name) || stringValue(step.step_id) || `step_${index + 1}`,
      functionName: stringValue(step.function_name) || stringValue(step.task_name) || stringValue(step.step_id) || `step_${index + 1}`,
      mode,
      functionSource: stringValue(step.function_source),
      functionRef: stringValue(step.function_ref),
      functionHash: stringValue(step.function_hash),
      argsJson: JSON.stringify(step.args_json ?? { args: [], kwargs: {} }, null, 2),
      dependsOn: Array.isArray(step.depends_on) ? step.depends_on.map(stringValue).filter(Boolean) : [],
      maxAttempts: numberText(step.max_attempts),
      timeoutMs: numberText(step.timeout_ms),
      llmModelName: stringValue(step.llm_model_name),
      llmModelVersion: stringValue(step.llm_model_version),
      llmAdapter: stringValue(step.llm_adapter),
      llmMaxTokens: numberText(step.llm_max_tokens),
      position: defaultPosition(index)
    } satisfies WorkflowStepDraft;
  });
  return steps.length ? steps : [newStepDraft(0)];
}

// Compile draft UI state into the backend workflow definition plus field errors.
function buildWorkflowDefinition(workflowName: string, steps: WorkflowStepDraft[], resultStepId: string, maxAttemptsText: string, timeoutMsText: string) {
  const errors: BuilderErrors = {};
  const maxAttempts = positiveInt(maxAttemptsText);
  const timeoutMs = positiveInt(timeoutMsText);
  if (maxAttempts === undefined) errors.maxAttempts = "Max attempts must be a positive integer.";
  if (timeoutMs === undefined) errors.timeoutMs = "Timeout ms must be a positive integer.";

  const definition: BuiltWorkflowDefinition = {
    workflow_name: workflowName.trim(),
    steps: steps.map((step) => stepDefinitionFromDraft(step, errors, maxAttempts ?? 3, timeoutMs ?? 30000)),
    result_step_id: resultStepId.trim(),
    max_attempts: maxAttempts ?? 3,
    timeout_ms: timeoutMs ?? 30000
  };
  const analysis = analyzeWorkflowDefinition(definition);
  if (!analysis.valid) errors.definition = analysis.errors[0];
  return { definition, errors, valid: Object.values(errors).every((error) => !error) };
}

// Convert one visual step draft into the API step payload while collecting per-field errors.
function stepDefinitionFromDraft(step: WorkflowStepDraft, errors: BuilderErrors, inheritedAttempts: number, inheritedTimeout: number): Record<string, unknown> {
  // Build the per-step validation key used to map errors back to form fields.
  const key = (field: string) => `${step.uid}.${field}`;
  const stepID = step.stepId.trim();
  const taskName = step.taskName.trim();
  const functionName = step.functionName.trim();
  // LLM steps are identified either by explicit model metadata or the reserved backend function name.
  const isLLMStep = Boolean(step.llmModelName.trim()) || functionName === "__logserve_llm__";
  if (!stepID) errors[key("stepId")] = "Step ID is required.";
  if (!taskName) errors[key("taskName")] = "Task name is required.";
  if (!functionName) errors[key("functionName")] = "Function name is required.";
  if (step.mode === "source" && !isLLMStep && !step.functionSource.trim()) errors[key("functionSource")] = "Function source is required.";
  if (step.mode === "ref" && !step.functionRef.trim()) errors[key("functionRef")] = "Function ref is required.";
  if (step.mode === "ref" && !step.functionHash.trim()) errors[key("functionHash")] = "Function hash is required with function ref.";
  if (step.mode === "hash" && !step.functionHash.trim()) errors[key("functionHash")] = "Function hash is required.";

  const args = parseJSONField<unknown>("Args JSON", step.argsJson, { args: [], kwargs: {} });
  if (!args.valid) errors[key("argsJson")] = args.message;
  const maxAttempts = step.maxAttempts.trim() ? positiveInt(step.maxAttempts) : inheritedAttempts;
  const timeoutMs = step.timeoutMs.trim() ? positiveInt(step.timeoutMs) : inheritedTimeout;
  const llmMaxTokens = step.llmMaxTokens.trim() ? positiveInt(step.llmMaxTokens) : undefined;
  if (maxAttempts === undefined) errors[key("maxAttempts")] = "Step max attempts must be a positive integer.";
  if (timeoutMs === undefined) errors[key("timeoutMs")] = "Step timeout ms must be a positive integer.";
  if (step.llmMaxTokens.trim() && llmMaxTokens === undefined) errors[key("llmMaxTokens")] = "LLM max tokens must be a positive integer.";
  if (isLLMStep && !step.llmModelName.trim()) errors[key("llmModelName")] = "LLM model is required.";

  const payload: Record<string, unknown> = {
    step_id: stepID,
    task_name: taskName,
    function_name: functionName,
    // Preserve invalid args text in the preview payload while field errors block submission.
    args_json: args.valid ? args.value : step.argsJson,
    // Normalize dependencies so duplicate multi-select values cannot produce duplicate edges.
    depends_on: [...new Set(step.dependsOn.map((dep) => dep.trim()).filter(Boolean))],
    max_attempts: maxAttempts ?? inheritedAttempts,
    timeout_ms: timeoutMs ?? inheritedTimeout
  };
  if (step.mode === "source") payload.function_source = step.functionSource;
  if (step.mode === "ref") {
    payload.function_ref = step.functionRef.trim();
    payload.function_hash = step.functionHash.trim();
  }
  if (step.mode === "hash") payload.function_hash = step.functionHash.trim();
  if (step.llmModelName.trim()) payload.llm_model_name = step.llmModelName.trim();
  if (step.llmModelVersion.trim()) payload.llm_model_version = step.llmModelVersion.trim();
  if (step.llmAdapter.trim()) payload.llm_adapter = step.llmAdapter.trim();
  if (llmMaxTokens !== undefined) payload.llm_max_tokens = llmMaxTokens;
  return payload;
}

// Project the flat builder error map into the selected step field errors.
function stepErrorsFor(errors: BuilderErrors, uid: string): StepDraftErrors {
  return {
    stepId: errors[`${uid}.stepId`],
    taskName: errors[`${uid}.taskName`],
    functionName: errors[`${uid}.functionName`],
    functionSource: errors[`${uid}.functionSource`],
    functionRef: errors[`${uid}.functionRef`],
    functionHash: errors[`${uid}.functionHash`],
    argsJson: errors[`${uid}.argsJson`],
    maxAttempts: errors[`${uid}.maxAttempts`],
    timeoutMs: errors[`${uid}.timeoutMs`],
    llmMaxTokens: errors[`${uid}.llmMaxTokens`]
  };
}

// Derive visual DAG edges from each step dependency list.
function workflowEdges(steps: WorkflowStepDraft[]) {
  return steps.flatMap((step) => step.dependsOn.map((dependency) => ({ from: dependency, to: step.stepId })));
}

// Find a draft step by its user-visible step id.
function stepByID(steps: WorkflowStepDraft[], stepID: string): WorkflowStepDraft | undefined {
  return steps.find((step) => step.stepId === stepID);
}

// Read selected dependency ids from the multi-select control.
function selectedValues(select: HTMLSelectElement): string[] {
  return Array.from(select.selectedOptions, (option) => option.value);
}

// Treat only non-array objects as workflow-definition records.
function objectValue(value: unknown): Record<string, unknown> {
  // Template definitions enter as unknown JSON, so narrow once at the read boundary.
  return typeof value === "object" && value !== null && !Array.isArray(value) ? value as Record<string, unknown> : {};
}

// Recover a string field from an unknown workflow-definition value.
function stringValue(value: unknown): string {
  return typeof value === "string" ? value : "";
}

// Convert finite numeric fields back into editable input text.
function numberText(value: unknown): string {
  return typeof value === "number" && Number.isFinite(value) ? String(value) : "";
}

// Parse a positive integer form field or report it as absent.
function positiveInt(value: string): number | undefined {
  const parsed = Number(value);
  return Number.isInteger(parsed) && parsed > 0 ? parsed : undefined;
}

// Place new draft steps on a compact grid in the visual editor.
function defaultPosition(index: number): StepPosition {
  return {
    x: 18 + (index % 3) * 190,
    y: 18 + Math.floor(index / 3) * 112
  };
}

// Generate a process-local React key that survives step id edits.
function stepUID(): string {
  nextStepUID += 1;
  return `wf-step-${nextStepUID}`;
}
