package control

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/metadata"
)

func (s *Service) SetBackpressure(ctx context.Context, req *logservepb.SetBackpressureRequest) (*logservepb.SetBackpressureResponse, error) {
	s.configMu.RLock()
	queueHighWatermark := s.queueHighWatermark
	redeliveryTimeout := s.redeliveryTimeout
	logAppendSlowLimit := s.logAppendSlowLimit
	s.configMu.RUnlock()

	if req.GetQueueHighWatermark() > 0 {
		queueHighWatermark = req.GetQueueHighWatermark()
	}
	if req.GetRedeliveryTimeoutMs() > 0 {
		redeliveryTimeout = time.Duration(req.GetRedeliveryTimeoutMs()) * time.Millisecond
	}
	if req.GetLogAppendSlowMs() > 0 {
		logAppendSlowLimit = time.Duration(req.GetLogAppendSlowMs()) * time.Millisecond
	}

	payload, _ := json.Marshal(map[string]any{
		"queue_high_watermark":  queueHighWatermark,
		"redelivery_timeout_ms": redeliveryTimeout.Milliseconds(),
		"log_append_slow_ms":    logAppendSlowLimit.Milliseconds(),
		"timestamp_ms":          time.Now().UnixMilli(),
	})
	if _, err := s.appendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       "system:backpressure",
		EventType:      "BackpressureConfigured",
		IdempotencyKey: "backpressure:" + time.Now().Format("150405.000000000"),
		Payload:        payload,
	}); err != nil {
		return nil, err
	}
	s.configMu.Lock()
	s.queueHighWatermark = queueHighWatermark
	s.redeliveryTimeout = redeliveryTimeout
	s.logAppendSlowLimit = logAppendSlowLimit
	s.configMu.Unlock()
	return &logservepb.SetBackpressureResponse{
		QueueHighWatermark:  queueHighWatermark,
		RedeliveryTimeoutMs: redeliveryTimeout.Milliseconds(),
		LogAppendSlowMs:     logAppendSlowLimit.Milliseconds(),
	}, nil
}

func (s *Service) GetDashboardSnapshot(ctx context.Context, req *logservepb.GetDashboardSnapshotRequest) (*logservepb.DashboardSnapshot, error) {
	s.queueMu.Lock()
	queueDepth := uint32(len(s.queue))
	s.queueMu.Unlock()

	tasks := s.meta.ListTasks()
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAtMs < tasks[j].CreatedAtMs })
	dashboardTasks := make([]*logservepb.DashboardTask, 0, len(tasks))
	for _, task := range tasks {
		dashboardTasks = append(dashboardTasks, &logservepb.DashboardTask{
			TaskId:          task.TaskID,
			TaskName:        task.TaskName,
			Status:          task.Status,
			WorkerId:        task.WorkerID,
			WorkflowId:      task.WorkflowID,
			StepId:          task.StepID,
			ActorId:         task.ActorID,
			LlmModelName:    task.LLMModelName,
			LlmModelVersion: task.LLMModelVersion,
			CreatedAtMs:     task.CreatedAtMs,
			UpdatedAtMs:     task.UpdatedAtMs,
		})
	}

	workflows := s.meta.ListWorkflows()
	sort.Slice(workflows, func(i, j int) bool { return workflows[i].CreatedAtMs < workflows[j].CreatedAtMs })
	dashboardWorkflows := make([]*logservepb.DashboardWorkflow, 0, len(workflows))
	for _, workflowState := range workflows {
		status := workflowStatusResponse(workflowState)
		dashboardWorkflows = append(dashboardWorkflows, &logservepb.DashboardWorkflow{
			WorkflowId:   status.GetWorkflowId(),
			WorkflowName: status.GetWorkflowName(),
			Status:       status.GetStatus(),
			Steps:        status.GetSteps(),
		})
	}

	actors := s.meta.ListActors()
	sort.Slice(actors, func(i, j int) bool { return actors[i].CreatedAtMs < actors[j].CreatedAtMs })
	dashboardActors := make([]*logservepb.GetActorStatusResponse, 0, len(actors))
	for _, actorState := range actors {
		dashboardActors = append(dashboardActors, actorStatusResponse(actorState))
	}

	workers := s.meta.ListWorkers()
	sort.Slice(workers, func(i, j int) bool { return workers[i].WorkerID < workers[j].WorkerID })
	dashboardWorkers := make([]*logservepb.DashboardWorker, 0, len(workers))
	for _, worker := range workers {
		dashboardWorkers = append(dashboardWorkers, &logservepb.DashboardWorker{
			WorkerId:        worker.WorkerID,
			Capacity:        worker.Capacity,
			RunningTasks:    worker.RunningTasks,
			CachedModels:    cacheEntries(worker),
			LastHeartbeatMs: worker.LastHeartbeat,
		})
	}

	models := s.meta.ListModels()
	sort.Slice(models, func(i, j int) bool {
		if models[i].GetName() == models[j].GetName() {
			return models[i].GetVersion() < models[j].GetVersion()
		}
		return models[i].GetName() < models[j].GetName()
	})
	queueHighWatermark, redeliveryTimeout, logAppendSlowLimit := s.getBackpressureConfig()
	return &logservepb.DashboardSnapshot{
		QueueDepth:          queueDepth,
		QueueHighWatermark:  queueHighWatermark,
		RedeliveryTimeoutMs: redeliveryTimeout.Milliseconds(),
		SchedulingPolicy:    s.getSchedulingPolicy(),
		Tasks:               dashboardTasks,
		Workflows:           dashboardWorkflows,
		Actors:              dashboardActors,
		Workers:             dashboardWorkers,
		Models:              models,
		LastLogAppendMs:     s.lastLogAppendMs.Load(),
		LogAppendSlowMs:     logAppendSlowLimit.Milliseconds(),
	}, nil
}

