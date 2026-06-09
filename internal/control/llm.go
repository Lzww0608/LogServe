package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/metadata"
)

type llmEventPayload struct {
	TaskID             string `json:"task_id,omitempty"`
	ModelName          string `json:"model_name,omitempty"`
	ModelVersion       string `json:"model_version,omitempty"`
	WorkerID           string `json:"worker_id,omitempty"`
	CacheHit           bool   `json:"cache_hit,omitempty"`
	CheckpointFetchMs  int64  `json:"checkpoint_fetch_ms,omitempty"`
	CacheUsedBytes     int64  `json:"cache_used_bytes,omitempty"`
	CacheCapacityBytes int64  `json:"cache_capacity_bytes,omitempty"`
	EvictionCount      int64  `json:"eviction_count,omitempty"`
	ModelLoadMs        int64  `json:"model_load_ms,omitempty"`
	FirstTokenMs       int64  `json:"first_token_ms,omitempty"`
	TotalLatencyMs     int64  `json:"total_latency_ms,omitempty"`
	TimestampMs        int64  `json:"timestamp_ms,omitempty"`
}

func (s *Service) RegisterModel(ctx context.Context, req *logservepb.RegisterModelRequest) (*logservepb.RegisterModelResponse, error) {
	model := req.GetModel()
	if model.GetName() == "" {
		return nil, errors.New("model.name is required")
	}
	registered := normalizeModelInfo(model)
	payload, _ := json.Marshal(registered)
	if _, err := s.appendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       "system:models",
		EventType:      "ModelRegistered",
		IdempotencyKey: fmt.Sprintf("%s:registered:%d", metadata.ModelKey(registered.GetName(), registered.GetVersion()), time.Now().UnixNano()),
		Payload:        payload,
	}); err != nil {
		return nil, err
	}
	return &logservepb.RegisterModelResponse{Model: s.meta.RegisterModel(registered)}, nil
}

func (s *Service) SetSchedulingPolicy(ctx context.Context, req *logservepb.SetSchedulingPolicyRequest) (*logservepb.SetSchedulingPolicyResponse, error) {
	policy := req.GetPolicy()
	if policy == logservepb.SchedulingPolicy_SCHEDULING_POLICY_UNSPECIFIED {
		policy = logservepb.SchedulingPolicy_SCHEDULING_POLICY_LOCALITY_AWARE
	}
	payload, _ := json.Marshal(map[string]any{
		"policy":       policy.String(),
		"timestamp_ms": time.Now().UnixMilli(),
	})
	if _, err := s.appendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       "system:scheduler",
		EventType:      "SchedulingPolicyChanged",
		IdempotencyKey: fmt.Sprintf("policy:%d:%d", policy, time.Now().UnixNano()),
		Payload:        payload,
	}); err != nil {
		return nil, err
	}
	s.configMu.Lock()
	s.schedulingPolicy = policy
	s.configMu.Unlock()
	return &logservepb.SetSchedulingPolicyResponse{Policy: policy}, nil
}

func (s *Service) SubmitLLM(ctx context.Context, req *logservepb.SubmitLLMRequest) (*logservepb.SubmitLLMResponse, error) {
	if req.GetModelName() == "" {
		return nil, errors.New("model_name is required")
	}
	if req.GetPrompt() == "" {
		return nil, errors.New("prompt is required")
	}
	version := req.GetModelVersion()
	if version == "" {
		version = "v1"
	}
	adapter := req.GetAdapter()
	model, ok := s.meta.GetModel(req.GetModelName(), version)
	if !ok {
		return nil, fmt.Errorf("model %s:%s is not registered", req.GetModelName(), version)
	}
	if adapter == "" {
		adapter = model.GetAdapter()
	}
	if adapter == "" {
		adapter = "mock"
	}
	maxTokens := req.GetMaxTokens()
	if maxTokens == 0 {
		maxTokens = 64
	}
	argsJSON, err := json.Marshal(map[string]any{
		"args":   []any{req.GetPrompt()},
		"kwargs": map[string]any{},
	})
	if err != nil {
		return nil, err
	}
	task, _, err := s.enqueueTask(ctx, &logservepb.TaskSpec{
		TaskId:          newTaskID(),
		TaskName:        "llm:" + req.GetModelName(),
		FunctionName:    "__logserve_llm__",
		ArgsJson:        argsJSON,
		IdempotencyKey:  req.GetIdempotencyKey(),
		LlmModelName:    req.GetModelName(),
		LlmModelVersion: version,
		LlmAdapter:      adapter,
		LlmMaxTokens:    maxTokens,
	})
	if err != nil {
		return nil, err
	}
	return &logservepb.SubmitLLMResponse{TaskId: task.TaskID, Status: task.Status}, nil
}

