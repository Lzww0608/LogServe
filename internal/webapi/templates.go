package webapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
)

type templateDTO struct {
	ID             string `json:"id"`
	Label          string `json:"label"`
	Kind           string `json:"kind"`
	Description    string `json:"description"`
	ExpectedResult string `json:"expected_result"`
	RequiredRole   role   `json:"required_role"`
	Payload        any    `json:"payload,omitempty"`
}

type runTemplateRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
}

func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"templates": builtinTemplates(false)})
}

func (s *Server) handleGetTemplate(w http.ResponseWriter, r *http.Request) {
	template, ok := builtinTemplate(r.PathValue("template_id"), true)
	if !ok {
		writeAPIError(w, http.StatusNotFound, "NOT_FOUND", "template not found")
		return
	}
	writeJSON(w, template)
}

func (s *Server) handleRunTemplate(w http.ResponseWriter, r *http.Request) {
	templateID := strings.TrimSpace(r.PathValue("template_id"))
	template, ok := builtinTemplate(templateID, false)
	if !ok {
		writeAPIError(w, http.StatusNotFound, "NOT_FOUND", "template not found")
		return
	}
	if !roleAllows(principalFromRequest(r).Role, template.RequiredRole) {
		writeAPIError(w, http.StatusForbidden, "PERMISSION_DENIED", "insufficient role for this template")
		return
	}
	input := runTemplateRequest{}
	if r.Body != nil && r.ContentLength != 0 {
		if err := decodeJSON(w, r, &input); err != nil {
			writeErr(w, err)
			return
		}
	}
	key := strings.TrimSpace(input.IdempotencyKey)
	if key == "" {
		key = fmt.Sprintf("console:template:%s:%d", templateID, time.Now().UnixNano())
	}
	result, err := s.runBuiltinTemplate(r, templateID, key)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"template": template,
		"result":   result,
	})
}

func (s *Server) runBuiltinTemplate(r *http.Request, templateID, idempotencyKey string) (any, error) {
	switch templateID {
	case "add_task":
		return s.runTaskTemplate(r, &logservepb.SubmitTaskRequest{
			TaskName:       "add",
			FunctionName:   "add",
			FunctionSource: addTaskSource,
			ArgsJson:       []byte(`{"args":[1,2],"kwargs":{}}`),
			IdempotencyKey: idempotencyKey,
		})
	case "fail_task":
		return s.runTaskTemplate(r, &logservepb.SubmitTaskRequest{
			TaskName:       "fail",
			FunctionName:   "fail",
			FunctionSource: failTaskSource,
			ArgsJson:       []byte(`{"args":[],"kwargs":{}}`),
			IdempotencyKey: idempotencyKey,
		})
	case "sleep_task":
		return s.runTaskTemplate(r, &logservepb.SubmitTaskRequest{
			TaskName:       "sleep_task",
			FunctionName:   "sleep_task",
			FunctionSource: sleepTaskSource,
			ArgsJson:       []byte(`{"args":[0.1],"kwargs":{}}`),
			IdempotencyKey: idempotencyKey,
		})
	case "simple_rag_workflow":
		definition, err := json.Marshal(simpleRAGWorkflowDefinition())
		if err != nil {
			return nil, err
		}
		return s.runWorkflowTemplate(r, "simple_rag", definition, idempotencyKey)
	case "actor_counter":
		return s.runCounterTemplate(r, idempotencyKey)
	case "mock_llm_request":
		return s.runMockLLMTemplate(r, idempotencyKey)
	default:
		return nil, fmt.Errorf("%w: unknown template %q", errInvalidInput, templateID)
	}
}

func (s *Server) runTaskTemplate(r *http.Request, req *logservepb.SubmitTaskRequest) (TaskDTO, error) {
	ctx, cancel := requestContext(r, s.cfg.RequestTimeout)
	defer cancel()
	resp, err := s.clients.Control.SubmitTask(ctx, req)
	if err != nil {
		return TaskDTO{}, err
	}
	dto := TaskDTO{TaskID: resp.GetTaskId(), Status: taskStatusString(resp.GetStatus())}
	if waitRequested(r) {
		return s.waitTask(r, resp.GetTaskId(), waitTimeout(r, 30*time.Second))
	}
	return dto, nil
}

