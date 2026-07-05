package webapi

// This file defines the stable JSON DTO contract returned to the console and
// the protobuf conversion helpers that populate those DTOs.

import (
	"encoding/json"

	"github.com/logserve/logserve/gen/logservepb"
)

// DashboardDTO is the aggregated console snapshot returned by dashboard and SSE
// endpoints. Slice fields are intentionally emitted as empty arrays rather than
// null so the frontend can render tables without per-field nil checks.
type DashboardDTO struct {
	QueueDepth                uint32                   `json:"queue_depth"`
	QueueHighWatermark        uint32                   `json:"queue_high_watermark"`
	RedeliveryTimeoutMs       int64                    `json:"redelivery_timeout_ms"`
	SchedulingPolicy          string                   `json:"scheduling_policy"`
	Tasks                     []TaskDTO                `json:"tasks"`
	Workflows                 []WorkflowDTO            `json:"workflows"`
	Actors                    []ActorDTO               `json:"actors"`
	Workers                   []WorkerDTO              `json:"workers"`
	Models                    []ModelDTO               `json:"models"`
	LastLogAppendMs           int64                    `json:"last_log_append_ms"`
	LogAppendSlowMs           int64                    `json:"log_append_slow_ms"`
	CompactableLogRecords     uint64                   `json:"compactable_log_records"`
	CompactableLogBytes       uint64                   `json:"compactable_log_bytes"`
	MetadataMaterializerStats *MetadataMaterializerDTO `json:"metadata_materializer,omitempty"`
}

// TaskDTO is the frontend shape for task state from dashboard, status, and wait
// responses. Result stays as raw JSON because task outputs can be arbitrary JSON
// documents and should not be coerced through Go map decoding.
type TaskDTO struct {
	TaskID          string          `json:"task_id"`
	TaskName        string          `json:"task_name,omitempty"`
	Status          string          `json:"status"`
	WorkerID        string          `json:"worker_id,omitempty"`
	WorkflowID      string          `json:"workflow_id,omitempty"`
	StepID          string          `json:"step_id,omitempty"`
	ActorID         string          `json:"actor_id,omitempty"`
	LLMModelName    string          `json:"llm_model_name,omitempty"`
	LLMModelVersion string          `json:"llm_model_version,omitempty"`
	CreatedAtMs     int64           `json:"created_at_ms,omitempty"`
	UpdatedAtMs     int64           `json:"updated_at_ms,omitempty"`
	Result          json.RawMessage `json:"result_json,omitempty"`
	Error           string          `json:"error,omitempty"`
}

// WorkflowDTO is the frontend shape for workflow status plus derived step counts
// used by the DAG view.
type WorkflowDTO struct {
	WorkflowID     string          `json:"workflow_id"`
	WorkflowName   string          `json:"workflow_name,omitempty"`
	Status         string          `json:"status"`
	Steps          []StepDTO       `json:"steps,omitempty"`
	Result         json.RawMessage `json:"result_json,omitempty"`
	ResultRef      string          `json:"result_ref,omitempty"`
	Error          string          `json:"error,omitempty"`
	CreatedAtMs    int64           `json:"created_at_ms,omitempty"`
	UpdatedAtMs    int64           `json:"updated_at_ms,omitempty"`
	CompletedAtMs  int64           `json:"completed_at_ms,omitempty"`
	LatencyMs      int64           `json:"latency_ms,omitempty"`
	StepCount      int             `json:"step_count"`
	SucceededSteps int             `json:"succeeded_steps"`
	FailedSteps    int             `json:"failed_steps"`
	RunningSteps   int             `json:"running_steps"`
}

// StepDTO is the frontend representation of one workflow step and its dependency
// metadata.
type StepDTO struct {
	StepID        string          `json:"step_id"`
	DependsOn     []string        `json:"depends_on,omitempty"`
	TaskName      string          `json:"task_name,omitempty"`
	Status        string          `json:"status"`
	Attempts      uint32          `json:"attempts,omitempty"`
	TaskID        string          `json:"task_id,omitempty"`
	Result        json.RawMessage `json:"result_json,omitempty"`
	ResultRef     string          `json:"result_ref,omitempty"`
	Error         string          `json:"error,omitempty"`
	StartedAtMs   int64           `json:"started_at_ms,omitempty"`
	CompletedAtMs int64           `json:"completed_at_ms,omitempty"`
	LatencyMs     int64           `json:"latency_ms,omitempty"`
}

