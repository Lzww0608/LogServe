// Form and workflow-definition validation shared by submit pages and unit tests.

// TaskMode selects how the task form identifies executable Python source.
export type TaskMode = "source" | "ref" | "hash";

// FieldErrors stores optional per-field messages while preserving stable field keys.
type FieldErrors = Record<string, string | undefined>;

// JSONFieldResult lets callers keep parsed values only when validation succeeded.
export type JSONFieldResult<T> =
  | { valid: true; value: T }
  | { valid: false; message: string };

// TaskFormInput is the raw string state captured by the submit-task page.
export interface TaskFormInput {
  mode: TaskMode;
  taskName: string;
  functionName: string;
  functionSource: string;
  functionRef: string;
  functionHash: string;
  argsText: string;
  kwargsText: string;
}

// TaskFormValidation returns both display errors and parsed JSON envelopes.
export interface TaskFormValidation {
  valid: boolean;
  errors: {
    taskName?: string;
    functionName?: string;
    functionSource?: string;
    functionRef?: string;
    functionHash?: string;
    args?: string;
    kwargs?: string;
  };
  parsedArgs?: unknown[];
  parsedKwargs?: Record<string, unknown>;
}

// WorkflowFormValidation combines outer form checks with parsed workflow JSON.
export interface WorkflowFormValidation {
  valid: boolean;
  errors: {
    workflowName?: string;
    definition?: string;
  };
  parsedDefinition?: unknown;
}

// WorkflowDefinitionAnalysis describes local DAG checks before backend validation.
export interface WorkflowDefinitionAnalysis {
  valid: boolean;
  errors: string[];
  order: string[];
}

// ArgsFormValidation is shared by actor create/call forms that parse positional args.
export interface ArgsFormValidation {
  valid: boolean;
  errors: {
    args?: string;
  };
  parsedArgs?: unknown[];
}

// ActorCreateFormValidation adds class identity/source checks to init args validation.
export interface ActorCreateFormValidation extends ArgsFormValidation {
  errors: ArgsFormValidation["errors"] & {
    className?: string;
    classSource?: string;
  };
}

// ActorCallFormValidation adds method-name checks to args validation.
export interface ActorCallFormValidation extends ArgsFormValidation {
  errors: ArgsFormValidation["errors"] & {
    method?: string;
  };
}

// LLMFormValidation covers the minimum fields required before an LLM submit.
export interface LLMFormValidation {
  valid: boolean;
  errors: {
    modelName?: string;
    prompt?: string;
  };
}

// Parse a JSON text field, treating blank input as the provided fallback value.
export function parseJSONField<T>(fieldName: string, text: string, fallback: T): JSONFieldResult<T> {
  // Stringify fallback so blank fields still pass through JSON.parse and keep one error path.
  const source = text.trim() === "" ? JSON.stringify(fallback) : text;
  try {
    return { valid: true, value: JSON.parse(source) as T };
  } catch (error) {
    return { valid: false, message: `${fieldName}: ${jsonErrorDetail(error, source)}` };
  }
}

// Validate task submission mode, function identity, and JSON argument envelopes.
export function validateTaskForm(input: TaskFormInput): TaskFormValidation {
  const errors: TaskFormValidation["errors"] = {};
  if (!input.taskName.trim()) errors.taskName = "Task name is required.";
  if (!input.functionName.trim()) errors.functionName = "Function name is required.";
  if (input.mode === "source" && !input.functionSource.trim()) errors.functionSource = "Function source is required.";
  if (input.mode === "ref" && !input.functionRef.trim()) errors.functionRef = "Function ref is required.";
  // Function refs are paired with hashes so the backend can verify the referenced source identity.
  if (input.mode === "ref" && !input.functionHash.trim()) errors.functionHash = "Function hash is required with function ref.";
  if (input.mode === "hash" && !input.functionHash.trim()) errors.functionHash = "Function hash is required.";

  // Args and kwargs are parsed even when required text fields fail, letting the UI show all errors at once.
  const args = parseArgsField(input.argsText);
  if (!args.valid) errors.args = args.message;

  const kwargs = parseKwargsField(input.kwargsText);
  if (!kwargs.valid) errors.kwargs = kwargs.message;

  return {
    valid: noErrors(errors),
    errors,
    parsedArgs: args.valid ? args.value : undefined,
    parsedKwargs: kwargs.valid ? kwargs.value : undefined
  };
}

