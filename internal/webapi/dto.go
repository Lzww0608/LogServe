package webapi

import (
	"encoding/json"

	"github.com/logserve/logserve/gen/logservepb"
)

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

type StepDTO struct {
	StepID        string          `json:"step_id"`
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

type ModelDTO struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	SizeBytes uint64 `json:"size_bytes,omitempty"`
	Path      string `json:"path,omitempty"`
	Adapter   string `json:"adapter,omitempty"`
}

type WorkerDTO struct {
	WorkerID        string          `json:"worker_id"`
	Capacity        uint32          `json:"capacity"`
	RunningTasks    uint32          `json:"running_tasks"`
	CachedModels    []ModelCacheDTO `json:"cached_models,omitempty"`
	LastHeartbeatMs int64           `json:"last_heartbeat_ms,omitempty"`
}

type ModelCacheDTO struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

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

func dashboardDTO(snapshot *logservepb.DashboardSnapshot) DashboardDTO {
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

func dashboardWorkflowDTO(wf *logservepb.DashboardWorkflow) WorkflowDTO {
	out := WorkflowDTO{
		WorkflowID:   wf.GetWorkflowId(),
		WorkflowName: wf.GetWorkflowName(),
		Status:       workflowStatusString(wf.GetStatus()),
	}
	for _, step := range wf.GetSteps() {
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

func stepDTO(step *logservepb.WorkflowStepState) StepDTO {
	return StepDTO{
		StepID:        step.GetStepId(),
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

func modelDTO(model *logservepb.ModelInfo) ModelDTO {
	return ModelDTO{
		Name:      model.GetName(),
		Version:   model.GetVersion(),
		SizeBytes: model.GetSizeBytes(),
		Path:      model.GetPath(),
		Adapter:   model.GetAdapter(),
	}
}

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

func llmReplayDTO(resp *logservepb.ReplayLLMResponse) LLMDTO {
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
