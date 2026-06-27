export const addSource = `def add(a: int, b: int) -> int:
    return a + b
`;

export const failSource = `def fail() -> None:
    raise RuntimeError("demo failure")
`;

export const counterSource = `class Counter:
    def __init__(self, value=0):
        self.value = value

    def inc(self, by=1):
        self.value += by
        return self.value
`;

export const workflowTemplate = {
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
};
