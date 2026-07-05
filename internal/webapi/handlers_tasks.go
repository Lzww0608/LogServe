package webapi

// This file implements task list, submit, detail, retry, resubmit, and cancel
// HTTP endpoints.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/eventcodec"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// submitTaskRequest is the JSON shape accepted by the task submission endpoint.
// It supports either args/kwargs fields or a prebuilt args_json envelope.
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

// handleListTasks filters dashboard tasks by status, worker, workflow, and search
// text before applying shared pagination.
func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	dashboard, err := s.dashboard(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	status := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("status")))
	search := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	workerID := strings.TrimSpace(r.URL.Query().Get("worker_id"))
	workflowID := strings.TrimSpace(r.URL.Query().Get("workflow_id"))
	out := make([]TaskDTO, 0, len(dashboard.Tasks))
	for _, task := range dashboard.Tasks {
		if status != "" && task.Status != status {
			continue
		}
		if workerID != "" && task.WorkerID != workerID {
			continue
		}
		if workflowID != "" && task.WorkflowID != workflowID {
			continue
		}
		haystack := strings.ToLower(task.TaskID + " " + task.TaskName + " " + task.WorkerID + " " + task.WorkflowID + " " + task.ActorID + " " + task.LLMModelName)
		if search != "" && !strings.Contains(haystack, search) {
			continue
		}
		out = append(out, task)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAtMs == out[j].CreatedAtMs {
			return out[i].TaskID < out[j].TaskID
		}
		return out[i].CreatedAtMs < out[j].CreatedAtMs
	})
	params, err := parsePaginationParams(r, len(out))
	if err != nil {
		writeErr(w, err)
		return
	}
	page := paginate(len(out), params)
	writeJSON(w, map[string]any{
		"tasks":           out[page.Start:page.End],
		"limit":           page.Limit,
		"total_count":     page.TotalCount,
		"next_page_token": page.NextPageToken,
	})
}

// handleSubmitTask validates source and argument JSON, submits a task to the
// control plane, and optionally waits for terminal status.
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
		// Prefer structured args/kwargs for normal UI requests, but preserve args_json
		// when callers already built the executor envelope themselves.
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

// handleGetTask reads authoritative task status and opportunistically merges
// dashboard metadata such as task name and worker assignment.
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

// handleRetryTask resubmits a failed standalone task using the original submitted
// task spec recovered from the log.
func (s *Server) handleRetryTask(w http.ResponseWriter, r *http.Request) {
	s.handleTaskSubmitOperation(w, r, "retry")
}

// handleResubmitTask resubmits a standalone task regardless of current task
// status, using the original submitted task spec recovered from the log.
func (s *Server) handleResubmitTask(w http.ResponseWriter, r *http.Request) {
	s.handleTaskSubmitOperation(w, r, "resubmit")
}

// handleCancelTask reports the current backend limitation rather than silently
// pretending cancellation is supported.
func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	writeAPIError(w, http.StatusNotImplemented, "UNSUPPORTED_OPERATION", "task cancellation is not supported by this backend")
}

// handleTaskSubmitOperation implements retry/resubmit by reading the original
// TaskSubmitted event, rejecting derived tasks, and submitting a fresh task with
// a console-scoped idempotency key.
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

	// Use a fresh console-scoped idempotency key so retry/resubmit does not collide
	// with the original submission or with another manual operation on the same task.
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

// readTaskSubmittedSpec scans the task-specific log stream for the original
// TaskSubmitted event used to reconstruct retry/resubmit requests.
func (s *Server) readTaskSubmittedSpec(ctx context.Context, taskID string) (*logservepb.TaskSpec, error) {
	resp, err := s.clients.Log.ReadLog(ctx, &logservepb.ReadLogRequest{StreamId: "task:" + taskID, FromSeq: 1, Limit: maxLogReadLimit})
	if err != nil {
		return nil, err
	}
	for _, record := range resp.GetRecords() {
		// The task stream can contain lifecycle events before/after submission; only
		// TaskSubmitted carries the original TaskSpec needed for reconstruction.
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

// taskSubmittedPayload is the legacy JSON TaskSubmitted shape used before the
// eventcodec encoded payload path.
type taskSubmittedPayload struct {
	TaskSpec json.RawMessage `json:"task_spec,omitempty"`
}

// unmarshalTaskSubmittedSpec accepts both eventcodec payloads and legacy JSON
// payloads so retry/resubmit works across log format upgrades.
func unmarshalTaskSubmittedSpec(data []byte) (*logservepb.TaskSpec, error) {
	var fields map[string]any
	encoded, err := eventcodec.Unmarshal(eventcodec.KindTaskSubmitted, data, &fields)
	if err != nil {
		return nil, err
	}
	decoded := &logservepb.TaskSpec{}
	if encoded {
		// Newer logs store the protobuf TaskSpec bytes inside the eventcodec envelope;
		// an absent field falls through as an empty spec to preserve historical behavior.
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

// validateStandaloneTaskOperationSpec rejects workflow, actor, LLM, and internal
// scheduling task specs because retry/resubmit cannot safely replay their hidden
// execution context.
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

// dashboardTaskByID finds optional dashboard metadata for a task status response.
func dashboardTaskByID(tasks []TaskDTO, taskID string) TaskDTO {
	for _, task := range tasks {
		if task.TaskID == taskID {
			return task
		}
	}
	return TaskDTO{}
}

// mergeTaskDTO fills missing status-response fields with dashboard metadata
// without overwriting authoritative task status, result, or error values.
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

// waitTask polls task status until a terminal state or timeout. On timeout it
// returns the latest observed state together with the context error.
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

// waitRequested parses the optional wait query flag accepted by submit endpoints.
func waitRequested(r *http.Request) bool {
	value := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("wait")))
	return value == "1" || value == "true" || value == "yes"
}

// waitTimeout parses timeout_ms and falls back when omitted or malformed.
func waitTimeout(r *http.Request, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(r.URL.Query().Get("timeout_ms"))
	if value == "" {
		return fallback
	}
	var parsed int64
	if _, err := fmt.Sscan(value, &parsed); err != nil || parsed <= 0 {
		// Bad timeout values are treated as omitted so callers cannot accidentally
		// force a zero-timeout wait loop.
		return fallback
	}
	return time.Duration(parsed) * time.Millisecond
}
