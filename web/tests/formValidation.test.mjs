import assert from "node:assert/strict";
import test from "node:test";
import {
  parseJSONField,
  validateActorCallForm,
  validateLLMForm,
  validateTaskForm,
  validateWorkflowForm
} from "../.tmp-form-tests/src/utils/formValidation.js";

test("parseJSONField reports the field and JSON error position", () => {
  const result = parseJSONField("Args JSON", "[1, }", []);

  assert.equal(result.valid, false);
  assert.match(result.message ?? "", /^Args JSON:/);
  assert.match(result.message ?? "", /position 4/);
  assert.match(result.message ?? "", /line 1, column 5/);
});

test("validateTaskForm blocks empty names and bad args before submit", () => {
  const result = validateTaskForm({
    mode: "source",
    taskName: " ",
    functionName: "",
    functionSource: " ",
    functionRef: "",
    functionHash: "",
    argsText: "[1, }",
    kwargsText: "{}"
  });

  assert.equal(result.valid, false);
  assert.equal(result.parsedArgs, undefined);
  assert.equal(result.errors.taskName, "Task name is required.");
  assert.equal(result.errors.functionName, "Function name is required.");
  assert.equal(result.errors.functionSource, "Function source is required.");
  assert.match(result.errors.args ?? "", /Args JSON:.*position 4/);
});


test("validateTaskForm requires the selected function identity", () => {
  const refResult = validateTaskForm({
    mode: "ref",
    taskName: "add",
    functionName: "add",
    functionSource: "",
    functionRef: " ",
    functionHash: "",
    argsText: "[]",
    kwargsText: "{}"
  });
  const hashResult = validateTaskForm({
    mode: "hash",
    taskName: "add",
    functionName: "add",
    functionSource: "",
    functionRef: "",
    functionHash: " ",
    argsText: "[]",
    kwargsText: "{}"
  });

  assert.equal(refResult.valid, false);
  assert.equal(refResult.errors.functionRef, "Function ref is required.");
  assert.equal(hashResult.valid, false);
  assert.equal(hashResult.errors.functionHash, "Function hash is required.");
});
test("validateTaskForm accepts hash mode with parsed args and kwargs", () => {
  const result = validateTaskForm({
    mode: "hash",
    taskName: "add",
    functionName: "add",
    functionSource: "",
    functionRef: "",
    functionHash: "abc123",
    argsText: "[1, 2]",
    kwargsText: "{\"scale\": 3}"
  });

  assert.equal(result.valid, true);
  assert.deepEqual(result.parsedArgs, [1, 2]);
  assert.deepEqual(result.parsedKwargs, { scale: 3 });
});

test("validateWorkflowForm validates name and definition JSON immediately", () => {
  const result = validateWorkflowForm(" ", "{\"steps\": [}");

  assert.equal(result.valid, false);
  assert.equal(result.parsedDefinition, undefined);
  assert.equal(result.errors.workflowName, "Workflow name is required.");
  assert.match(result.errors.definition ?? "", /Workflow definition JSON:.*position 11/);
});

test("validateActorCallForm requires method and valid args", () => {
  const result = validateActorCallForm(" ", "[1, }");

  assert.equal(result.valid, false);
  assert.equal(result.parsedArgs, undefined);
  assert.equal(result.errors.method, "Actor method is required.");
  assert.match(result.errors.args ?? "", /Args JSON:.*position 4/);
});

test("validateLLMForm requires model name and prompt", () => {
  const result = validateLLMForm(" ", " ");

  assert.deepEqual(result, {
    valid: false,
    errors: {
      modelName: "Model name is required.",
      prompt: "Prompt is required."
    }
  });
});