func (s *Service) ReplayLLM(ctx context.Context, req *logservepb.ReplayLLMRequest) (*logservepb.ReplayLLMResponse, error) {
	if req.GetTaskId() == "" {
		return nil, errors.New("task_id is required")
	}
	resp, err := s.log.ReadLog(ctx, &logservepb.ReadLogRequest{
		StreamId: llmStream(req.GetTaskId()),
		FromSeq:  1,
		Limit:    1000,
	})
	if err != nil {
		return nil, err
	}
	out := &logservepb.ReplayLLMResponse{TaskId: req.GetTaskId()}
	for _, rec := range resp.GetRecords() {
		var payload llmEventPayload
		if len(rec.GetPayload()) > 0 {
			if err := json.Unmarshal(rec.GetPayload(), &payload); err != nil {
				return nil, err
			}
		}
		if payload.TimestampMs == 0 {
			payload.TimestampMs = rec.GetTimestampMs()
		}
		event := &logservepb.LLMEvent{
			EventType:          rec.GetEventType(),
			TimestampMs:        payload.TimestampMs,
			TaskId:             payload.TaskID,
			ModelName:          payload.ModelName,
			ModelVersion:       payload.ModelVersion,
			WorkerId:           payload.WorkerID,
			CacheHit:           payload.CacheHit,
			CheckpointFetchMs:  payload.CheckpointFetchMs,
			CacheUsedBytes:     payload.CacheUsedBytes,
			CacheCapacityBytes: payload.CacheCapacityBytes,
			EvictionCount:      payload.EvictionCount,
			ModelLoadMs:        payload.ModelLoadMs,
			FirstTokenMs:       payload.FirstTokenMs,
			TotalLatencyMs:     payload.TotalLatencyMs,
		}
		out.Events = append(out.Events, event)
		if payload.ModelName != "" {
			out.ModelName = payload.ModelName
		}
		if payload.ModelVersion != "" {
			out.ModelVersion = payload.ModelVersion
		}
		if payload.WorkerID != "" {
			out.WorkerId = payload.WorkerID
		}
		if rec.GetEventType() == "ModelLoaded" {
			out.CacheHit = payload.CacheHit
			out.CheckpointFetchMs = payload.CheckpointFetchMs
			out.CacheUsedBytes = payload.CacheUsedBytes
			out.CacheCapacityBytes = payload.CacheCapacityBytes
			out.EvictionCount = payload.EvictionCount
			out.ModelLoadMs = payload.ModelLoadMs
		}
		if rec.GetEventType() == "LLMCompleted" {
			out.CacheHit = payload.CacheHit
			out.CheckpointFetchMs = payload.CheckpointFetchMs
			out.CacheUsedBytes = payload.CacheUsedBytes
			out.CacheCapacityBytes = payload.CacheCapacityBytes
			out.EvictionCount = payload.EvictionCount
			out.FirstTokenMs = payload.FirstTokenMs
			out.TotalLatencyMs = payload.TotalLatencyMs
		}
	}
	return out, nil
}

func (s *Service) canAssignTaskToWorker(taskID string, spec *logservepb.TaskSpec, workerID string) bool {
	if spec.GetTargetWorkerId() != "" && spec.GetTargetWorkerId() != workerID {
		return false
	}
	if spec.GetLlmModelName() == "" {
		return true
	}
	worker, ok := s.meta.GetWorker(workerID)
	if !ok || !workerHasCapacity(worker) {
		return false
	}
	switch s.getSchedulingPolicy() {
	case logservepb.SchedulingPolicy_SCHEDULING_POLICY_RESOURCE_ONLY:
		return true
	case logservepb.SchedulingPolicy_SCHEDULING_POLICY_PREDICTED_LATENCY:
		preferred := s.predictedLLMWorker(taskID, spec)
		return preferred == "" || preferred == workerID
	default:
		preferred := s.preferredLLMWorker(taskID, spec)
		return preferred == "" || preferred == workerID
	}
}