// Validate workflow name plus JSON definition before backend validation.
export function validateWorkflowForm(workflowName: string, definitionText: string): WorkflowFormValidation {
  const errors: WorkflowFormValidation["errors"] = {};
  if (!workflowName.trim()) errors.workflowName = "Workflow name is required.";
  let parsedDefinition: unknown;
  if (!definitionText.trim()) {
    errors.definition = "Workflow definition JSON is required.";
  } else {
    const definition = parseJSONField<unknown>("Workflow definition JSON", definitionText, {});
    if (definition.valid) {
      parsedDefinition = definition.value;
      // Local DAG validation catches structural mistakes before the request reaches backend validation.
      const analysis = analyzeWorkflowDefinition(definition.value);
      if (!analysis.valid) errors.definition = analysis.errors[0];
    } else {
      errors.definition = definition.message;
    }
  }
  return { valid: noErrors(errors), errors, parsedDefinition };
}

// Check workflow DAG shape, dependencies, result step, and topological order locally.
export function analyzeWorkflowDefinition(definition: unknown): WorkflowDefinitionAnalysis {
  const errors: string[] = [];
  if (!isPlainObject(definition)) {
    return { valid: false, errors: ["Workflow definition must be a JSON object."], order: [] };
  }

  const rawSteps = definition.steps;
  if (!Array.isArray(rawSteps) || rawSteps.length === 0) {
    return { valid: false, errors: ["Workflow must contain at least one step."], order: [] };
  }

  const stepIDs: string[] = [];
  const stepSet = new Set<string>();
  // Track duplicates separately so every step can still be inspected for additional errors.
  const duplicateSet = new Set<string>();
  const dependencies = new Map<string, string[]>();

  rawSteps.forEach((rawStep, index) => {
    if (!isPlainObject(rawStep)) {
      errors.push(`Step ${index + 1} must be a JSON object.`);
      return;
    }

    // Convert unknown JSON values through stringField so non-string ids become validation errors, not crashes.
    const stepID = stringField(rawStep.step_id);
    const label = stepID || `step ${index + 1}`;
    if (!stepID) {
      errors.push(`Step ${index + 1} requires step_id.`);
    } else if (stepSet.has(stepID)) {
      duplicateSet.add(stepID);
    } else {
      stepSet.add(stepID);
      stepIDs.push(stepID);
    }

    const functionName = stringField(rawStep.function_name);
    // Synthetic LLM steps do not need Python source but must declare a model name.
    const isLLMStep = Boolean(stringField(rawStep.llm_model_name)) || functionName === "__logserve_llm__";
    if (!stringField(rawStep.task_name)) errors.push(`Step ${label} requires task_name.`);
    if (!functionName) errors.push(`Step ${label} requires function_name.`);
    if (!isLLMStep && !stringField(rawStep.function_source) && !stringField(rawStep.function_ref) && !stringField(rawStep.function_hash)) {
      errors.push(`Step ${label} requires function_source, function_ref, or function_hash.`);
    }
    if (isLLMStep && !stringField(rawStep.llm_model_name)) errors.push(`Step ${label} requires llm_model_name.`);

    // Numeric options are optional, but supplied values must be positive integers accepted by the API.
    if (rawStep.max_attempts !== undefined && !positiveInteger(rawStep.max_attempts)) {
      errors.push(`Step ${label} max_attempts must be a positive integer.`);
    }
    if (rawStep.timeout_ms !== undefined && !positiveInteger(rawStep.timeout_ms)) {
      errors.push(`Step ${label} timeout_ms must be a positive integer.`);
    }
    if (rawStep.llm_max_tokens !== undefined && !positiveInteger(rawStep.llm_max_tokens)) {
      errors.push(`Step ${label} llm_max_tokens must be a positive integer.`);
    }

    const dependsOn = rawStep.depends_on;
    if (dependsOn === undefined) {
      if (stepID && !dependencies.has(stepID)) dependencies.set(stepID, []);
    } else if (!Array.isArray(dependsOn)) {
      errors.push(`Step ${label} depends_on must be an array.`);
      if (stepID && !dependencies.has(stepID)) dependencies.set(stepID, []);
    } else {
      // Drop blank dependencies after reporting type errors so graph checks only see usable ids.
      const deps = dependsOn.map((dep) => stringField(dep)).filter((dep) => dep !== "");
      if (deps.length !== dependsOn.length) errors.push(`Step ${label} depends_on entries must be strings.`);
      if (stepID && !dependencies.has(stepID)) dependencies.set(stepID, [...new Set(deps)]);
    }
  });

  for (const duplicate of duplicateSet) {
    errors.push(`Duplicate workflow step_id "${duplicate}".`);
  }

  // Result step validation runs after scanning steps so duplicates and unknown ids are all reported together.
  const resultStepID = stringField(definition.result_step_id);
  if (!resultStepID) {
    errors.push("result_step_id is required.");
  } else if (!stepSet.has(resultStepID)) {
    errors.push(`result_step_id "${resultStepID}" does not match any step.`);
  }

  for (const [stepID, deps] of dependencies) {
    // Dependency existence is checked after all steps are scanned so forward references are valid.
    if (!stepID) continue;
    // Unknown dependencies are skipped here; existence errors are reported before topological sorting.
    for (const dep of deps) {
      if (dep === stepID) {
        errors.push(`Step ${stepID} cannot depend on itself.`);
      } else if (!stepSet.has(dep)) {
        errors.push(`Step ${stepID} depends on unknown step "${dep}".`);
      }
    }
  }

  // A shortened topological order is the cycle signal used by this lightweight validator.
  const order = topologicalStepOrder(stepIDs, dependencies);
  if (stepIDs.length > 0 && order.length !== stepIDs.length) {
    errors.push("Workflow dependency cycle detected.");
  }

  return { valid: errors.length === 0, errors, order };
}

