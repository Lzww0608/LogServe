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
	TaskID         string `json:"task_id,omitempty"`
	ModelName      string `json:"model_name,omitempty"`
	ModelVersion   string `json:"model_version,omitempty"`
	WorkerID       string `json:"worker_id,omitempty"`
	CacheHit       bool   `json:"cache_hit,omitempty"`
	ModelLoadMs    int64  `json:"model_load_ms,omitempty"`
	FirstTokenMs   int64  `json:"first_token_ms,omitempty"`
	TotalLatencyMs int64  `json:"total_latency_ms,omitempty"`
	TimestampMs    int64  `json:"timestamp_ms,omitempty"`
}

func (s *Service) RegisterModel(ctx context.Context, req *logservepb.RegisterModelRequest) (*logservepb.RegisterModelResponse, error) {
	model := req.GetModel()
	if model.GetName() == "" {
		return nil, errors.New("model.name is required")
	}
	registered := s.meta.RegisterModel(model)
	payload, _ := json.Marshal(registered)
	_, _ = s.appendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       "system:models",
		EventType:      "ModelRegistered",
		IdempotencyKey: metadata.ModelKey(registered.GetName(), registered.GetVersion()) + ":registered",
		Payload:        payload,
	})
	return &logservepb.RegisterModelResponse{Model: registered}, nil
}

func (s *Service) SetSchedulingPolicy(ctx context.Context, req *logservepb.SetSchedulingPolicyRequest) (*logservepb.SetSchedulingPolicyResponse, error) {
	policy := req.GetPolicy()
	if policy == logservepb.SchedulingPolicy_SCHEDULING_POLICY_UNSPECIFIED {
		policy = logservepb.SchedulingPolicy_SCHEDULING_POLICY_LOCALITY_AWARE
	}
	s.configMu.Lock()
	s.schedulingPolicy = policy
	s.configMu.Unlock()
	payload, _ := json.Marshal(map[string]any{
		"policy":       policy.String(),
		"timestamp_ms": time.Now().UnixMilli(),
	})
	_, _ = s.appendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       "system:scheduler",
		EventType:      "SchedulingPolicyChanged",
		IdempotencyKey: fmt.Sprintf("policy:%d:%d", policy, time.Now().UnixNano()),
		Payload:        payload,
	})
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
			EventType:      rec.GetEventType(),
			TimestampMs:    payload.TimestampMs,
			TaskId:         payload.TaskID,
			ModelName:      payload.ModelName,
			ModelVersion:   payload.ModelVersion,
			WorkerId:       payload.WorkerID,
			CacheHit:       payload.CacheHit,
			ModelLoadMs:    payload.ModelLoadMs,
			FirstTokenMs:   payload.FirstTokenMs,
			TotalLatencyMs: payload.TotalLatencyMs,
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
			out.ModelLoadMs = payload.ModelLoadMs
		}
		if rec.GetEventType() == "LLMCompleted" {
			out.CacheHit = payload.CacheHit
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
	if s.getSchedulingPolicy() == logservepb.SchedulingPolicy_SCHEDULING_POLICY_RESOURCE_ONLY {
		return true
	}
	preferred := s.preferredLLMWorker(taskID, spec)
	return preferred == "" || preferred == workerID
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
