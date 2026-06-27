package webapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
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