// ActorDTO represents actor state, actor call results, and replay consistency
// metadata in one console-friendly shape.
type ActorDTO struct {
	ActorID                string          `json:"actor_id"`
	CallID                 string          `json:"call_id,omitempty"`
	ClassName              string          `json:"class_name,omitempty"`
	Status                 string          `json:"status"`
	OwnerWorkerID          string          `json:"owner_worker_id,omitempty"`
	Epoch                  uint64          `json:"epoch,omitempty"`
	CommandCount           uint64          `json:"command_count,omitempty"`
	SnapshotRef            string          `json:"snapshot_ref,omitempty"`
	SnapshotCommandCount   uint64          `json:"snapshot_command_count,omitempty"`
	State                  json.RawMessage `json:"state_json,omitempty"`
	Result                 json.RawMessage `json:"result_json,omitempty"`
	Error                  string          `json:"error,omitempty"`
	CreatedAtMs            int64           `json:"created_at_ms,omitempty"`
	UpdatedAtMs            int64           `json:"updated_at_ms,omitempty"`
	Consistent             bool            `json:"consistent_with_metadata,omitempty"`
	FullReplayCommands     uint64          `json:"full_replay_commands,omitempty"`
	SnapshotReplayCommands uint64          `json:"snapshot_replay_commands,omitempty"`
}

// ModelDTO is the registered model metadata exposed to the console.
type ModelDTO struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	SizeBytes uint64 `json:"size_bytes,omitempty"`
	Path      string `json:"path,omitempty"`
	Adapter   string `json:"adapter,omitempty"`
}

// WorkerDTO is the worker heartbeat/capacity view returned by dashboard.
type WorkerDTO struct {
	WorkerID        string          `json:"worker_id"`
	Capacity        uint32          `json:"capacity"`
	RunningTasks    uint32          `json:"running_tasks"`
	CachedModels    []ModelCacheDTO `json:"cached_models,omitempty"`
	LastHeartbeatMs int64           `json:"last_heartbeat_ms,omitempty"`
}

// ModelCacheDTO identifies one model cached by a worker.
type ModelCacheDTO struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// MetadataMaterializerDTO surfaces async materializer counters and lag estimates
// from the control-plane dashboard snapshot.
type MetadataMaterializerDTO struct {
	Mode                  string `json:"mode,omitempty"`
	PendingDeltas         uint64 `json:"pending_deltas,omitempty"`
	QueuedDeltas          uint64 `json:"queued_deltas,omitempty"`
	BatchMax              uint32 `json:"batch_max,omitempty"`
	FlushIntervalMs       int64  `json:"flush_interval_ms,omitempty"`
	FlushCount            uint64 `json:"flush_count,omitempty"`
	FlushErrorCount       uint64 `json:"flush_error_count,omitempty"`
	LastFlushAtMs         int64  `json:"last_flush_at_ms,omitempty"`
	LastSuccessAtMs       int64  `json:"last_success_at_ms,omitempty"`
	LastErrorAtMs         int64  `json:"last_error_at_ms,omitempty"`
	LastFlushDurationMs   int64  `json:"last_flush_duration_ms,omitempty"`
	LastFlushDeltas       uint64 `json:"last_flush_deltas,omitempty"`
	LastError             string `json:"last_error,omitempty"`
	EventualLagEstimateMs int64  `json:"eventual_lag_estimate_ms,omitempty"`
}

// LLMDTO represents LLM task submission, replay, cache, and latency fields in a
// single response shape.
type LLMDTO struct {
	TaskID             string          `json:"task_id,omitempty"`
	Status             string          `json:"status,omitempty"`
	Result             json.RawMessage `json:"result_json,omitempty"`
	Error              string          `json:"error,omitempty"`
	WorkerID           string          `json:"worker_id,omitempty"`
	ModelName          string          `json:"model_name,omitempty"`
	ModelVersion       string          `json:"model_version,omitempty"`
	CacheHit           bool            `json:"cache_hit,omitempty"`
	ModelLoadMs        int64           `json:"model_load_ms,omitempty"`
	CheckpointFetchMs  int64           `json:"checkpoint_fetch_ms,omitempty"`
	FirstTokenMs       int64           `json:"first_token_ms,omitempty"`
	TotalLatencyMs     int64           `json:"total_latency_ms,omitempty"`
	CacheUsedBytes     int64           `json:"cache_used_bytes,omitempty"`
	CacheCapacityBytes int64           `json:"cache_capacity_bytes,omitempty"`
	EvictionCount      int64           `json:"eviction_count,omitempty"`
	Events             []LLMEventDTO   `json:"events,omitempty"`
}