func (s *Service) redeliverExpiredTasks(ctx context.Context) error {
	_, redeliveryTimeout, _ := s.getBackpressureConfig()
	if redeliveryTimeout <= 0 {
		return nil
	}

	now := time.Now()
	expired := make([]metadata.Task, 0)
	for _, task := range s.meta.ListTasks() {
		if task.Status != logservepb.TaskStatus_TASK_STATUS_RUNNING {
			continue
		}
		if task.UpdatedAtMs == 0 || now.Sub(time.UnixMilli(task.UpdatedAtMs)) < redeliveryTimeout {
			continue
		}
		expired = append(expired, task)
	}
	if len(expired) == 0 {
		return nil
	}
	for _, task := range expired {
		payload, _ := json.Marshal(map[string]any{
			"task_id":      task.TaskID,
			"task_name":    task.TaskName,
			"timestamp_ms": time.Now().UnixMilli(),
		})
		if _, err := s.appendLog(ctx, &logservepb.AppendLogRequest{
			StreamId:       taskStream(task.TaskID),
			EventType:      "TaskRedelivered",
			IdempotencyKey: task.TaskID + ":redelivered:" + time.Now().Format("150405.000000000"),
			Payload:        payload,
		}); err != nil {
			return err
		}
	}
	requeued := s.meta.RequeueExpiredRunningTasks(redeliveryTimeout)
	s.queueMu.Lock()
	for _, task := range requeued {
		s.queue = append(s.queue, task.TaskID)
		if task.WorkerID != "" {
			s.meta.DecrementWorkerLoad(task.WorkerID)
		}
	}
	s.queueMu.Unlock()
	return nil
}

func cacheEntries(worker metadata.Worker) []*logservepb.ModelCacheEntry {
	entries := make([]*logservepb.ModelCacheEntry, 0, len(worker.CachedModels))
	for key := range worker.CachedModels {
		name, version := splitModelKey(key)
		entries = append(entries, &logservepb.ModelCacheEntry{Name: name, Version: version})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].GetName() == entries[j].GetName() {
			return entries[i].GetVersion() < entries[j].GetVersion()
		}
		return entries[i].GetName() < entries[j].GetName()
	})
	return entries
}

func splitModelKey(key string) (string, string) {
	for i := 0; i < len(key); i++ {
		if key[i] == ':' {
			return key[:i], key[i+1:]
		}
	}
	return key, "v1"
}
