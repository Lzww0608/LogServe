// Built-in Python and workflow examples used to seed console forms.

// addSource is the smallest successful Python task example.
export const addSource = `def add(a: int, b: int) -> int:
    return a + b
`;

// failSource intentionally raises so users can inspect failed-task handling.
export const failSource = `def fail() -> None:
    raise RuntimeError("demo failure")
`;

// counterSource demonstrates actor state mutation through instance fields.
export const counterSource = `class Counter:
    def __init__(self, value=0):
        self.value = value

    def inc(self, by=1):
        self.value += by
        return self.value
`;

// embedSource is a deterministic stand-in for vector embedding in workflow examples.
export const embedSource = `def embed(query: str) -> str:
    return "vec:" + query
`;

// searchSource consumes the mock embedding output and returns deterministic documents.
export const searchSource = `def search(vec: str) -> list[str]:
    return ["doc:" + vec]
`;

// generateMockSource simulates a final answer step without requiring an LLM backend.
export const generateMockSource = `def generate_mock(query: str, docs: list[str]) -> str:
    return "answer:" + query + ":" + docs[0]
`;

// splitSource is retained for custom map/reduce editing examples.
export const splitSource = `def split_items(items: list[int]) -> list[list[int]]:
    midpoint = max(1, len(items) // 2)
    return [items[:midpoint], items[midpoint:]]
`;

// sumPartSource is the map phase used by the map-reduce workflow template.
export const sumPartSource = `def sum_part(values: list[int]) -> int:
    return sum(values)
`;

// combineSource is the reduce phase used by the map-reduce workflow template.
export const combineSource = `def combine(left: int, right: int) -> int:
    return left + right
`;

// buildPromptSource feeds an LLM workflow step through a StepRef prompt.
export const buildPromptSource = `def build_prompt(query: str, docs: list[str]) -> str:
    return "answer " + query + " using " + docs[0]
`;

// workflowTemplates are UI seed definitions kept valid against the local workflow validator.
export const workflowTemplates = [
  {
    id: "simple_add",
    label: "simple_add",
    definition: {
      workflow_name: "simple_add",
      steps: [
        {
          step_id: "add",
          task_name: "add",
          function_name: "add",
          function_source: addSource,
          args_json: { args: [1, 2], kwargs: {} },
          depends_on: []
        }
      ],
      result_step_id: "add",
      max_attempts: 3,
      timeout_ms: 30000
    }
  },
  {
    id: "simple_rag",
    label: "simple_rag",
    definition: {
      workflow_name: "simple_rag",
      steps: [
        {
          step_id: "embed",
          task_name: "embed",
          function_name: "embed",
          function_source: embedSource,
          args_json: { args: ["hello"], kwargs: {} },
          depends_on: []
        },
        {
          step_id: "search",
          task_name: "search",
          function_name: "search",
          function_source: searchSource,
          // __step_ref__ demonstrates how workflow args consume a prior step result.
          args_json: { args: [{ __step_ref__: "embed" }], kwargs: {} },
          depends_on: ["embed"]
        },
        {
          step_id: "generate_mock",
          task_name: "generate_mock",
          function_name: "generate_mock",
          function_source: generateMockSource,
          args_json: { args: ["hello", { __step_ref__: "search" }], kwargs: {} },
          depends_on: ["search"]
        }
      ],
      result_step_id: "generate_mock",
      max_attempts: 3,
      timeout_ms: 30000
    }
  },
  {
    id: "map_reduce",
    label: "map-reduce",
    definition: {
      workflow_name: "map_reduce",
      steps: [
        {
          step_id: "sum_left",
          task_name: "sum_part",
          function_name: "sum_part",
          function_source: sumPartSource,
          args_json: { args: [[1, 2, 3]], kwargs: {} },
          depends_on: []
        },
        {
          step_id: "sum_right",
          task_name: "sum_part",
          function_name: "sum_part",
          function_source: sumPartSource,
          args_json: { args: [[4, 5, 6]], kwargs: {} },
          depends_on: []
        },
        {
          step_id: "combine",
          task_name: "combine",
          function_name: "combine",
          function_source: combineSource,
          // Multiple __step_ref__ args show fan-in from independent upstream steps.
          args_json: { args: [{ __step_ref__: "sum_left" }, { __step_ref__: "sum_right" }], kwargs: {} },
          depends_on: ["sum_left", "sum_right"]
        }
      ],
      result_step_id: "combine",
      max_attempts: 3,
      timeout_ms: 30000
    }
  },
  {
    id: "llm_generation_step",
    label: "LLM generation step",
    definition: {
      workflow_name: "llm_generation",
      steps: [
        {
          step_id: "build_prompt",
          task_name: "build_prompt",
          function_name: "build_prompt",
          function_source: buildPromptSource,
          args_json: { args: ["hello", ["doc:vec:hello"]], kwargs: {} },
          depends_on: []
        },
        {
          step_id: "generate_answer",
          task_name: "llm:model-A",
          // __logserve_llm__ marks this workflow step as an LLM task rather than Python execution.
          function_name: "__logserve_llm__",
          function_source: "",
          args_json: { args: [{ __step_ref__: "build_prompt" }], kwargs: {} },
          depends_on: ["build_prompt"],
          llm_model_name: "model-A",
          llm_model_version: "v1",
          llm_adapter: "mock",
          llm_max_tokens: 64
        }
      ],
      result_step_id: "generate_answer",
      max_attempts: 3,
      timeout_ms: 30000
    }
  }
] as const;

// workflowTemplate preserves the original single default consumed by older form code.
export const workflowTemplate = workflowTemplates[0].definition;