// LLMEventDTO is one replayed LLM lifecycle/cache event.
type LLMEventDTO struct {
	EventType          string `json:"event_type,omitempty"`
	TimestampMs        int64  `json:"timestamp_ms,omitempty"`
	TaskID             string `json:"task_id,omitempty"`
	ModelName          string `json:"model_name,omitempty"`
	ModelVersion       string `json:"model_version,omitempty"`
	WorkerID           string `json:"worker_id,omitempty"`
	CacheHit           bool   `json:"cache_hit,omitempty"`
	ModelLoadMs        int64  `json:"model_load_ms,omitempty"`
	FirstTokenMs       int64  `json:"first_token_ms,omitempty"`
	TotalLatencyMs     int64  `json:"total_latency_ms,omitempty"`
	CheckpointFetchMs  int64  `json:"checkpoint_fetch_ms,omitempty"`
	CacheUsedBytes     int64  `json:"cache_used_bytes,omitempty"`
	CacheCapacityBytes int64  `json:"cache_capacity_bytes,omitempty"`
	EvictionCount      int64  `json:"eviction_count,omitempty"`
}

// dashboardDTO converts a protobuf dashboard snapshot into non-nil JSON slices so
// empty collections serialize as arrays rather than null.
func dashboardDTO(snapshot *logservepb.DashboardSnapshot) DashboardDTO {
	// Pre-seed slices before the nil check so an unavailable dashboard still has
	// the same JSON shape as an empty dashboard.
	out := DashboardDTO{
		Tasks:     []TaskDTO{},
		Workflows: []WorkflowDTO{},
		Actors:    []ActorDTO{},
		Workers:   []WorkerDTO{},
		Models:    []ModelDTO{},
	}
	if snapshot == nil {
		return out
	}
	out.QueueDepth = snapshot.GetQueueDepth()
	out.QueueHighWatermark = snapshot.GetQueueHighWatermark()
	out.RedeliveryTimeoutMs = snapshot.GetRedeliveryTimeoutMs()
	out.SchedulingPolicy = schedulingPolicyString(snapshot.GetSchedulingPolicy())
	out.LastLogAppendMs = snapshot.GetLastLogAppendMs()
	out.LogAppendSlowMs = snapshot.GetLogAppendSlowMs()
	out.CompactableLogRecords = snapshot.GetCompactableLogRecords()
	out.CompactableLogBytes = snapshot.GetCompactableLogBytes()
	for _, task := range snapshot.GetTasks() {
		out.Tasks = append(out.Tasks, dashboardTaskDTO(task))
	}
	for _, workflow := range snapshot.GetWorkflows() {
		out.Workflows = append(out.Workflows, dashboardWorkflowDTO(workflow))
	}
	for _, actor := range snapshot.GetActors() {
		out.Actors = append(out.Actors, actorStatusDTO(actor))
	}
	for _, worker := range snapshot.GetWorkers() {
		out.Workers = append(out.Workers, workerDTO(worker))
	}
	for _, model := range snapshot.GetModels() {
		out.Models = append(out.Models, modelDTO(model))
	}
	if stats := snapshot.GetMetadataMaterializer(); stats != nil {
		out.MetadataMaterializerStats = &MetadataMaterializerDTO{
			Mode:                  stats.GetMode(),
			PendingDeltas:         stats.GetPendingDeltas(),
			QueuedDeltas:          stats.GetQueuedDeltas(),
			BatchMax:              stats.GetBatchMax(),
			FlushIntervalMs:       stats.GetFlushIntervalMs(),
			FlushCount:            stats.GetFlushCount(),
			FlushErrorCount:       stats.GetFlushErrorCount(),
			LastFlushAtMs:         stats.GetLastFlushAtMs(),
			LastSuccessAtMs:       stats.GetLastSuccessAtMs(),
			LastErrorAtMs:         stats.GetLastErrorAtMs(),
			LastFlushDurationMs:   stats.GetLastFlushDurationMs(),
			LastFlushDeltas:       stats.GetLastFlushDeltas(),
			LastError:             stats.GetLastError(),
			EventualLagEstimateMs: stats.GetEventualLagEstimateMs(),
		}
	}
	return out
}

