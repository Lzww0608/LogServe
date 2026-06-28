export type TaskMode = "source" | "ref" | "hash";

type FieldErrors = Record<string, string | undefined>;

export type JSONFieldResult<T> =
  | { valid: true; value: T }
  | { valid: false; message: string };

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

export interface WorkflowFormValidation {
  valid: boolean;
  errors: {
    workflowName?: string;
    definition?: string;
  };
  parsedDefinition?: unknown;
}

export interface ArgsFormValidation {
  valid: boolean;
  errors: {
    args?: string;
  };
  parsedArgs?: unknown[];
}

export interface ActorCreateFormValidation extends ArgsFormValidation {
  errors: ArgsFormValidation["errors"] & {
    className?: string;
    classSource?: string;
  };
}

export interface ActorCallFormValidation extends ArgsFormValidation {
  errors: ArgsFormValidation["errors"] & {
    method?: string;
  };
}

export interface LLMFormValidation {
  valid: boolean;
  errors: {
    modelName?: string;
    prompt?: string;
  };
}

export function parseJSONField<T>(fieldName: string, text: string, fallback: T): JSONFieldResult<T> {
  const source = text.trim() === "" ? JSON.stringify(fallback) : text;
  try {
    return { valid: true, value: JSON.parse(source) as T };
  } catch (error) {
    return { valid: false, message: `${fieldName}: ${jsonErrorDetail(error, source)}` };
  }
}

export function validateTaskForm(input: TaskFormInput): TaskFormValidation {
  const errors: TaskFormValidation["errors"] = {};
  if (!input.taskName.trim()) errors.taskName = "Task name is required.";
  if (!input.functionName.trim()) errors.functionName = "Function name is required.";
  if (input.mode === "source" && !input.functionSource.trim()) errors.functionSource = "Function source is required.";
  if (input.mode === "ref" && !input.functionRef.trim()) errors.functionRef = "Function ref is required.";
  if (input.mode === "hash" && !input.functionHash.trim()) errors.functionHash = "Function hash is required.";

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

export function validateWorkflowForm(workflowName: string, definitionText: string): WorkflowFormValidation {
  const errors: WorkflowFormValidation["errors"] = {};
  if (!workflowName.trim()) errors.workflowName = "Workflow name is required.";
  let parsedDefinition: unknown;
  if (!definitionText.trim()) {
    errors.definition = "Workflow definition JSON is required.";
  } else {
    const definition = parseJSONField<unknown>("Workflow definition JSON", definitionText, {});
    if (definition.valid) parsedDefinition = definition.value;
    else errors.definition = definition.message;
  }
  return { valid: noErrors(errors), errors, parsedDefinition };
}

export function validateActorCreateForm(className: string, classSource: string, initArgsText: string): ActorCreateFormValidation {
  const errors: ActorCreateFormValidation["errors"] = {};
  if (!className.trim()) errors.className = "Class name is required.";
  if (!classSource.trim()) errors.classSource = "Class source is required.";
  const args = parseArgsField(initArgsText, "Init args JSON");
  if (!args.valid) errors.args = args.message;
  return { valid: noErrors(errors), errors, parsedArgs: args.valid ? args.value : undefined };
}

export function validateActorCallForm(method: string, argsText: string): ActorCallFormValidation {
  const errors: ActorCallFormValidation["errors"] = {};
  if (!method.trim()) errors.method = "Actor method is required.";
  const args = parseArgsField(argsText);
  if (!args.valid) errors.args = args.message;
  return { valid: noErrors(errors), errors, parsedArgs: args.valid ? args.value : undefined };
}

export function validateLLMForm(modelName: string, prompt: string): LLMFormValidation {
  const errors: LLMFormValidation["errors"] = {};
  if (!modelName.trim()) errors.modelName = "Model name is required.";
  if (!prompt.trim()) errors.prompt = "Prompt is required.";
  return { valid: noErrors(errors), errors };
}

export function firstValidationError(errors: FieldErrors): string {
  return Object.values(errors).find((message): message is string => Boolean(message)) ?? "Fix highlighted fields before submitting.";
}

function parseArgsField(text: string, fieldName = "Args JSON"): JSONFieldResult<unknown[]> {
  const args = parseJSONField<unknown>(fieldName, text, []);
  if (!args.valid) return args;
  if (!Array.isArray(args.value)) return { valid: false, message: `${fieldName}: expected a JSON array.` };
  return { valid: true, value: args.value };
}

function parseKwargsField(text: string): JSONFieldResult<Record<string, unknown>> {
  const kwargs = parseJSONField<unknown>("Kwargs JSON", text, {});
  if (!kwargs.valid) return kwargs;
  if (!isPlainObject(kwargs.value)) return { valid: false, message: "Kwargs JSON: expected a JSON object." };
  return { valid: true, value: kwargs.value };
}

function isPlainObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function noErrors(errors: FieldErrors): boolean {
  return Object.values(errors).every((message) => !message);
}

function jsonErrorDetail(error: unknown, source: string): string {
  const message = error instanceof Error ? error.message : String(error);
  const position = extractPosition(message) ?? extractLineColumnPosition(message, source) ?? inferPosition(message, source);
  if (position === undefined) return message;
  const location = locationForPosition(source, position);
  return `${message} (line ${location.line}, column ${location.column}, position ${position})`;
}

function extractPosition(message: string): number | undefined {
  const match = /position\s+(\d+)/i.exec(message);
  if (!match) return undefined;
  return Number(match[1]);
}

function extractLineColumnPosition(message: string, source: string): number | undefined {
  const match = /line\s+(\d+)[,\s]+column\s+(\d+)/i.exec(message);
  if (!match) return undefined;
  return positionForLineColumn(source, Number(match[1]), Number(match[2]));
}

function inferPosition(message: string, source: string): number | undefined {
  const unexpectedToken = /Unexpected token '([^']+)'/.exec(message);
  if (unexpectedToken?.[1]) {
    const index = source.indexOf(unexpectedToken[1]);
    if (index >= 0) return index;
  }
  if (/Unexpected end/i.test(message)) return source.length;
  return undefined;
}

function positionForLineColumn(source: string, targetLine: number, targetColumn: number): number | undefined {
  if (targetLine <= 0 || targetColumn <= 0) return undefined;
  let line = 1;
  let column = 1;
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

function locationForPosition(source: string, position: number): { line: number; column: number } {
  let line = 1;
  let column = 1;
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
