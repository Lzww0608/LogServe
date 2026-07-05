// Unit tests for console form validation and workflow DAG validation.

import assert from "node:assert/strict";
import test from "node:test";
import {
  analyzeWorkflowDefinition,
  parseJSONField,
  validateActorCallForm,
  validateLLMForm,
  validateTaskForm,
  validateWorkflowForm
} from "../.tmp-form-tests/src/utils/formValidation.js";

// The test script compiles source into .tmp-form-tests before running these Node tests.
// Verifies parseJSONField reports the field and JSON error position.
test("parseJSONField reports the field and JSON error position", () => {
  const result = parseJSONField("Args JSON", "[1, }", []);

  assert.equal(result.valid, false);
  assert.match(result.message ?? "", /^Args JSON:/);
  assert.match(result.message ?? "", /position 4/);
  assert.match(result.message ?? "", /line 1, column 5/);
});

// Verifies validateTaskForm blocks empty names and bad args before submit.
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


// Verifies validateTaskForm requires the selected function identity.
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
  const refWithoutHash = validateTaskForm({
    mode: "ref",
    taskName: "add",
    functionName: "add",
    functionSource: "",
    functionRef: "local://functions/add.py",
    functionHash: " ",
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
  assert.equal(refResult.errors.functionHash, "Function hash is required with function ref.");
  assert.equal(refWithoutHash.valid, false);
  assert.equal(refWithoutHash.errors.functionHash, "Function hash is required with function ref.");
  assert.equal(hashResult.valid, false);
  assert.equal(hashResult.errors.functionHash, "Function hash is required.");
});

// Verifies validateTaskForm accepts registered function ref with hash.
test("validateTaskForm accepts registered function ref with hash", () => {
  const result = validateTaskForm({
    mode: "ref",
    taskName: "add",
    functionName: "add",
    functionSource: "",
    functionRef: "local://functions/add.py",
    functionHash: "sha256:abc123",
    argsText: "[1, 2]",
    kwargsText: "{}"
  });

  assert.equal(result.valid, true);
  assert.deepEqual(result.parsedArgs, [1, 2]);
});
// Verifies validateTaskForm accepts hash mode with parsed args and kwargs.
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

// Verifies validateWorkflowForm validates name and definition JSON immediately.
test("validateWorkflowForm validates name and definition JSON immediately", () => {
  const result = validateWorkflowForm(" ", "{\"steps\": [}");

  assert.equal(result.valid, false);
  assert.equal(result.parsedDefinition, undefined);
  assert.equal(result.errors.workflowName, "Workflow name is required.");
  assert.match(result.errors.definition ?? "", /Workflow definition JSON:.*position 11/);
});

// Verifies validateActorCallForm requires method and valid args.
test("validateActorCallForm requires method and valid args", () => {
  const result = validateActorCallForm(" ", "[1, }");

  assert.equal(result.valid, false);
  assert.equal(result.parsedArgs, undefined);
  assert.equal(result.errors.method, "Actor method is required.");
  assert.match(result.errors.args ?? "", /Args JSON:.*position 4/);
});

// Verifies validateLLMForm requires model name and prompt.
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

// Verifies analyzeWorkflowDefinition validates DAG shape and topological order.
test("analyzeWorkflowDefinition validates DAG shape and topological order", () => {
  const valid = analyzeWorkflowDefinition({
    result_step_id: "finish",
    steps: [
      { step_id: "start", task_name: "start", function_name: "start", function_source: "def start():\n    return 1\n", depends_on: [] },
      { step_id: "finish", task_name: "finish", function_name: "finish", function_source: "def finish():\n    return 2\n", depends_on: ["start"] }
    ]
  });

  assert.equal(valid.valid, true);
  assert.deepEqual(valid.order, ["start", "finish"]);

  const invalid = analyzeWorkflowDefinition({
    result_step_id: "missing",
    steps: [
      { step_id: "a", task_name: "a", function_name: "a", function_source: "def a():\n    return 1\n", depends_on: ["b"] },
      { step_id: "a", task_name: "dup", function_name: "dup", function_source: "def dup():\n    return 2\n", depends_on: [] },
      { step_id: "cycle", task_name: "cycle", function_name: "cycle", function_source: "def cycle():\n    return 3\n", depends_on: ["cycle"] }
    ]
  });

  assert.equal(invalid.valid, false);
  assert.match(invalid.errors.join("\n"), /Duplicate workflow step_id "a"/);
  assert.match(invalid.errors.join("\n"), /result_step_id "missing"/);
  assert.match(invalid.errors.join("\n"), /unknown step "b"/);
  assert.match(invalid.errors.join("\n"), /cannot depend on itself/);
  assert.match(invalid.errors.join("\n"), /cycle/i);
});