// dashboardTaskDTO converts lightweight dashboard task metadata into TaskDTO.
func dashboardTaskDTO(task *logservepb.DashboardTask) TaskDTO {
	return TaskDTO{
		TaskID:          task.GetTaskId(),
		TaskName:        task.GetTaskName(),
		Status:          taskStatusString(task.GetStatus()),
		WorkerID:        task.GetWorkerId(),
		WorkflowID:      task.GetWorkflowId(),
		StepID:          task.GetStepId(),
		ActorID:         task.GetActorId(),
		LLMModelName:    task.GetLlmModelName(),
		LLMModelVersion: task.GetLlmModelVersion(),
		CreatedAtMs:     task.GetCreatedAtMs(),
		UpdatedAtMs:     task.GetUpdatedAtMs(),
	}
}

// taskStatusDTO converts a task status RPC response into TaskDTO with owned JSON
// result bytes.
func taskStatusDTO(resp *logservepb.GetTaskStatusResponse) TaskDTO {
	return TaskDTO{
		TaskID:      resp.GetTaskId(),
		Status:      taskStatusString(resp.GetStatus()),
		WorkerID:    resp.GetWorkerId(),
		CreatedAtMs: resp.GetCreatedAtMs(),
		UpdatedAtMs: resp.GetUpdatedAtMs(),
		Result:      jsonOrNil(resp.GetResultJson()),
		Error:       resp.GetError(),
	}
}

// dashboardWorkflowDTO converts dashboard workflow metadata and derives step
// status counters for summary cards.
func dashboardWorkflowDTO(wf *logservepb.DashboardWorkflow) WorkflowDTO {
	out := WorkflowDTO{
		WorkflowID:   wf.GetWorkflowId(),
		WorkflowName: wf.GetWorkflowName(),
		Status:       workflowStatusString(wf.GetStatus()),
	}
	for _, step := range wf.GetSteps() {
		dto := stepDTO(step)
		// Counters are derived from normalized DTO strings so dashboard and detail
		// responses use the same status vocabulary.
		out.Steps = append(out.Steps, dto)
		out.StepCount++
		switch dto.Status {
		case "SUCCEEDED":
			out.SucceededSteps++
		case "FAILED":
			out.FailedSteps++
		case "STARTED":
			out.RunningSteps++
		}
	}
	return out
}

// workflowStatusDTO converts the detailed workflow status response and derives
// step counters for the console.
func workflowStatusDTO(resp *logservepb.GetWorkflowStatusResponse) WorkflowDTO {
	out := WorkflowDTO{
		WorkflowID:    resp.GetWorkflowId(),
		WorkflowName:  resp.GetWorkflowName(),
		Status:        workflowStatusString(resp.GetStatus()),
		Result:        jsonOrNil(resp.GetResultJson()),
		ResultRef:     resp.GetResultRef(),
		Error:         resp.GetError(),
		CreatedAtMs:   resp.GetCreatedAtMs(),
		UpdatedAtMs:   resp.GetUpdatedAtMs(),
		CompletedAtMs: resp.GetCompletedAtMs(),
		LatencyMs:     resp.GetLatencyMs(),
	}
	for _, step := range resp.GetSteps() {
		dto := stepDTO(step)
		out.Steps = append(out.Steps, dto)
		out.StepCount++
		switch dto.Status {
		case "SUCCEEDED":
			out.SucceededSteps++
		case "FAILED":
			out.FailedSteps++
		case "STARTED":
			out.RunningSteps++
		}
	}
	return out
}

// stepDTO converts one protobuf workflow step and copies dependency slices for
// caller ownership.
func stepDTO(step *logservepb.WorkflowStepState) StepDTO {
	// Copy dependency slices so later protobuf reuse or mutation cannot affect the
	// already-built response object.
	return StepDTO{
		StepID:        step.GetStepId(),
		DependsOn:     append([]string(nil), step.GetDependsOn()...),
		TaskName:      step.GetTaskName(),
		Status:        workflowStepStatusString(step.GetStatus()),
		Attempts:      step.GetAttempts(),
		TaskID:        step.GetTaskId(),
		Result:        jsonOrNil(step.GetResultJson()),
		ResultRef:     step.GetResultRef(),
		Error:         step.GetError(),
		StartedAtMs:   step.GetStartedAtMs(),
		CompletedAtMs: step.GetCompletedAtMs(),
		LatencyMs:     step.GetLatencyMs(),
	}
}

