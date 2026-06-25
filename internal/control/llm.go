package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/logrecord"
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

type llmStatsKey struct {
	modelName    string
	modelVersion string
	workerID     string
}

type llmWorkerStats struct {
	RequestCount          uint64
	CacheHitCount         uint64
	EWMATotalLatencyMs    int64
	EWMAModelLoadMs       int64
	EWMACheckpointFetchMs int64
	LastEvictionCount     int64
	LastUpdatedMs         int64
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
	registeredModel := s.meta.RegisterModel(registered)
	if err := s.metadataPersisted(); err != nil {
		return nil, err
	}
	return &logservepb.RegisterModelResponse{Model: registeredModel}, nil
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
	out := &logservepb.ReplayLLMResponse{TaskId: req.GetTaskId()}
	if err := s.forEachRawLogRecord(ctx, llmStream(req.GetTaskId()), 1, func(rec logrecord.RawRecord) error {
		var payload llmEventPayload
		if len(rec.Payload) > 0 {
			if err := json.Unmarshal(rec.Payload, &payload); err != nil {
				return err
			}
		}
		if payload.TimestampMs == 0 {
			payload.TimestampMs = rec.TimestampMs
		}
		event := &logservepb.LLMEvent{
			EventType:          rec.EventType,
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
		if rec.EventType == "ModelLoaded" {
			out.CacheHit = payload.CacheHit
			out.CheckpointFetchMs = payload.CheckpointFetchMs
			out.CacheUsedBytes = payload.CacheUsedBytes
			out.CacheCapacityBytes = payload.CacheCapacityBytes
			out.EvictionCount = payload.EvictionCount
			out.ModelLoadMs = payload.ModelLoadMs
		}
		if rec.EventType == "LLMCompleted" {
			out.CacheHit = payload.CacheHit
			out.CheckpointFetchMs = payload.CheckpointFetchMs
			out.CacheUsedBytes = payload.CacheUsedBytes
			out.CacheCapacityBytes = payload.CacheCapacityBytes
			out.EvictionCount = payload.EvictionCount
			out.FirstTokenMs = payload.FirstTokenMs
			out.TotalLatencyMs = payload.TotalLatencyMs
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) canAssignTaskToWorker(taskID string, spec *logservepb.TaskSpec, workerID string) bool {
	if s.useSchedulerV2() {
		return s.canAssignTaskToWorkerIndexed(taskID, spec, workerID)
	}
	if spec.GetActorId() == "" && spec.GetTargetWorkerId() != "" && spec.GetTargetWorkerId() != workerID {
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

func (s *Service) canAssignTaskToWorkerIndexed(taskID string, spec *logservepb.TaskSpec, workerID string) bool {
	if spec.GetActorId() == "" && spec.GetTargetWorkerId() != "" && spec.GetTargetWorkerId() != workerID {
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
		return true
	}
}

func (s *Service) preferredLLMWorker(taskID string, spec *logservepb.TaskSpec) string {
	createdAtMs := int64(0)
	if task, ok := s.meta.GetTask(taskID); ok {
		createdAtMs = task.CreatedAtMs
	}
	key := modelKeyFromParts(spec.GetLlmModelName(), spec.GetLlmModelVersion())
	if s.scheduler != nil {
		return s.scheduler.PreferredLocalityWorker(
			key,
			createdAtMs,
			time.Now().UnixMilli(),
			localityQueueWait.Milliseconds(),
		)
	}
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

func (s *Service) predictedLLMWorker(taskID string, spec *logservepb.TaskSpec) string {
	modelName := spec.GetLlmModelName()
	modelVersion := spec.GetLlmModelVersion()
	modelKey := metadata.ModelKey(modelName, modelVersion)
	task, _ := s.meta.GetTask(taskID)
	queueDelayMs := int64(0)
	if task.CreatedAtMs > 0 {
		queueDelayMs = time.Since(time.UnixMilli(task.CreatedAtMs)).Milliseconds()
	}
	if s.scheduler != nil {
		if s.scheduler.WorkerCount() == 0 {
			s.syncActiveSchedulerWorkers()
		}
		return s.scheduler.PreferredPredictedWorker(
			modelKeyFromParts(modelName, modelVersion),
			modelKey,
			queueDelayMs,
			localityQueueWait.Milliseconds(),
		)
	}
	workers := s.meta.ActiveWorkers(schedulerWorkerLease)
	if len(workers) == 0 {
		return ""
	}
	bestWorker := ""
	bestPrediction := int64(1<<62 - 1)
	for _, worker := range workers {
		if !workerHasCapacity(worker) {
			continue
		}
		stats, _ := s.llmStatsForWorker(modelName, modelVersion, worker.WorkerID)
		predicted := predictedLatencyMs(worker, modelKey, stats)
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
	base := int64(25)
	if stats.RequestCount > 0 && stats.EWMATotalLatencyMs > 0 {
		base = stats.EWMATotalLatencyMs
	} else if !worker.CachedModels[modelKey] {
		base = 25
	}

	coldStartPenalty := int64(0)
	if !worker.CachedModels[modelKey] {
		coldStartPenalty = stats.EWMAModelLoadMs + stats.EWMACheckpointFetchMs
		if coldStartPenalty <= 0 {
			coldStartPenalty = 100
		}
	}

	evictionPenalty := int64(0)
	if stats.LastEvictionCount > 0 {
		evictionPenalty = 25 * stats.LastEvictionCount
	}
	return base + coldStartPenalty + evictionPenalty
}

func (s *Service) materializedLLMStats(modelName, modelVersion string) map[string]llmWorkerStats {
	if modelVersion == "" {
		modelVersion = "v1"
	}
	out := make(map[string]llmWorkerStats)
	s.llmStatsMu.RLock()
	defer s.llmStatsMu.RUnlock()
	for key, stats := range s.llmStats {
		if key.modelName == modelName && key.modelVersion == modelVersion {
			out[key.workerID] = stats
		}
	}
	return out
}

func (s *Service) llmStatsForWorker(modelName, modelVersion, workerID string) (llmWorkerStats, bool) {
	if modelVersion == "" {
		modelVersion = "v1"
	}
	s.llmStatsMu.RLock()
	defer s.llmStatsMu.RUnlock()
	stats, ok := s.llmStats[llmStatsKey{
		modelName:    modelName,
		modelVersion: modelVersion,
		workerID:     workerID,
	}]
	return stats, ok
}

func (s *Service) materializeLLMTaskCompletion(ctx context.Context, taskID string) error {
	var done bool
	return s.forEachRawLogRecord(ctx, llmStream(taskID), 1, func(rec logrecord.RawRecord) error {
		if done || rec.EventType != "LLMCompleted" {
			return nil
		}
		var payload llmEventPayload
		if err := json.Unmarshal(rec.Payload, &payload); err != nil {
			return err
		}
		if payload.TimestampMs == 0 {
			payload.TimestampMs = rec.TimestampMs
		}
		s.materializeLLMCompleted(payload)
		done = true
		return nil
	})
}

func (s *Service) materializeLLMCompleted(payload llmEventPayload) {
	if payload.ModelName == "" || payload.WorkerID == "" || payload.TotalLatencyMs <= 0 {
		return
	}
	modelVersion := firstNonEmpty(payload.ModelVersion, "v1")
	updatedAt := payload.TimestampMs
	if updatedAt == 0 {
		updatedAt = time.Now().UnixMilli()
	}
	key := llmStatsKey{
		modelName:    payload.ModelName,
		modelVersion: modelVersion,
		workerID:     payload.WorkerID,
	}

	s.llmStatsMu.Lock()
	defer s.llmStatsMu.Unlock()
	stats := s.llmStats[key]
	stats.RequestCount++
	if payload.CacheHit {
		stats.CacheHitCount++
	}
	stats.EWMATotalLatencyMs = updateEWMA(stats.EWMATotalLatencyMs, payload.TotalLatencyMs, stats.RequestCount)
	stats.EWMAModelLoadMs = updateEWMA(stats.EWMAModelLoadMs, payload.ModelLoadMs, stats.RequestCount)
	stats.EWMACheckpointFetchMs = updateEWMA(stats.EWMACheckpointFetchMs, payload.CheckpointFetchMs, stats.RequestCount)
	stats.LastEvictionCount = payload.EvictionCount
	stats.LastUpdatedMs = updatedAt
	s.llmStats[key] = stats
	if s.scheduler != nil {
		s.scheduler.UpdateLLMStats(payload.ModelName, modelVersion, payload.WorkerID, stats)
	}
}

func updateEWMA(previous, sample int64, count uint64) int64 {
	if count <= 1 {
		return sample
	}
	return (previous*7 + sample*3) / 10
}

func (s *Service) bootstrapLLMStats(ctx context.Context) error {
	streams, err := s.listStreams(ctx, "llm:")
	if err != nil {
		return err
	}
	sort.Strings(streams)
	s.llmStatsMu.Lock()
	s.llmStats = make(map[llmStatsKey]llmWorkerStats)
	s.llmStatsMu.Unlock()
	for _, streamID := range streams {
		if err := s.forEachRawLogRecord(ctx, streamID, 1, func(rec logrecord.RawRecord) error {
			if rec.EventType != "LLMCompleted" {
				return nil
			}
			var payload llmEventPayload
			if err := json.Unmarshal(rec.Payload, &payload); err != nil {
				return err
			}
			if payload.TimestampMs == 0 {
				payload.TimestampMs = rec.TimestampMs
			}
			s.materializeLLMCompleted(payload)
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
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