// Validate actor creation fields and init-args JSON.
export function validateActorCreateForm(className: string, classSource: string, initArgsText: string): ActorCreateFormValidation {
  const errors: ActorCreateFormValidation["errors"] = {};
  if (!className.trim()) errors.className = "Class name is required.";
  if (!classSource.trim()) errors.classSource = "Class source is required.";
  const args = parseArgsField(initArgsText, "Init args JSON");
  if (!args.valid) errors.args = args.message;
  return { valid: noErrors(errors), errors, parsedArgs: args.valid ? args.value : undefined };
}

// Validate actor method invocation fields and args JSON.
export function validateActorCallForm(method: string, argsText: string): ActorCallFormValidation {
  const errors: ActorCallFormValidation["errors"] = {};
  if (!method.trim()) errors.method = "Actor method is required.";
  const args = parseArgsField(argsText);
  if (!args.valid) errors.args = args.message;
  return { valid: noErrors(errors), errors, parsedArgs: args.valid ? args.value : undefined };
}

// Validate the minimum LLM request fields before submit.
export function validateLLMForm(modelName: string, prompt: string): LLMFormValidation {
  const errors: LLMFormValidation["errors"] = {};
  if (!modelName.trim()) errors.modelName = "Model name is required.";
  if (!prompt.trim()) errors.prompt = "Prompt is required.";
  return { valid: noErrors(errors), errors };
}

// Return the first visible validation error for compact form feedback.
export function firstValidationError(errors: FieldErrors): string {
  return Object.values(errors).find((message): message is string => Boolean(message)) ?? "Fix highlighted fields before submitting.";
}

// Parse a JSON array field used for positional args.
function parseArgsField(text: string, fieldName = "Args JSON"): JSONFieldResult<unknown[]> {
  // Blank args fields become [] through parseJSONField so simple calls need no boilerplate JSON.
  const args = parseJSONField<unknown>(fieldName, text, []);
  if (!args.valid) return args;
  if (!Array.isArray(args.value)) return { valid: false, message: `${fieldName}: expected a JSON array.` };
  return { valid: true, value: args.value };
}

// Parse a JSON object field used for keyword args.
function parseKwargsField(text: string): JSONFieldResult<Record<string, unknown>> {
  // Blank kwargs fields become {} to match the executor envelope default.
  const kwargs = parseJSONField<unknown>("Kwargs JSON", text, {});
  if (!kwargs.valid) return kwargs;
  if (!isPlainObject(kwargs.value)) return { valid: false, message: "Kwargs JSON: expected a JSON object." };
  return { valid: true, value: kwargs.value };
}

