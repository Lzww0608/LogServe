package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/eventcodec"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type submitTaskRequest struct {
	TaskName       string          `json:"task_name"`
	FunctionName   string          `json:"function_name"`
	FunctionSource string          `json:"function_source"`
	FunctionRef    string          `json:"function_ref"`
	FunctionHash   string          `json:"function_hash"`
	Args           json.RawMessage `json:"args"`
	Kwargs         json.RawMessage `json:"kwargs"`
	ArgsJSON       json.RawMessage `json:"args_json"`
	IdempotencyKey string          `json:"idempotency_key"`
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	dashboard, err := s.dashboard(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	status := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("status")))
	search := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	out := make([]TaskDTO, 0, len(dashboard.Tasks))
	for _, task := range dashboard.Tasks {
		if status != "" && task.Status != status {
			continue
		}
		haystack := strings.ToLower(task.TaskID + " " + task.TaskName + " " + task.WorkerID + " " + task.WorkflowID + " " + task.ActorID + " " + task.LLMModelName)
		if search != "" && !strings.Contains(haystack, search) {
			continue
		}
		out = append(out, task)
	}
	writeJSON(w, map[string]any{"tasks": out})
}

func (s *Server) handleSubmitTask(w http.ResponseWriter, r *http.Request) {
	var input submitTaskRequest
	if err := decodeJSON(w, r, &input); err != nil {
		writeErr(w, err)
		return
	}
	if len(input.FunctionSource) > maxSourceBytes {
		writeErr(w, fmt.Errorf("%w: function_source exceeds %d bytes", errInvalidInput, maxSourceBytes))
		return
	}
	argsJSON := []byte(input.ArgsJSON)
	var err error
	if len(argsJSON) == 0 {
		argsJSON, err = envelopeArgs(input.Args, input.Kwargs)
		if err != nil {
			writeErr(w, err)
			return
		}
	} else if err := validateRawJSON("args_json", input.ArgsJSON, maxJSONBytes); err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := requestContext(r, s.cfg.RequestTimeout)
	defer cancel()
	resp, err := s.clients.Control.SubmitTask(ctx, &logservepb.SubmitTaskRequest{
		TaskName:       input.TaskName,
		FunctionName:   input.FunctionName,
		FunctionSource: input.FunctionSource,
		FunctionRef:    input.FunctionRef,
		FunctionHash:   input.FunctionHash,
		ArgsJson:       argsJSON,
		IdempotencyKey: input.IdempotencyKey,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	dto := TaskDTO{TaskID: resp.GetTaskId(), Status: taskStatusString(resp.GetStatus())}
	if waitRequested(r) {
		dto, err = s.waitTask(r, resp.GetTaskId(), waitTimeout(r, 30*time.Second))
		if err != nil {
			writeErr(w, err)
			return
		}
	}
	writeJSON(w, dto)
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("task_id")
	ctx, cancel := requestContext(r, s.cfg.RequestTimeout)
	defer cancel()
	resp, err := s.clients.Control.GetTaskStatus(ctx, &logservepb.GetTaskStatusRequest{TaskId: taskID})
	if err != nil {
		writeErr(w, err)
		return
	}
	dto := taskStatusDTO(resp)
	if dashboard, err := s.dashboard(r); err == nil {
		dto = mergeTaskDTO(dto, dashboardTaskByID(dashboard.Tasks, taskID))
	}
	writeJSON(w, dto)
}

func (s *Server) handleRetryTask(w http.ResponseWriter, r *http.Request) {
	s.handleTaskSubmitOperation(w, r, "retry")
}

func (s *Server) handleResubmitTask(w http.ResponseWriter, r *http.Request) {
	s.handleTaskSubmitOperation(w, r, "resubmit")
}

func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	writeAPIError(w, http.StatusNotImplemented, "UNSUPPORTED_OPERATION", "task cancellation is not supported by this backend")
}

func (s *Server) handleTaskSubmitOperation(w http.ResponseWriter, r *http.Request, operation string) {
	taskID := strings.TrimSpace(r.PathValue("task_id"))
	if taskID == "" {
		writeErr(w, fmt.Errorf("%w: task_id is required", errInvalidInput))
		return
	}

	ctx, cancel := requestContext(r, s.cfg.RequestTimeout)
	defer cancel()
	current, err := s.clients.Control.GetTaskStatus(ctx, &logservepb.GetTaskStatusRequest{TaskId: taskID})
	if err != nil {
		writeErr(w, err)
		return
	}
	if operation == "retry" && current.GetStatus() != logservepb.TaskStatus_TASK_STATUS_FAILED {
		writeErr(w, status.Errorf(codes.FailedPrecondition, "retry requires a failed task; current status is %s", taskStatusString(current.GetStatus())))
		return
	}

	spec, err := s.readTaskSubmittedSpec(ctx, taskID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if err := validateStandaloneTaskOperationSpec(spec); err != nil {
		writeErr(w, err)
		return
	}

	resp, err := s.clients.Control.SubmitTask(ctx, &logservepb.SubmitTaskRequest{
		TaskName:       spec.GetTaskName(),
		FunctionName:   spec.GetFunctionName(),
		FunctionSource: spec.GetFunctionSource(),
		FunctionRef:    spec.GetFunctionRef(),
		FunctionHash:   spec.GetFunctionHash(),
		ArgsJson:       append([]byte(nil), spec.GetArgsJson()...),
		IdempotencyKey: fmt.Sprintf("console:%s:%s:%d", operation, taskID, time.Now().UnixNano()),
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	dto := TaskDTO{TaskID: resp.GetTaskId(), Status: taskStatusString(resp.GetStatus())}
	if waitRequested(r) {
		dto, err = s.waitTask(r, resp.GetTaskId(), waitTimeout(r, 30*time.Second))
		if err != nil {
			writeErr(w, err)
			return
		}
	}
	writeJSON(w, dto)
}

func (s *Server) readTaskSubmittedSpec(ctx context.Context, taskID string) (*logservepb.TaskSpec, error) {
	resp, err := s.clients.Log.ReadLog(ctx, &logservepb.ReadLogRequest{StreamId: "task:" + taskID, FromSeq: 1, Limit: maxLogReadLimit})
	if err != nil {
		return nil, err
	}
	for _, record := range resp.GetRecords() {
		if record.GetEventType() != "TaskSubmitted" {
			continue
		}
		spec, err := unmarshalTaskSubmittedSpec(record.GetPayload())
		if err != nil {
			return nil, err
		}
		if spec.GetTaskId() == taskID {
			return spec, nil
		}
	}
	return nil, status.Error(codes.NotFound, "task submitted record not found")
}

type taskSubmittedPayload struct {
	TaskSpec json.RawMessage `json:"task_spec,omitempty"`
}

func unmarshalTaskSubmittedSpec(data []byte) (*logservepb.TaskSpec, error) {
	var fields map[string]any
	encoded, err := eventcodec.Unmarshal(eventcodec.KindTaskSubmitted, data, &fields)
	if err != nil {
		return nil, err
	}
	decoded := &logservepb.TaskSpec{}
	if encoded {
		specData := eventcodec.BytesValue(fields["task_spec"])
		if len(specData) == 0 {
			return decoded, nil
		}
		if err := proto.Unmarshal(specData, decoded); err != nil {
			return nil, err
		}
		return decoded, nil
	}

	var payload taskSubmittedPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	if len(payload.TaskSpec) == 0 {
		return decoded, nil
	}
	if err := protojson.Unmarshal(payload.TaskSpec, decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

func validateStandaloneTaskOperationSpec(spec *logservepb.TaskSpec) error {
	if spec.GetWorkflowId() != "" || spec.GetStepId() != "" {
		return status.Error(codes.FailedPrecondition, "task operation is not supported for workflow tasks")
	}
	if spec.GetActorId() != "" || spec.GetActorCallId() != "" || spec.GetActorClassName() != "" || spec.GetActorMethod() != "" {
		return status.Error(codes.FailedPrecondition, "task operation is not supported for actor tasks")
	}
	if spec.GetLlmModelName() != "" || spec.GetLlmModelVersion() != "" || spec.GetLlmAdapter() != "" || spec.GetLlmMaxTokens() != 0 {
		return status.Error(codes.FailedPrecondition, "task operation is not supported for LLM tasks")
	}
	if spec.GetTargetWorkerId() != "" || spec.GetTimeoutMs() != 0 || spec.GetTaskLeaseEpoch() != 0 {
		return status.Error(codes.FailedPrecondition, "task operation cannot safely preserve internal scheduling fields")
	}
	return nil
}
func dashboardTaskByID(tasks []TaskDTO, taskID string) TaskDTO {
	for _, task := range tasks {
		if task.TaskID == taskID {
			return task
		}
	}
	return TaskDTO{}
}

func mergeTaskDTO(primary, metadata TaskDTO) TaskDTO {
	if metadata.TaskID == "" {
		return primary
	}
	if primary.TaskName == "" {
		primary.TaskName = metadata.TaskName
	}
	if primary.WorkerID == "" {
		primary.WorkerID = metadata.WorkerID
	}
	if primary.WorkflowID == "" {
		primary.WorkflowID = metadata.WorkflowID
	}
	if primary.StepID == "" {
		primary.StepID = metadata.StepID
	}
	if primary.ActorID == "" {
		primary.ActorID = metadata.ActorID
	}
	if primary.LLMModelName == "" {
		primary.LLMModelName = metadata.LLMModelName
	}
	if primary.LLMModelVersion == "" {
		primary.LLMModelVersion = metadata.LLMModelVersion
	}
	if primary.CreatedAtMs == 0 {
		primary.CreatedAtMs = metadata.CreatedAtMs
	}
	if primary.UpdatedAtMs == 0 {
		primary.UpdatedAtMs = metadata.UpdatedAtMs
	}
	return primary
}

func (s *Server) waitTask(r *http.Request, taskID string, timeout time.Duration) (TaskDTO, error) {
	ctx, cancel := requestContext(r, timeout)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		resp, err := s.clients.Control.GetTaskStatus(ctx, &logservepb.GetTaskStatusRequest{TaskId: taskID})
		if err != nil {
			return TaskDTO{}, err
		}
		dto := taskStatusDTO(resp)
		if terminalTaskStatus(dto.Status) {
			return dto, nil
		}
		select {
		case <-ctx.Done():
			return dto, ctx.Err()
		case <-ticker.C:
		}
	}
}

func waitRequested(r *http.Request) bool {
	value := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("wait")))
	return value == "1" || value == "true" || value == "yes"
}

func waitTimeout(r *http.Request, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(r.URL.Query().Get("timeout_ms"))
	if value == "" {
		return fallback
	}
	var parsed int64
	if _, err := fmt.Sscan(value, &parsed); err != nil || parsed <= 0 {
		return fallback
	}
	return time.Duration(parsed) * time.Millisecond
}
