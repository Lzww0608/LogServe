package control

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/metadata"
	"github.com/logserve/logserve/internal/observability"
)

// SetBackpressure persists queue, redelivery, and log-latency limits before applying
// them to the live service configuration.
func (s *Service) SetBackpressure(ctx context.Context, req *logservepb.SetBackpressureRequest) (*logservepb.SetBackpressureResponse, error) {
	s.configMu.RLock()
	queueHighWatermark := s.queueHighWatermark
	redeliveryTimeout := s.redeliveryTimeout
	logAppendSlowLimit := s.logAppendSlowLimit
	s.configMu.RUnlock()

	// Zero request fields mean keep the current live value rather than resetting the
	// corresponding backpressure setting.
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

// GetDashboardSnapshot assembles a sorted, read-only view of queues, tasks,
// workflows, actors, workers, models, log compaction, and materializer state.
func (s *Service) GetDashboardSnapshot(ctx context.Context, req *logservepb.GetDashboardSnapshotRequest) (*logservepb.DashboardSnapshot, error) {
	var queueDepth uint32
	if s.useSchedulerV2() {
		queueDepth = uint32(s.scheduler.QueueDepth())
	} else {
		s.queueMu.Lock()
		queueDepth = uint32(len(s.queue))
		s.queueMu.Unlock()
	}

	tasks := s.meta.ListTasks()
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].CreatedAtMs == tasks[j].CreatedAtMs {
			return tasks[i].TaskID < tasks[j].TaskID
		}
		return tasks[i].CreatedAtMs < tasks[j].CreatedAtMs
	})
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
	sort.Slice(workflows, func(i, j int) bool {
		if workflows[i].CreatedAtMs == workflows[j].CreatedAtMs {
			return workflows[i].WorkflowID < workflows[j].WorkflowID
		}
		return workflows[i].CreatedAtMs < workflows[j].CreatedAtMs
	})
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
	compactableRecords, compactableBytes := s.compactableLogStats(ctx)
	return &logservepb.DashboardSnapshot{
		QueueDepth:            queueDepth,
		QueueHighWatermark:    queueHighWatermark,
		RedeliveryTimeoutMs:   redeliveryTimeout.Milliseconds(),
		SchedulingPolicy:      s.getSchedulingPolicy(),
		Tasks:                 dashboardTasks,
		Workflows:             dashboardWorkflows,
		Actors:                dashboardActors,
		Workers:               dashboardWorkers,
		Models:                models,
		LastLogAppendMs:       s.lastLogAppendMs.Load(),
		LogAppendSlowMs:       logAppendSlowLimit.Milliseconds(),
		CompactableLogRecords: compactableRecords,
		CompactableLogBytes:   compactableBytes,
		MetadataMaterializer:  metadataMaterializerSnapshot(s.meta),
	}, nil
}

// metadataMaterializerSnapshot adapts optional metadata materializer metrics into
// the dashboard response.
func metadataMaterializerSnapshot(store metadata.Store) *logservepb.MetadataMaterializerStats {
	reporter, ok := store.(interface {
		MaterializerStats() metadata.MaterializerStats
	})
	if !ok {
		return nil
	}
	stats := reporter.MaterializerStats()
	return &logservepb.MetadataMaterializerStats{
		Mode:                  stats.Mode,
		PendingDeltas:         uint64(stats.PendingDeltas),
		QueuedDeltas:          uint64(stats.QueuedDeltas),
		BatchMax:              uint32(stats.BatchMax),
		FlushIntervalMs:       stats.FlushInterval.Milliseconds(),
		FlushCount:            stats.FlushCount,
		FlushErrorCount:       stats.FlushErrorCount,
		LastFlushAtMs:         unixMilliOrZero(stats.LastFlushAt),
		LastSuccessAtMs:       unixMilliOrZero(stats.LastSuccessAt),
		LastErrorAtMs:         unixMilliOrZero(stats.LastErrorAt),
		LastFlushDurationMs:   stats.LastFlushDuration.Milliseconds(),
		LastFlushDeltas:       uint64(stats.LastFlushDeltas),
		LastError:             stats.LastError,
		EventualLagEstimateMs: stats.EventualLagEstimate.Milliseconds(),
	}
}

// unixMilliOrZero converts zero times to zero so dashboard JSON omits fake epoch data.
func unixMilliOrZero(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UnixMilli()
}

// compactableLogStats totals compactable records and bytes across all log streams.
func (s *Service) compactableLogStats(ctx context.Context) (uint64, uint64) {
	resp, err := s.log.GetStreamStats(ctx, &logservepb.GetStreamStatsRequest{})
	if err != nil {
		observability.Error("log_stats_failed", err, nil)
		return 0, 0
	}
	var records uint64
	var bytes uint64
	for _, stream := range resp.GetStreams() {
		records += stream.GetCompactableRecords()
		bytes += stream.GetCompactableBytes()
	}
	return records, bytes
}

// taskStatusLister is an optional metadata-store fast path for listing running tasks.
type taskStatusLister interface {
	ListTasksByStatus(status logservepb.TaskStatus) []metadata.Task
}

// redeliverExpiredTasks routes redelivery through the legacy queue or indexed
// scheduler implementation.
func (s *Service) redeliverExpiredTasks(ctx context.Context) error {
	if s.useSchedulerV2() {
		return s.redeliverExpiredTasksIndexed(ctx)
	}
	return s.redeliverExpiredTasksLegacy(ctx)
}