func (s *Server) runWorkflowTemplate(r *http.Request, name string, definition []byte, idempotencyKey string) (WorkflowDTO, error) {
	ctx, cancel := requestContext(r, s.cfg.RequestTimeout)
	defer cancel()
	resp, err := s.clients.Control.SubmitWorkflow(ctx, &logservepb.SubmitWorkflowRequest{
		WorkflowName:   name,
		DefinitionJson: definition,
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return WorkflowDTO{}, err
	}
	dto := WorkflowDTO{WorkflowID: resp.GetWorkflowId(), Status: workflowStatusString(resp.GetStatus())}
	if waitRequested(r) {
		return s.waitWorkflow(r, resp.GetWorkflowId(), waitTimeout(r, 60*time.Second))
	}
	return dto, nil
}

func (s *Server) runCounterTemplate(r *http.Request, idempotencyKey string) (map[string]any, error) {
	ctx, cancel := requestContext(r, 35*time.Second)
	defer cancel()
	created, err := s.clients.Control.CreateActor(ctx, &logservepb.CreateActorRequest{
		ClassName:      "Counter",
		ClassSource:    counterTemplateSource,
		InitArgsJson:   []byte(`{"args":[0],"kwargs":{}}`),
		IdempotencyKey: idempotencyKey + ":create",
		SnapshotEvery:  25,
	})
	if err != nil {
		return nil, err
	}
	called, err := s.clients.Control.CallActor(ctx, &logservepb.CallActorRequest{
		ActorId:        created.GetActorId(),
		MethodName:     "inc",
		ArgsJson:       []byte(`{"args":[1],"kwargs":{}}`),
		IdempotencyKey: idempotencyKey + ":call",
		TimeoutMs:      30000,
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"actor": ActorDTO{
			ActorID:       created.GetActorId(),
			ClassName:     "Counter",
			Status:        actorStatusString(created.GetStatus()),
			OwnerWorkerID: created.GetOwnerWorkerId(),
			Epoch:         created.GetEpoch(),
		},
		"call": actorCallDTO(called),
	}, nil
}

func (s *Server) runMockLLMTemplate(r *http.Request, idempotencyKey string) (LLMDTO, error) {
	ctx, cancel := requestContext(r, s.cfg.RequestTimeout)
	defer cancel()
	registerStarted := time.Now()
	_, err := s.clients.Control.RegisterModel(ctx, &logservepb.RegisterModelRequest{Model: &logservepb.ModelInfo{
		Name:      "model-A",
		Version:   "v1",
		SizeBytes: 100,
		Path:      "mock://model-A",
		Adapter:   "mock",
	}})
	if err != nil {
		return LLMDTO{}, err
	}
	s.auditFrontendOperation(r, principalFromRequest(r), "register_model", http.StatusOK, registerStarted)
	resp, err := s.clients.Control.SubmitLLM(ctx, &logservepb.SubmitLLMRequest{
		ModelName:      "model-A",
		ModelVersion:   "v1",
		Prompt:         "hello",
		MaxTokens:      64,
		Adapter:        "mock",
		IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		return LLMDTO{}, err
	}
	dto := LLMDTO{TaskID: resp.GetTaskId(), Status: taskStatusString(resp.GetStatus())}
	if waitRequested(r) {
		task, err := s.waitTask(r, resp.GetTaskId(), waitTimeout(r, 60*time.Second))
		if err != nil {
			return LLMDTO{}, err
		}
		dto.Status = task.Status
		dto.Result = task.Result
		dto.Error = task.Error
		dto.WorkerID = task.WorkerID
	}
	return dto, nil
}

func builtinTemplates(includePayload bool) []templateDTO {
	ids := []string{"add_task", "fail_task", "sleep_task", "simple_rag_workflow", "actor_counter", "mock_llm_request"}
	out := make([]templateDTO, 0, len(ids))
	for _, id := range ids {
		template, _ := builtinTemplate(id, includePayload)
		out = append(out, template)
	}
	return out
}

func builtinTemplate(id string, includePayload bool) (templateDTO, bool) {
	template := templateDTO{RequiredRole: roleOperator}
	switch id {
	case "add_task":
		template = templateDTO{ID: id, Label: "Add task", Kind: "task", Description: "Runs a Python add(a, b) task with inputs 1 and 2.", ExpectedResult: "Task succeeds with result_json 3.", RequiredRole: roleOperator}
		if includePayload {
			template.Payload = map[string]any{"task_name": "add", "function_name": "add", "function_source": addTaskSource, "args": []any{1, 2}, "kwargs": map[string]any{}}
		}
	case "fail_task":
		template = templateDTO{ID: id, Label: "Fail task", Kind: "task", Description: "Runs a Python task that raises RuntimeError for failure-path checks.", ExpectedResult: "Task fails and records RuntimeError: demo failure.", RequiredRole: roleOperator}
		if includePayload {
			template.Payload = map[string]any{"task_name": "fail", "function_name": "fail", "function_source": failTaskSource, "args": []any{}, "kwargs": map[string]any{}}
		}
	case "sleep_task":
		template = templateDTO{ID: id, Label: "Sleep task", Kind: "task", Description: "Sleeps briefly, then returns the elapsed demo marker.", ExpectedResult: "Task succeeds with result_json \"slept:0.1\".", RequiredRole: roleOperator}
		if includePayload {
			template.Payload = map[string]any{"task_name": "sleep_task", "function_name": "sleep_task", "function_source": sleepTaskSource, "args": []any{0.1}, "kwargs": map[string]any{}}
		}
	case "simple_rag_workflow":
		template = templateDTO{ID: id, Label: "Simple RAG workflow", Kind: "workflow", Description: "Runs embed, search, and mock generation steps as a small DAG.", ExpectedResult: "Workflow completes with result_json \"answer:hello:doc:vec:hello\".", RequiredRole: roleOperator}
		if includePayload {
			template.Payload = simpleRAGWorkflowDefinition()
		}
	case "actor_counter":
		template = templateDTO{ID: id, Label: "Actor Counter", Kind: "actor", Description: "Creates a Counter actor initialized at 0 and calls inc(1).", ExpectedResult: "Actor call succeeds with result_json 1.", RequiredRole: roleOperator}
		if includePayload {
			template.Payload = map[string]any{"class_name": "Counter", "class_source": counterTemplateSource, "init_args": []any{0}, "call": map[string]any{"method_name": "inc", "args": []any{1}}}
		}
	case "mock_llm_request":
		template = templateDTO{ID: id, Label: "Mock LLM request", Kind: "llm", Description: "Registers the built-in mock model and submits a prompt through the LLM task path.", ExpectedResult: "LLM task succeeds with result_json \"mock:model-A:v1:hello\".", RequiredRole: roleAdmin}
		if includePayload {
			template.Payload = map[string]any{"model_name": "model-A", "model_version": "v1", "adapter": "mock", "prompt": "hello", "max_tokens": 64}
		}
	default:
		return templateDTO{}, false
	}
	return template, true
}

func simpleRAGWorkflowDefinition() map[string]any {
	return map[string]any{
		"workflow_name": "simple_rag",
		"steps": []map[string]any{
			{"step_id": "embed", "task_name": "embed", "function_name": "embed", "function_source": embedSource, "args_json": map[string]any{"args": []any{"hello"}, "kwargs": map[string]any{}}, "depends_on": []string{}},
			{"step_id": "search", "task_name": "search", "function_name": "search", "function_source": searchSource, "args_json": map[string]any{"args": []any{map[string]any{"__step_ref__": "embed"}}, "kwargs": map[string]any{}}, "depends_on": []string{"embed"}},
			{"step_id": "generate_mock", "task_name": "generate_mock", "function_name": "generate_mock", "function_source": generateMockSource, "args_json": map[string]any{"args": []any{"hello", map[string]any{"__step_ref__": "search"}}, "kwargs": map[string]any{}}, "depends_on": []string{"search"}},
		},
		"result_step_id": "generate_mock",
		"max_attempts":   3,
		"timeout_ms":     30000,
	}
}

const addTaskSource = `def add(a: int, b: int) -> int:
    return a + b
`

const failTaskSource = `def fail() -> None:
    raise RuntimeError("demo failure")
`

const sleepTaskSource = `import time

def sleep_task(seconds: float) -> str:
    time.sleep(seconds)
    return f"slept:{seconds}"
`

const counterTemplateSource = `class Counter:
    def __init__(self, value=0):
        self.value = value

    def inc(self, by=1):
        self.value += by
        return self.value

    def get(self):
        return self.value
`

const embedSource = `def embed(query: str) -> str:
    return "vec:" + query
`

const searchSource = `def search(vec: str) -> list[str]:
    return ["doc:" + vec]
`

const generateMockSource = `def generate_mock(query: str, docs: list[str]) -> str:
    return "answer:" + query + ":" + docs[0]
`