// actorStatusDTO converts actor status RPC fields into the shared ActorDTO shape.
func actorStatusDTO(resp *logservepb.GetActorStatusResponse) ActorDTO {
	return ActorDTO{
		ActorID:              resp.GetActorId(),
		ClassName:            resp.GetClassName(),
		Status:               actorStatusString(resp.GetStatus()),
		OwnerWorkerID:        resp.GetOwnerWorkerId(),
		Epoch:                resp.GetEpoch(),
		CommandCount:         resp.GetCommandCount(),
		SnapshotRef:          resp.GetSnapshotRef(),
		SnapshotCommandCount: resp.GetSnapshotCommandCount(),
		State:                jsonOrNil(resp.GetStateJson()),
		CreatedAtMs:          resp.GetCreatedAtMs(),
		UpdatedAtMs:          resp.GetUpdatedAtMs(),
	}
}

// actorCallDTO converts an actor call response into the same DTO used for actor
// status rows.
func actorCallDTO(resp *logservepb.CallActorResponse) ActorDTO {
	return ActorDTO{
		ActorID: resp.GetActorId(),
		CallID:  resp.GetCallId(),
		Status:  taskStatusString(resp.GetStatus()),
		Result:  jsonOrNil(resp.GetResultJson()),
		Error:   resp.GetError(),
		Epoch:   resp.GetEpoch(),
	}
}

// modelDTO converts model registry protobuf metadata into JSON fields.
func modelDTO(model *logservepb.ModelInfo) ModelDTO {
	return ModelDTO{
		Name:      model.GetName(),
		Version:   model.GetVersion(),
		SizeBytes: model.GetSizeBytes(),
		Path:      model.GetPath(),
		Adapter:   model.GetAdapter(),
	}
}

// workerDTO converts worker dashboard metadata and cached model entries.
func workerDTO(worker *logservepb.DashboardWorker) WorkerDTO {
	out := WorkerDTO{
		WorkerID:        worker.GetWorkerId(),
		Capacity:        worker.GetCapacity(),
		RunningTasks:    worker.GetRunningTasks(),
		LastHeartbeatMs: worker.GetLastHeartbeatMs(),
	}
	for _, model := range worker.GetCachedModels() {
		out.CachedModels = append(out.CachedModels, ModelCacheDTO{Name: model.GetName(), Version: model.GetVersion()})
	}
	return out
}

// llmReplayDTO converts replay output into the LLMDTO shape used by the console
// timeline and cache panels.
func llmReplayDTO(resp *logservepb.ReplayLLMResponse) LLMDTO {
	// Replay fields mirror the worker's lifecycle events rather than live task
	// status, which keeps cache/latency diagnostics stable after completion.
	out := LLMDTO{
		TaskID:             resp.GetTaskId(),
		ModelName:          resp.GetModelName(),
		ModelVersion:       resp.GetModelVersion(),
		WorkerID:           resp.GetWorkerId(),
		CacheHit:           resp.GetCacheHit(),
		ModelLoadMs:        resp.GetModelLoadMs(),
		CheckpointFetchMs:  resp.GetCheckpointFetchMs(),
		FirstTokenMs:       resp.GetFirstTokenMs(),
		TotalLatencyMs:     resp.GetTotalLatencyMs(),
		CacheUsedBytes:     resp.GetCacheUsedBytes(),
		CacheCapacityBytes: resp.GetCacheCapacityBytes(),
		EvictionCount:      resp.GetEvictionCount(),
	}
	for _, event := range resp.GetEvents() {
		out.Events = append(out.Events, LLMEventDTO{
			EventType:          event.GetEventType(),
			TimestampMs:        event.GetTimestampMs(),
			TaskID:             event.GetTaskId(),
			ModelName:          event.GetModelName(),
			ModelVersion:       event.GetModelVersion(),
			WorkerID:           event.GetWorkerId(),
			CacheHit:           event.GetCacheHit(),
			ModelLoadMs:        event.GetModelLoadMs(),
			FirstTokenMs:       event.GetFirstTokenMs(),
			TotalLatencyMs:     event.GetTotalLatencyMs(),
			CheckpointFetchMs:  event.GetCheckpointFetchMs(),
			CacheUsedBytes:     event.GetCacheUsedBytes(),
			CacheCapacityBytes: event.GetCacheCapacityBytes(),
			EvictionCount:      event.GetEvictionCount(),
		})
	}
	return out
}