// redeliverExpiredTasksLegacy scans running tasks, appends redelivery events, and
// requeues leases that exceeded the configured timeout.
func (s *Service) redeliverExpiredTasksLegacy(ctx context.Context) error {
	_, redeliveryTimeout, _ := s.getBackpressureConfig()
	if redeliveryTimeout <= 0 {
		return nil
	}

	now := time.Now()
	runningTasks := s.meta.ListTasks()
	if lister, ok := s.meta.(taskStatusLister); ok {
		runningTasks = lister.ListTasksByStatus(logservepb.TaskStatus_TASK_STATUS_RUNNING)
	}
	expired := make([]metadata.Task, 0)
	for _, task := range runningTasks {
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
		if err := s.appendTaskRedelivered(ctx, task); err != nil {
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
	if len(requeued) > 0 {
		s.notifyTaskAvailable()
	}
	return nil
}

// redeliverExpiredTasksIndexed uses scheduler deadline tracking to redeliver only
// the leases that should have expired.
func (s *Service) redeliverExpiredTasksIndexed(ctx context.Context) error {
	_, redeliveryTimeout, _ := s.getBackpressureConfig()
	if redeliveryTimeout <= 0 || s.scheduler == nil {
		return nil
	}
	expired := s.scheduler.PopExpiredRunning(time.Now().UnixMilli())
	if len(expired) == 0 {
		return nil
	}
	valid := make([]runningLease, 0, len(expired))
	for _, lease := range expired {
		task, ok := s.meta.GetTask(lease.taskID)
		if !ok {
			s.scheduler.Forget(lease.taskID)
			continue
		}
		if task.Status != logservepb.TaskStatus_TASK_STATUS_RUNNING || task.TaskLeaseEpoch != lease.leaseEpoch {
			s.resyncSchedulerTask(task, redeliveryTimeout)
			continue
		}
		if task.UpdatedAtMs == 0 || time.Since(time.UnixMilli(task.UpdatedAtMs)) < redeliveryTimeout {
			s.scheduler.TrackRunning(task.TaskID, schedulerLeaseDeadlineMs(task, redeliveryTimeout), task.TaskLeaseEpoch)
			continue
		}
		valid = append(valid, lease)
	}
	if len(valid) == 0 {
		return nil
	}
	for _, lease := range valid {
		task, ok := s.meta.GetTask(lease.taskID)
		if !ok {
			s.scheduler.Forget(lease.taskID)
			continue
		}
		if err := s.appendTaskRedelivered(ctx, task); err != nil {
			for _, pending := range valid {
				if current, ok := s.meta.GetTask(pending.taskID); ok && current.Status == logservepb.TaskStatus_TASK_STATUS_RUNNING {
					s.scheduler.TrackRunning(current.TaskID, schedulerLeaseDeadlineMs(current, redeliveryTimeout), current.TaskLeaseEpoch)
				}
			}
			return err
		}
	}
	requeuedCount := 0
	for _, lease := range valid {
		task, requeued := s.meta.RequeueTaskIfLeaseExpired(lease.taskID, lease.leaseEpoch, redeliveryTimeout)
		if !requeued {
			if current, ok := s.meta.GetTask(lease.taskID); ok {
				s.resyncSchedulerTask(current, redeliveryTimeout)
			} else {
				s.scheduler.Forget(lease.taskID)
			}
			continue
		}
		requeuedCount++
		s.scheduler.Enqueue(s.schedulerMetaFromTask(task))
		if task.WorkerID != "" {
			s.meta.DecrementWorkerLoad(task.WorkerID)
			s.updateSchedulerWorker(task.WorkerID)
		}
	}
	if requeuedCount > 0 {
		s.notifyTaskAvailable()
	}
	return nil
}

// appendTaskRedelivered writes the durable redelivery marker for a task lease.
func (s *Service) appendTaskRedelivered(ctx context.Context, task metadata.Task) error {
	payload, _ := json.Marshal(map[string]any{
		"task_id":          task.TaskID,
		"task_name":        task.TaskName,
		"task_lease_epoch": task.TaskLeaseEpoch,
		"timestamp_ms":     time.Now().UnixMilli(),
	})
	_, err := s.appendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       taskStream(task.TaskID),
		EventType:      "TaskRedelivered",
		IdempotencyKey: task.TaskID + ":redelivered:" + time.Now().Format("150405.000000000"),
		Payload:        payload,
	})
	return err
}

// resyncSchedulerTask repairs scheduler state when a deadline heap entry no longer
// matches current metadata.
func (s *Service) resyncSchedulerTask(task metadata.Task, redeliveryTimeout time.Duration) {
	if s.scheduler == nil {
		return
	}
	switch task.Status {
	case logservepb.TaskStatus_TASK_STATUS_QUEUED:
		s.scheduler.Enqueue(s.schedulerMetaFromTask(task))
	case logservepb.TaskStatus_TASK_STATUS_RUNNING:
		s.scheduler.TrackRunning(task.TaskID, schedulerLeaseDeadlineMs(task, redeliveryTimeout), task.TaskLeaseEpoch)
	default:
		s.scheduler.Forget(task.TaskID)
	}
}

// cacheEntries returns sorted cached-model entries for dashboard output.
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

// splitModelKey decodes metadata model keys and supplies v1 when no version is present.
func splitModelKey(key string) (string, string) {
	for i := 0; i < len(key); i++ {
		if key[i] == ':' {
			return key[:i], key[i+1:]
		}
	}
	return key, "v1"
}