// Accept only non-null, non-array objects for JSON object fields.
function isPlainObject(value: unknown): value is Record<string, unknown> {
  // Arrays are objects in JavaScript, so exclude them for JSON object form fields.
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

// Normalize unknown values into trimmed strings for workflow validation.
function stringField(value: unknown): string {
  // Non-string values intentionally normalize to empty strings and trigger required-field errors.
  return typeof value === "string" ? value.trim() : "";
}

// Check that a workflow numeric option is a positive integer.
function positiveInteger(value: unknown): boolean {
  // JSON has only one number type; reject floats and non-positive values here.
  return typeof value === "number" && Number.isInteger(value) && value > 0;
}

// Compute Kahn-style step order and expose cycles through a shortened result.
function topologicalStepOrder(stepIDs: string[], dependencies: Map<string, string[]>): string[] {
  const indegree = new Map<string, number>();
  const outgoing = new Map<string, string[]>();
  for (const stepID of stepIDs) {
    indegree.set(stepID, 0);
    outgoing.set(stepID, []);
  }
  for (const [stepID, deps] of dependencies) {
    // Ignore dependency entries for invalid or duplicate step ids; earlier validation already recorded them.
    if (!indegree.has(stepID)) continue;
    // Unknown dependencies are skipped here; existence errors are reported before topological sorting.
    for (const dep of deps) {
      if (!indegree.has(dep)) continue;
      indegree.set(stepID, (indegree.get(stepID) ?? 0) + 1);
      outgoing.get(dep)?.push(stepID);
    }
  }
  // Queue preserves author order for independent steps, making validation output deterministic.
  const queue = stepIDs.filter((stepID) => (indegree.get(stepID) ?? 0) === 0);
  const order: string[] = [];
  for (let head = 0; head < queue.length; head += 1) {
    const stepID = queue[head];
    order.push(stepID);
    for (const next of outgoing.get(stepID) ?? []) {
      const remaining = (indegree.get(next) ?? 0) - 1;
      indegree.set(next, remaining);
      // Newly unblocked dependents are appended so the traversal remains stable and iterative.
      if (remaining === 0) queue.push(next);
    }
  }
  return order;
}

// Report whether the collected field-error map is empty.
function noErrors(errors: FieldErrors): boolean {
  // Empty strings and undefined both mean a field has no active validation error.
  return Object.values(errors).every((message) => !message);
}

// Add line, column, and position hints to browser-specific JSON parse errors.
function jsonErrorDetail(error: unknown, source: string): string {
  const message = error instanceof Error ? error.message : String(error);
  // Browser engines expose JSON parse locations in different formats; try precise forms first.
  const position = extractPosition(message) ?? extractLineColumnPosition(message, source) ?? inferPosition(message, source);
  // When browser messages provide no usable location, keep the original parser text unchanged.
  if (position === undefined) return message;
  const location = locationForPosition(source, position);
  return `${message} (line ${location.line}, column ${location.column}, position ${position})`;
}

// Read V8-style JSON error positions from an exception message.
function extractPosition(message: string): number | undefined {
  // Chromium-style SyntaxError messages include a zero-based character position.
  const match = /position\s+(\d+)/i.exec(message);
  if (!match) return undefined;
  return Number(match[1]);
}

// Read line/column JSON error locations from browser messages.
function extractLineColumnPosition(message: string, source: string): number | undefined {
  // Firefox-style SyntaxError messages provide one-based line and column numbers.
  const match = /line\s+(\d+)[,\s]+column\s+(\d+)/i.exec(message);
  if (!match) return undefined;
  return positionForLineColumn(source, Number(match[1]), Number(match[2]));
}

// Infer a useful JSON error offset when the browser omits a numeric position.
function inferPosition(message: string, source: string): number | undefined {
  const unexpectedToken = /Unexpected token '([^']+)'/.exec(message);
  if (unexpectedToken?.[1]) {
    // This fallback is approximate but points users at the first matching unexpected character.
    const index = source.indexOf(unexpectedToken[1]);
    if (index >= 0) return index;
  }
  // Unexpected EOF points at the end of the current text, which is where users need to continue editing.
  if (/Unexpected end/i.test(message)) return source.length;
  return undefined;
}

// Convert one-based parser line/column positions into a zero-based string offset.
function positionForLineColumn(source: string, targetLine: number, targetColumn: number): number | undefined {
  if (targetLine <= 0 || targetColumn <= 0) return undefined;
  let line = 1;
  let column = 1;
  // Include source.length so parsers that point at EOF can still map to a position.
  for (let index = 0; index <= source.length; index += 1) {
    if (line === targetLine && column === targetColumn) return index;
    if (source[index] === "\n") {
      line += 1;
      column = 1;
    } else {
      column += 1;
    }
  }
  return undefined;
}

// Convert a zero-based string offset back into one-based line and column.
function locationForPosition(source: string, position: number): { line: number; column: number } {
  let line = 1;
  let column = 1;
  // Clamp to source length so inferred EOF positions cannot walk past the string.
  for (let index = 0; index < Math.min(position, source.length); index += 1) {
    if (source[index] === "\n") {
      line += 1;
      column = 1;
    } else {
      column += 1;
    }
  }
  return { line, column };
}