func (s *Service) preferredLLMWorker(taskID string, spec *logservepb.TaskSpec) string {
	workers := s.meta.ActiveWorkers(schedulerWorkerLease)
	if len(workers) == 0 {
		return ""
	}
	task, _ := s.meta.GetTask(taskID)
	queueDelay := time.Duration(0)
	if task.CreatedAtMs > 0 {
		queueDelay = time.Since(time.UnixMilli(task.CreatedAtMs))
	}
	modelKey := metadata.ModelKey(spec.GetLlmModelName(), spec.GetLlmModelVersion())
	hasCachedAvailable := false
	for _, worker := range workers {
		if workerHasCapacity(worker) && worker.CachedModels[modelKey] {
			hasCachedAvailable = true
			break
		}
	}

	bestWorker := ""
	bestScore := -1 << 30
	for _, worker := range workers {
		if !workerHasCapacity(worker) {
			continue
		}
		available := int(worker.Capacity - worker.RunningTasks)
		score := available*100 - int(worker.RunningTasks)*10
		cacheHit := worker.CachedModels[modelKey]
		if cacheHit {
			score += 1000
		}
		if !cacheHit && hasCachedAvailable && queueDelay < localityQueueWait {
			score -= 1000
		}
		if bestWorker == "" || score > bestScore || (score == bestScore && worker.WorkerID < bestWorker) {
			bestWorker = worker.WorkerID
			bestScore = score
		}
	}
	return bestWorker
}

type llmWorkerStats struct {
	count          int
	totalLatencyMs int64
}

func (s *Service) predictedLLMWorker(taskID string, spec *logservepb.TaskSpec) string {
	workers := s.meta.ActiveWorkers(schedulerWorkerLease)
	if len(workers) == 0 {
		return ""
	}
	modelKey := metadata.ModelKey(spec.GetLlmModelName(), spec.GetLlmModelVersion())
	stats := s.observedLLMStats(spec.GetLlmModelName(), spec.GetLlmModelVersion())
	task, _ := s.meta.GetTask(taskID)
	queueDelayMs := int64(0)
	if task.CreatedAtMs > 0 {
		queueDelayMs = time.Since(time.UnixMilli(task.CreatedAtMs)).Milliseconds()
	}

	bestWorker := ""
	bestPrediction := int64(1<<62 - 1)
	for _, worker := range workers {
		if !workerHasCapacity(worker) {
			continue
		}
		predicted := predictedLatencyMs(worker, modelKey, stats[worker.WorkerID])
		predicted += int64(worker.RunningTasks) * 50
		if queueDelayMs > localityQueueWait.Milliseconds() && !worker.CachedModels[modelKey] {
			predicted -= 25
		}
		if bestWorker == "" || predicted < bestPrediction || (predicted == bestPrediction && worker.WorkerID < bestWorker) {
			bestWorker = worker.WorkerID
			bestPrediction = predicted
		}
	}
	return bestWorker
}

func predictedLatencyMs(worker metadata.Worker, modelKey string, stats llmWorkerStats) int64 {
	if stats.count > 0 {
		return stats.totalLatencyMs / int64(stats.count)
	}
	if worker.CachedModels[modelKey] {
		return 25
	}
	return 125
}

func (s *Service) observedLLMStats(modelName, modelVersion string) map[string]llmWorkerStats {
	streams, err := s.listStreams(context.Background(), "llm:")
	if err != nil {
		return nil
	}
	out := make(map[string]llmWorkerStats)
	if modelVersion == "" {
		modelVersion = "v1"
	}
	for _, streamID := range streams {
		records, err := s.readAllLog(context.Background(), streamID)
		if err != nil {
			continue
		}
		for _, rec := range records {
			if rec.GetEventType() != "LLMCompleted" {
				continue
			}
			var payload llmEventPayload
			if err := json.Unmarshal(rec.GetPayload(), &payload); err != nil {
				continue
			}
			if payload.ModelName != modelName || firstNonEmpty(payload.ModelVersion, "v1") != modelVersion {
				continue
			}
			if payload.WorkerID == "" || payload.TotalLatencyMs <= 0 {
				continue
			}
			stats := out[payload.WorkerID]
			stats.count++
			stats.totalLatencyMs += payload.TotalLatencyMs
			out[payload.WorkerID] = stats
		}
	}
	return out
}

func workerHasCapacity(worker metadata.Worker) bool {
	capacity := worker.Capacity
	if capacity == 0 {
		capacity = 1
	}
	return worker.RunningTasks < capacity
}

func modelCacheFromProto(entries []*logservepb.ModelCacheEntry) map[string]bool {
	out := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if entry.GetName() == "" {
			continue
		}
		out[metadata.ModelKey(entry.GetName(), entry.GetVersion())] = true
	}
	return out
}

func llmStream(taskID string) string {
	return "llm:" + taskID
}

func normalizeModelInfo(model *logservepb.ModelInfo) *logservepb.ModelInfo {
	clone := &logservepb.ModelInfo{
		Name:      model.GetName(),
		Version:   model.GetVersion(),
		SizeBytes: model.GetSizeBytes(),
		Path:      model.GetPath(),
		Adapter:   model.GetAdapter(),
	}
	if clone.Version == "" {
		clone.Version = "v1"
	}
	if clone.Adapter == "" {
		clone.Adapter = "mock"
	}
	return clone
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
