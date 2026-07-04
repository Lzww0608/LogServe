package control

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/actorlock"
	"github.com/logserve/logserve/internal/eventcodec"
	"github.com/logserve/logserve/internal/logrecord"
	"github.com/logserve/logserve/internal/metadata"
	"github.com/logserve/logserve/internal/objectstore"
	"github.com/logserve/logserve/internal/observability"
	"github.com/logserve/logserve/internal/workflow"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// This file implements the main control-plane service: task submission, worker
// polling, workflow progression, result materialization, and queue signaling.
const defaultResultInlineThreshold = 4096
const largeActorStateLogThreshold = 4096
const actorOwnerLease = 750 * time.Millisecond
const schedulerWorkerLease = 5 * time.Second
const localityQueueWait = 250 * time.Millisecond
const defaultQueueHighWatermark = 1024
const defaultRedeliveryTimeout = 30 * time.Second
const defaultPollBatchLimit = 64
const maxPollWaitTimeout = 5 * time.Second

// taskSubmittedPayload is the legacy JSON wrapper for TaskSubmitted events.
// Newer records use eventcodec, but bootstrap still accepts this shape.
type taskSubmittedPayload struct {
	TaskSpec json.RawMessage `json:"task_spec,omitempty"`
}

// taskLifecyclePayload is written to task lifecycle events and carries the lease
// epoch so replay can fence stale starts, completions, and redeliveries.
type taskLifecyclePayload struct {
	TaskLeaseEpoch uint64          `json:"task_lease_epoch,omitempty"`
	WorkerID       string          `json:"worker_id,omitempty"`
	ResultJSON     json.RawMessage `json:"result_json,omitempty"`
	Error          string          `json:"error,omitempty"`
	TimestampMs    int64           `json:"timestamp_ms,omitempty"`
}

// Service owns the in-memory control-plane materialization backed by the log.
// Its mutating RPCs append durable events before updating metadata so restart
// recovery can rebuild state from log records when metadata is lost.
type Service struct {
	logservepb.UnimplementedControlServiceServer
	meta                  metadata.Store
	log                   logClient
	queueMu               sync.Mutex
	queue                 []string
	scheduler             *Scheduler
	schedulerV2           bool
	specMu                sync.RWMutex
	specs                 map[string]*logservepb.TaskSpec
	llmStatsMu            sync.RWMutex
	llmStats              map[llmStatsKey]llmWorkerStats
	workflowMu            sync.Mutex
	actorLocks            *actorlock.Table
	resultStore           objectstore.Store
	resultInlineThreshold int
	functionStore         objectstore.Store
	functionsMu           sync.RWMutex
	functions             map[string]functionRegisteredPayload
	configMu              sync.RWMutex
	schedulingPolicy      logservepb.SchedulingPolicy
	queueHighWatermark    uint32
	redeliveryTimeout     time.Duration
	logAppendSlowLimit    time.Duration
	lastLogAppendMs       atomic.Int64
	taskNotifyMu          sync.Mutex
	taskNotifyCh          chan struct{}
}

// NewService constructs a Service with the default local object store for large
// results and registered function sources.
func NewService(meta metadata.Store, logClient logClient) *Service {
	store, _ := objectstore.OpenLocal(filepath.Join(os.TempDir(), "logserve-objectstore"))
	return NewServiceWithResultStore(meta, logClient, store, defaultResultInlineThreshold)
}

// NewServiceWithResultStore constructs a Service with explicit result storage.
// Non-positive thresholds fall back to the default inline result size.
func NewServiceWithResultStore(meta metadata.Store, logClient logClient, store objectstore.Store, threshold int) *Service {
	if threshold <= 0 {
		threshold = defaultResultInlineThreshold
	}
	functionStore := store
	if functionStore == nil {
		functionStore, _ = objectstore.OpenLocal(filepath.Join(os.TempDir(), "logserve-objectstore"))
	}
	return &Service{
		meta:                  meta,
		log:                   logClient,
		queue:                 make([]string, 0, 1024),
		scheduler:             newScheduler(),
		schedulerV2:           os.Getenv("LOGSERVE_SCHEDULER_V2") == "1",
		specs:                 make(map[string]*logservepb.TaskSpec),
		llmStats:              make(map[llmStatsKey]llmWorkerStats),
		actorLocks:            actorlock.NewTable(),
		functions:             make(map[string]functionRegisteredPayload),
		resultStore:           store,
		functionStore:         functionStore,
		resultInlineThreshold: threshold,
		schedulingPolicy:      logservepb.SchedulingPolicy_SCHEDULING_POLICY_LOCALITY_AWARE,
		queueHighWatermark:    defaultQueueHighWatermark,
		redeliveryTimeout:     defaultRedeliveryTimeout,
		taskNotifyCh:          make(chan struct{}),
	}
}

// appendLog appends a control-plane event and records the observed latency for
// dashboard reporting and backpressure decisions.
func (s *Service) appendLog(ctx context.Context, req *logservepb.AppendLogRequest) (*logservepb.AppendLogResponse, error) {
	start := time.Now()
	resp, err := s.log.AppendLog(ctx, req)
	elapsedMs := time.Since(start).Milliseconds()
	if elapsedMs == 0 {
		elapsedMs = 1
	}
	s.lastLogAppendMs.Store(elapsedMs)
	return resp, err
}

// metadataPersisted surfaces synchronous metadata persistence failures while
// treating configured non-blocking persistence errors as observable warnings.
func (s *Service) metadataPersisted() error {
	reporter, ok := s.meta.(interface{ LastError() error })
	if !ok {
		return nil
	}
	if err := reporter.LastError(); err != nil {
		if nonBlocking, ok := s.meta.(interface{ NonBlockingPersistence() bool }); ok && nonBlocking.NonBlockingPersistence() {
			observability.Error("metadata_persistence_async_error", err, nil)
			return nil
		}
		return fmt.Errorf("metadata persistence failed: %w", err)
	}
	return nil
}

// pollBatchLimit normalizes worker batch requests and caps them to the server
// default so one poll cannot drain an unbounded number of tasks.
func pollBatchLimit(requested uint32) int {
	if requested == 0 {
		return 1
	}
	if requested > defaultPollBatchLimit {
		return defaultPollBatchLimit
	}
	return int(requested)
}

// pollWaitTimeout converts long-poll milliseconds into a bounded duration.
func pollWaitTimeout(waitMs int64) time.Duration {
	if waitMs <= 0 {
		return 0
	}
	wait := time.Duration(waitMs) * time.Millisecond
	if wait > maxPollWaitTimeout {
		return maxPollWaitTimeout
	}
	return wait
}

// pollTaskResponse keeps the legacy single Task field and the newer Tasks slice
// consistent for batch-aware and older workers.
func pollTaskResponse(tasks []*logservepb.TaskSpec) *logservepb.PollTaskResponse {
	resp := &logservepb.PollTaskResponse{}
	if len(tasks) == 0 {
		return resp
	}
	resp.HasTask = true
	resp.Task = tasks[0]
	resp.Tasks = tasks
	return resp
}

// taskAvailableSignal returns the current long-poll wakeup channel under lock.
func (s *Service) taskAvailableSignal() <-chan struct{} {
	s.taskNotifyMu.Lock()
	defer s.taskNotifyMu.Unlock()
	if s.taskNotifyCh == nil {
		s.taskNotifyCh = make(chan struct{})
	}
	return s.taskNotifyCh
}

// notifyTaskAvailable wakes current long-pollers and installs a fresh channel for
// future polls.
func (s *Service) notifyTaskAvailable() {
	if s == nil {
		return
	}
	s.taskNotifyMu.Lock()
	defer s.taskNotifyMu.Unlock()
	if s.taskNotifyCh == nil {
		s.taskNotifyCh = make(chan struct{})
		return
	}
	// Closing broadcasts to all current waiters; replacing the channel prevents
	// later pollers from observing a permanently closed signal.
	close(s.taskNotifyCh)
	s.taskNotifyCh = make(chan struct{})
}

// getBackpressureConfig snapshots queue and redelivery limits under the config lock.
func (s *Service) getBackpressureConfig() (uint32, time.Duration, time.Duration) {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.queueHighWatermark, s.redeliveryTimeout, s.logAppendSlowLimit
}

// getSchedulingPolicy reads the current LLM scheduling policy safely.
func (s *Service) getSchedulingPolicy() logservepb.SchedulingPolicy {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.schedulingPolicy
}

// useSchedulerV2 reports whether the indexed scheduler is enabled and available.
func (s *Service) useSchedulerV2() bool {
	return s != nil && s.schedulerV2 && s.scheduler != nil
}

// syncSchedulerLLMStats copies LLM statistics into the indexed scheduler without
// holding the stats lock while the scheduler updates its placement indexes.
func (s *Service) syncSchedulerLLMStats() {
	if s == nil || s.scheduler == nil {
		return
	}
	s.llmStatsMu.RLock()
	snapshot := make(map[llmStatsKey]llmWorkerStats, len(s.llmStats))
	for key, stats := range s.llmStats {
		snapshot[key] = stats
	}
	s.llmStatsMu.RUnlock()
	s.scheduler.SyncLLMStats(snapshot)
}

// syncActiveSchedulerWorkers hydrates scheduler placement data from currently
// live workers only.
func (s *Service) syncActiveSchedulerWorkers() {
	if s.scheduler == nil {
		return
	}
	for _, worker := range s.meta.ActiveWorkers(schedulerWorkerLease) {
		s.scheduler.UpsertWorker(worker)
	}
}

// syncSchedulerWorkers rebuilds scheduler worker views from all metadata workers
// and then refreshes LLM placement statistics.
func (s *Service) syncSchedulerWorkers() {
	if s.scheduler == nil {
		return
	}
	for _, worker := range s.meta.ListWorkers() {
		s.scheduler.UpsertWorker(worker)
	}
	s.syncSchedulerLLMStats()
}

// hydrateSchedulerWorkersIfNeeded lazily populates the scheduler after bootstrap or
// tests that construct scheduler state before worker heartbeats.
func (s *Service) hydrateSchedulerWorkersIfNeeded() {
	if s.scheduler == nil || s.scheduler.WorkerCount() > 0 {
		return
	}
	s.syncActiveSchedulerWorkers()
}

// updateSchedulerWorker mirrors a single metadata worker into scheduler indexes.
func (s *Service) updateSchedulerWorker(workerID string) {
	if s.scheduler == nil || workerID == "" {
		return
	}
	if worker, ok := s.meta.GetWorker(workerID); ok {
		s.scheduler.UpsertWorker(worker)
	}
}

// completeSchedulerRunning removes a task from indexed running-lease tracking.
func (s *Service) completeSchedulerRunning(taskID string) {
	if s != nil && s.scheduler != nil {
		s.scheduler.CompleteRunning(taskID)
	}
}

// schedulerSnapshot captures the worker-specific view used for one assignment pass,
// including actor ownership and the next command sequence each actor can accept.
func (s *Service) schedulerSnapshot(workerID string) workerSnapshot {
	snapshot := workerSnapshot{
		WorkerID:         workerID,
		ActorNextSeq:     make(map[string]uint64),
		CachedModels:     make(map[modelKey]struct{}),
		SchedulingPolicy: s.getSchedulingPolicy(),
	}
	if worker, ok := s.meta.GetWorker(workerID); ok {
		snapshot.CachedModels = modelKeySet(worker.CachedModels)
	}
	for _, actorState := range s.meta.ListActors() {
		if actorState.OwnerWorkerID != workerID {
			continue
		}
		snapshot.ActorIDs = append(snapshot.ActorIDs, actorState.ActorID)
		snapshot.ActorNextSeq[actorState.ActorID] = actorState.CommandCount + 1
	}
	sort.Strings(snapshot.ActorIDs)
	return snapshot
}

// schedulerMetaFromTask projects metadata fields into the scheduler key space.
func (s *Service) schedulerMetaFromTask(task metadata.Task) SchedMeta {
	return SchedMeta{
		TaskID:        task.TaskID,
		TargetWorker:  task.TargetWorkerID,
		ActorID:       task.ActorID,
		CommandSeq:    task.ActorCommandSeq,
		ModelName:     task.LLMModelName,
		ModelVersion:  task.LLMModelVersion,
		CreatedAtMs:   task.CreatedAtMs,
		LeaseEpoch:    task.TaskLeaseEpoch,
		RunningWorker: task.WorkerID,
	}
}

// schedulerLeaseDeadlineMs computes the indexed redelivery deadline for a running
// task lease.
func schedulerLeaseDeadlineMs(task metadata.Task, timeout time.Duration) int64 {
	if timeout <= 0 || task.UpdatedAtMs == 0 {
		return 0
	}
	return task.UpdatedAtMs + timeout.Milliseconds()
}

// SubmitTask validates a user task, registers its Python function source or hash,
// and enqueues it through the log-first task path.
func (s *Service) SubmitTask(ctx context.Context, req *logservepb.SubmitTaskRequest) (*logservepb.SubmitTaskResponse, error) {
	if req.GetTaskName() == "" {
		return nil, errors.New("task_name is required")
	}
	if req.GetFunctionName() == "" {
		return nil, errors.New("function_name is required")
	}
	if req.GetFunctionSource() == "" && req.GetFunctionRef() == "" && req.GetFunctionHash() == "" {
		return nil, errors.New("function_source or function_hash is required")
	}

	task, duplicate, err := s.enqueueTask(ctx, &logservepb.TaskSpec{
		TaskId:         newTaskID(),
		TaskName:       req.GetTaskName(),
		FunctionName:   req.GetFunctionName(),
		FunctionSource: req.GetFunctionSource(),
		FunctionRef:    req.GetFunctionRef(),
		FunctionHash:   req.GetFunctionHash(),
		ArgsJson:       append([]byte(nil), req.GetArgsJson()...),
		IdempotencyKey: req.GetIdempotencyKey(),
	})
	if err != nil {
		return nil, err
	}
	if duplicate {
		return &logservepb.SubmitTaskResponse{TaskId: task.TaskID, Status: task.Status}, nil
	}
	return &logservepb.SubmitTaskResponse{TaskId: task.TaskID, Status: task.Status}, nil
}

// GetTaskStatus returns the current materialized task status from metadata.
func (s *Service) GetTaskStatus(ctx context.Context, req *logservepb.GetTaskStatusRequest) (*logservepb.GetTaskStatusResponse, error) {
	task, ok := s.meta.GetTask(req.GetTaskId())
	if !ok {
		return nil, errors.New("task not found")
	}
	return taskStatusResponse(task), nil
}

// SubmitWorkflow parses and normalizes a workflow definition, records the start
// event, materializes metadata, and schedules initially ready steps.
func (s *Service) SubmitWorkflow(ctx context.Context, req *logservepb.SubmitWorkflowRequest) (*logservepb.SubmitWorkflowResponse, error) {
	if req.GetWorkflowName() == "" {
		return nil, errors.New("workflow_name is required")
	}
	if len(req.GetDefinitionJson()) == 0 {
		return nil, errors.New("definition_json is required")
	}
	def, err := workflow.ParseDefinition(req.GetDefinitionJson())
	if err != nil {
		return nil, err
	}
	if def.WorkflowName == "" {
		def.WorkflowName = req.GetWorkflowName()
	}
	if len(def.Steps) == 0 {
		return nil, errors.New("workflow must contain at least one step")
	}
	if def.ResultStepID == "" {
		def.ResultStepID = def.Steps[len(def.Steps)-1].StepID
	}
	if err := s.normalizeWorkflowDefinition(ctx, &def); err != nil {
		return nil, err
	}
	fingerprint, err := workflowFingerprint(req.GetWorkflowName(), def)
	if err != nil {
		return nil, err
	}

	if existing, ok := s.meta.GetWorkflowByIdempotencyKey(req.GetIdempotencyKey()); ok {
		if err := ensureIdempotencyFingerprint("workflow", req.GetIdempotencyKey(), existing.IdempotencyFingerprint, fingerprint); err != nil {
			return nil, err
		}
		return &logservepb.SubmitWorkflowResponse{WorkflowId: existing.WorkflowID, Status: existing.Status}, nil
	}

	workflowID := newWorkflowID()
	now := workflow.NowMs()
	state := workflow.NewState(workflowID, def, now)
	state.IdempotencyKey = req.GetIdempotencyKey()
	state.IdempotencyFingerprint = fingerprint
	definitionJSON, err := json.Marshal(def)
	if err != nil {
		return nil, err
	}
	payload, _ := workflow.MarshalEventPayload(workflow.EventPayload{
		WorkflowID:             workflowID,
		WorkflowName:           def.WorkflowName,
		DefinitionJSON:         definitionJSON,
		IdempotencyKey:         req.GetIdempotencyKey(),
		IdempotencyFingerprint: fingerprint,
		TimestampMs:            now,
	})
	if _, err := s.appendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       workflowStream(workflowID),
		EventType:      "WorkflowStarted",
		IdempotencyKey: workflowID + ":started",
		Payload:        payload,
	}); err != nil {
		return nil, err
	}
	created, duplicate := s.meta.CreateWorkflow(state, req.GetIdempotencyKey())
	if !duplicate {
		if err := s.metadataPersisted(); err != nil {
			return nil, err
		}
	}
	if duplicate {
		if err := ensureIdempotencyFingerprint("workflow", req.GetIdempotencyKey(), created.IdempotencyFingerprint, fingerprint); err != nil {
			return nil, err
		}
		return &logservepb.SubmitWorkflowResponse{WorkflowId: created.WorkflowID, Status: created.Status}, nil
	}
	if err := s.scheduleReadySteps(ctx, workflowID); err != nil {
		return nil, err
	}
	return &logservepb.SubmitWorkflowResponse{WorkflowId: workflowID, Status: logservepb.WorkflowStatus_WORKFLOW_STATUS_RUNNING}, nil
}

// GetWorkflowStatus returns the current materialized workflow state.
func (s *Service) GetWorkflowStatus(ctx context.Context, req *logservepb.GetWorkflowStatusRequest) (*logservepb.GetWorkflowStatusResponse, error) {
	state, ok := s.meta.GetWorkflow(req.GetWorkflowId())
	if !ok {
		return nil, errors.New("workflow not found")
	}
	return workflowStatusResponse(state), nil
}

// ReplayWorkflow rebuilds one workflow from its log stream and compares it with
// the current metadata materialization.
func (s *Service) ReplayWorkflow(ctx context.Context, req *logservepb.ReplayWorkflowRequest) (*logservepb.ReplayWorkflowResponse, error) {
	streamID := workflowStream(req.GetWorkflowId())
	replayed, err := workflow.ReplayRawEach(req.GetWorkflowId(), func(emit func(logrecord.RawRecord) error) error {
		return s.forEachRawLogRecord(ctx, streamID, 1, emit)
	})
	if err != nil {
		return nil, err
	}
	current, ok := s.meta.GetWorkflow(req.GetWorkflowId())
	consistent := ok && workflow.Consistent(replayed, current)
	return &logservepb.ReplayWorkflowResponse{
		Replayed:               workflowStatusResponse(replayed),
		ConsistentWithMetadata: consistent,
	}, nil
}

// RegisterWorker records a worker registration event, upserts worker metadata, and
// wakes pollers that may now have capacity to execute tasks.
func (s *Service) RegisterWorker(ctx context.Context, req *logservepb.RegisterWorkerRequest) (*logservepb.RegisterWorkerResponse, error) {
	if req.GetWorkerId() == "" {
		return nil, errors.New("worker_id is required")
	}
	payload, _ := json.Marshal(map[string]any{
		"worker_id":     req.GetWorkerId(),
		"address":       req.GetAddress(),
		"labels":        req.GetLabels(),
		"cached_models": req.GetCachedModels(),
		"capacity":      req.GetCapacity(),
	})
	if _, err := s.appendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       "system:workers",
		EventType:      "WorkerRegistered",
		IdempotencyKey: fmt.Sprintf("%s:registered:%d", req.GetWorkerId(), time.Now().UnixNano()),
		Payload:        payload,
	}); err != nil {
		return nil, err
	}
	s.meta.UpsertWorker(metadata.Worker{
		WorkerID:     req.GetWorkerId(),
		Address:      req.GetAddress(),
		Labels:       req.GetLabels(),
		CachedModels: modelCacheFromProto(req.GetCachedModels()),
		Capacity:     req.GetCapacity(),
	})
	s.updateSchedulerWorker(req.GetWorkerId())
	if err := s.metadataPersisted(); err != nil {
		return nil, err
	}
	s.notifyTaskAvailable()
	return &logservepb.RegisterWorkerResponse{Accepted: true}, nil
}

// Heartbeat refreshes worker liveness, model cache metadata, and scheduler indexes.
func (s *Service) Heartbeat(ctx context.Context, req *logservepb.HeartbeatRequest) (*logservepb.HeartbeatResponse, error) {
	if req.GetWorkerId() == "" {
		return nil, errors.New("worker_id is required")
	}
	s.meta.Heartbeat(req.GetWorkerId(), modelCacheFromProto(req.GetCachedModels()))
	s.updateSchedulerWorker(req.GetWorkerId())
	if err := s.metadataPersisted(); err != nil {
		return nil, err
	}
	s.notifyTaskAvailable()
	return &logservepb.HeartbeatResponse{ServerTimeMs: time.Now().UnixMilli()}, nil
}

// PollTask leases immediately available work or waits up to the requested bounded
// long-poll timeout for a task-available signal.
func (s *Service) PollTask(ctx context.Context, req *logservepb.PollTaskRequest) (*logservepb.PollTaskResponse, error) {
	if req.GetWorkerId() == "" {
		return nil, errors.New("worker_id is required")
	}
	maxTasks := pollBatchLimit(req.GetMaxTasks())
	wait := pollWaitTimeout(req.GetWaitTimeoutMs())
	// Capture the current signal before polling so a concurrent enqueue either wakes
	// this wait or is observed by the immediate poll below.
	signal := s.taskAvailableSignal()
	resp, err := s.pollTaskNow(ctx, req.GetWorkerId(), maxTasks)
	if err != nil || resp.GetHasTask() || wait <= 0 {
		return resp, err
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-signal:
		return s.pollTaskNow(ctx, req.GetWorkerId(), maxTasks)
	case <-timer.C:
		return &logservepb.PollTaskResponse{}, nil
	}
}

// pollTaskNow performs one non-blocking poll pass after first redelivering expired
// leases so workers can pick up abandoned tasks.
func (s *Service) pollTaskNow(ctx context.Context, workerID string, maxTasks int) (*logservepb.PollTaskResponse, error) {
	if err := s.redeliverExpiredTasks(ctx); err != nil {
		return nil, err
	}
	if s.useSchedulerV2() {
		tasks, err := s.pollTaskIndexed(workerID, maxTasks)
		if err != nil {
			return nil, err
		}
		return pollTaskResponse(tasks), nil
	}
	tasks, err := s.pollTaskLegacy(workerID, maxTasks)
	if err != nil {
		return nil, err
	}
	return pollTaskResponse(tasks), nil
}

// pollTaskLegacy scans the FIFO queue and leases tasks that match the polling
// worker, leaving blocked actor or LLM tasks in place.
func (s *Service) pollTaskLegacy(workerID string, maxTasks int) ([]*logservepb.TaskSpec, error) {
	tasks := make([]*logservepb.TaskSpec, 0, maxTasks)
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	for i := 0; i < len(s.queue) && len(tasks) < maxTasks; {
		taskID := s.queue[i]
		s.specMu.RLock()
		spec, ok := s.specs[taskID]
		s.specMu.RUnlock()
		if !ok {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			continue
		}
		before, ok := s.meta.GetTask(taskID)
		if !ok {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			continue
		}
		if !s.actorMailboxReady(before, workerID) {
			i++
			continue
		}
		if !s.canAssignTaskToWorker(taskID, spec, workerID) {
			i++
			continue
		}
		leased, err := s.meta.LeaseTask(taskID, workerID)
		if err != nil {
			return nil, err
		}
		if before.Status == logservepb.TaskStatus_TASK_STATUS_QUEUED {
			s.meta.IncrementWorkerLoad(workerID)
		}
		s.queue = append(s.queue[:i], s.queue[i+1:]...)
		tasks = append(tasks, s.leasedTaskSpec(spec, leased))
	}
	return tasks, nil
}

// pollTaskIndexed asks Scheduler for assignable tasks and keeps scheduler state in
// sync with metadata lease decisions.
func (s *Service) pollTaskIndexed(workerID string, maxTasks int) ([]*logservepb.TaskSpec, error) {
	s.hydrateSchedulerWorkersIfNeeded()
	tasks := make([]*logservepb.TaskSpec, 0, maxTasks)
	_, redeliveryTimeout, _ := s.getBackpressureConfig()
	for len(tasks) < maxTasks {
		snapshot := s.schedulerSnapshot(workerID)
		check := func(meta SchedMeta) schedulerDecision {
			spec := s.specForTask(meta.TaskID)
			if spec == nil {
				return schedulerDrop
			}
			task, ok := s.meta.GetTask(meta.TaskID)
			if !ok || isTerminalTaskStatus(task.Status) {
				return schedulerDrop
			}
			if task.Status != logservepb.TaskStatus_TASK_STATUS_QUEUED {
				return schedulerDrop
			}
			if !s.actorMailboxReady(task, workerID) {
				return schedulerSkip
			}
			if !s.canAssignTaskToWorker(meta.TaskID, spec, workerID) {
				return schedulerSkip
			}
			return schedulerAssign
		}
		meta, ok := s.scheduler.Assign(snapshot, time.Now().UnixMilli(), check)
		if !ok {
			break
		}
		spec := s.specForTask(meta.TaskID)
		if spec == nil {
			s.scheduler.Forget(meta.TaskID)
			continue
		}
		before, ok := s.meta.GetTask(meta.TaskID)
		if !ok || before.Status != logservepb.TaskStatus_TASK_STATUS_QUEUED {
			s.scheduler.Forget(meta.TaskID)
			continue
		}
		leased, err := s.meta.LeaseTask(meta.TaskID, workerID)
		if err != nil {
			return nil, err
		}
		if before.Status == logservepb.TaskStatus_TASK_STATUS_QUEUED {
			s.meta.IncrementWorkerLoad(workerID)
			s.updateSchedulerWorker(workerID)
		}
		s.scheduler.TrackRunning(leased.TaskID, schedulerLeaseDeadlineMs(leased, redeliveryTimeout), leased.TaskLeaseEpoch)
		tasks = append(tasks, s.leasedTaskSpec(spec, leased))
	}
	return tasks, nil
}

// actorMailboxReady enforces per-actor ownership and command sequence ordering
// before a task can be leased to a worker.
func (s *Service) actorMailboxReady(task metadata.Task, workerID string) bool {
	if task.ActorID == "" {
		return true
	}
	state, ok := s.meta.GetActor(task.ActorID)
	if !ok || state.OwnerWorkerID == "" || state.OwnerWorkerID != workerID {
		return false
	}
	commandSeq := task.ActorCommandSeq
	if commandSeq == 0 {
		commandSeq = state.CommandCount + 1
	}
	return commandSeq == state.CommandCount+1
}

// leasedTaskSpec returns the worker-facing task spec with lease epoch and fresh
// actor ownership/state injected at dispatch time.
func (s *Service) leasedTaskSpec(spec *logservepb.TaskSpec, leased metadata.Task) *logservepb.TaskSpec {
	leasedSpec := cloneSpec(spec)
	leasedSpec.TaskLeaseEpoch = leased.TaskLeaseEpoch
	if leased.ActorID == "" {
		return leasedSpec
	}
	state, ok := s.meta.GetActor(leased.ActorID)
	if !ok {
		return leasedSpec
	}
	leasedSpec.TargetWorkerId = state.OwnerWorkerID
	leasedSpec.ActorEpoch = state.Epoch
	leasedSpec.ActorStateJson = append([]byte(nil), state.StateJSON...)
	if stateBytes := len(state.StateJSON); stateBytes >= largeActorStateLogThreshold {
		observability.Info("actor_command_dispatched", map[string]any{
			"actor_id":         leased.ActorID,
			"task_id":          leased.TaskID,
			"command_seq":      leased.ActorCommandSeq,
			"state_json_bytes": stateBytes,
		})
	}
	return leasedSpec
}

// StartTask records that a validated lease has begun executing and materializes
// workflow step start state when applicable.
func (s *Service) StartTask(ctx context.Context, req *logservepb.StartTaskRequest) (*logservepb.StartTaskResponse, error) {
	task, err := s.meta.ValidateTaskLease(req.GetTaskId(), req.GetWorkerId(), req.GetTaskLeaseEpoch())
	if err != nil {
		return nil, err
	}
	if isTerminalTaskStatus(task.Status) {
		return &logservepb.StartTaskResponse{Accepted: true}, nil
	}
	if err := s.appendTaskStarted(ctx, task, req.GetWorkerId()); err != nil {
		return nil, err
	}
	if task.ActorID != "" {
		return &logservepb.StartTaskResponse{Accepted: true}, nil
	}
	if task.WorkflowID != "" {
		if err := s.markWorkflowStepStarted(ctx, task, req.GetWorkerId()); err != nil {
			return nil, err
		}
	}
	return &logservepb.StartTaskResponse{Accepted: true}, nil
}

// CompleteTask validates the lease epoch, records terminal task state, updates
// actor/workflow/LLM side materializations, and releases worker load.
func (s *Service) CompleteTask(ctx context.Context, req *logservepb.CompleteTaskRequest) (*logservepb.CompleteTaskResponse, error) {
	if req.GetStatus() != logservepb.TaskStatus_TASK_STATUS_SUCCEEDED && req.GetStatus() != logservepb.TaskStatus_TASK_STATUS_FAILED {
		return nil, errors.New("complete status must be SUCCEEDED or FAILED")
	}
	existing, ok := s.meta.GetTask(req.GetTaskId())
	if ok && isTerminalTaskStatus(existing.Status) {
		s.completeSchedulerRunning(req.GetTaskId())
		return &logservepb.CompleteTaskResponse{Accepted: true}, nil
	}
	validated, err := s.meta.ValidateTaskLease(req.GetTaskId(), req.GetWorkerId(), req.GetTaskLeaseEpoch())
	if err != nil {
		return nil, err
	}
	if validated.ActorID != "" {
		// Apply actor state before appending task completion so replay observes the
		// actor command and task terminal event in execution order.
		if err := s.completeActorCall(ctx, validated, req); err != nil {
			return nil, err
		}
		if err := s.appendTaskTerminal(ctx, validated, req.GetStatus(), req.GetResultJson(), req.GetError()); err != nil {
			return nil, err
		}
		if _, err := s.meta.CompleteTask(req.GetTaskId(), req.GetWorkerId(), req.GetTaskLeaseEpoch(), req.GetStatus(), req.GetResultJson(), req.GetError()); err != nil {
			return nil, err
		}
		if ok && existing.Status == logservepb.TaskStatus_TASK_STATUS_RUNNING {
			s.meta.DecrementWorkerLoad(req.GetWorkerId())
		}
		s.completeSchedulerRunning(req.GetTaskId())
		s.updateSchedulerWorker(req.GetWorkerId())
		s.notifyTaskAvailable()
		return &logservepb.CompleteTaskResponse{Accepted: true}, nil
	}
	if err := s.appendTaskTerminal(ctx, validated, req.GetStatus(), req.GetResultJson(), req.GetError()); err != nil {
		return nil, err
	}
	task, err := s.meta.CompleteTask(req.GetTaskId(), req.GetWorkerId(), req.GetTaskLeaseEpoch(), req.GetStatus(), req.GetResultJson(), req.GetError())
	if err != nil {
		return nil, err
	}
	if ok && existing.Status == logservepb.TaskStatus_TASK_STATUS_RUNNING {
		s.meta.DecrementWorkerLoad(req.GetWorkerId())
	}
	s.completeSchedulerRunning(req.GetTaskId())
	s.updateSchedulerWorker(req.GetWorkerId())
	wasTerminal := ok && isTerminalTaskStatus(existing.Status)
	if task.LLMModelName != "" && req.GetStatus() == logservepb.TaskStatus_TASK_STATUS_SUCCEEDED && !wasTerminal {
		if err := s.materializeLLMTaskCompletion(ctx, task.TaskID); err != nil {
			observability.Error("llm_stats_materialize_failed", err, map[string]any{"task_id": task.TaskID, "worker_id": req.GetWorkerId()})
		}
	}
	if task.WorkflowID != "" {
		shouldSchedule, err := s.completeWorkflowStep(ctx, task, req.GetStatus(), req.GetResultJson(), req.GetError())
		if err != nil {
			return nil, err
		}
		if shouldSchedule {
			if err := s.scheduleReadySteps(ctx, task.WorkflowID); err != nil {
				return nil, err
			}
		}
	}
	s.notifyTaskAvailable()
	return &logservepb.CompleteTaskResponse{Accepted: true}, nil
}

// CompleteTasks applies CompleteTask independently for each batch item and reports
// per-task errors without aborting the whole batch after a single bad item.
func (s *Service) CompleteTasks(ctx context.Context, req *logservepb.CompleteTaskBatchRequest) (*logservepb.CompleteTaskBatchResponse, error) {
	resp := &logservepb.CompleteTaskBatchResponse{Accepted: true}
	if req == nil {
		return resp, nil
	}
	resp.Results = make([]*logservepb.CompleteTaskBatchResult, 0, len(req.GetTasks()))
	for _, taskReq := range req.GetTasks() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result := &logservepb.CompleteTaskBatchResult{}
		if taskReq == nil {
			result.Error = "completion request is nil"
			resp.Accepted = false
			resp.Results = append(resp.Results, result)
			continue
		}
		result.TaskId = taskReq.GetTaskId()
		if _, err := s.CompleteTask(ctx, taskReq); err != nil {
			result.Error = err.Error()
			resp.Accepted = false
		} else {
			result.Accepted = true
		}
		resp.Results = append(resp.Results, result)
	}
	return resp, nil
}

// appendTaskStarted writes the durable TaskStarted event for a specific lease.
func (s *Service) appendTaskStarted(ctx context.Context, task metadata.Task, workerID string) error {
	payload, err := marshalTaskLifecyclePayload(taskLifecyclePayload{
		TaskLeaseEpoch: task.TaskLeaseEpoch,
		WorkerID:       workerID,
		TimestampMs:    time.Now().UnixMilli(),
	})
	if err != nil {
		return err
	}
	_, err = s.appendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       taskStream(task.TaskID),
		EventType:      "TaskStarted",
		IdempotencyKey: fmt.Sprintf("%s:started:%s:%d", task.TaskID, workerID, task.TaskLeaseEpoch),
		Payload:        payload,
	})
	return err
}

// appendTaskTerminal writes either TaskCompleted or TaskFailed with lease-scoped
// idempotency so stale completions do not overwrite newer redeliveries during replay.
func (s *Service) appendTaskTerminal(ctx context.Context, task metadata.Task, status logservepb.TaskStatus, resultJSON []byte, taskErr string) error {
	eventType := "TaskCompleted"
	if status == logservepb.TaskStatus_TASK_STATUS_FAILED {
		eventType = "TaskFailed"
	}
	_, err := s.appendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       taskStream(task.TaskID),
		EventType:      eventType,
		IdempotencyKey: taskTerminalIdempotencyKey(task.TaskID, eventType, task.WorkerID, task.TaskLeaseEpoch),
		Payload:        taskTerminalLogPayload(task, status, resultJSON, taskErr),
	})
	return err
}

// taskTerminalLogPayload encodes terminal result, error, worker, and lease fields
// into the lifecycle payload format used by bootstrap replay.
func taskTerminalLogPayload(task metadata.Task, status logservepb.TaskStatus, resultJSON []byte, taskErr string) []byte {
	payload, err := marshalTaskLifecyclePayload(taskLifecyclePayload{
		TaskLeaseEpoch: task.TaskLeaseEpoch,
		WorkerID:       task.WorkerID,
		ResultJSON:     resultJSON,
		Error:          taskErr,
		TimestampMs:    time.Now().UnixMilli(),
	})
	if err != nil {
		return nil
	}
	return payload
}

// taskTerminalIdempotencyKey scopes terminal events by task, event type, worker,
// and lease epoch.
func taskTerminalIdempotencyKey(taskID, eventType, workerID string, leaseEpoch uint64) string {
	return fmt.Sprintf("%s:%s:%s:%d", taskID, eventType, workerID, leaseEpoch)
}

// enqueueTask submits a task without extra metadata mutation.
func (s *Service) enqueueTask(ctx context.Context, spec *logservepb.TaskSpec) (metadata.Task, bool, error) {
	return s.enqueueTaskWithMetadata(ctx, spec, nil)
}

// enqueueTaskWithMetadata is the shared log-first task submission path. It applies
// backpressure, handles idempotency, persists the TaskSubmitted event, then updates
// metadata and queue/scheduler indexes.
func (s *Service) enqueueTaskWithMetadata(ctx context.Context, spec *logservepb.TaskSpec, mutate func(*metadata.Task)) (metadata.Task, bool, error) {
	if spec.GetTaskId() == "" {
		spec.TaskId = newTaskID()
	}
	if err := s.normalizeTaskFunction(ctx, spec); err != nil {
		return metadata.Task{}, false, err
	}
	fingerprint, err := taskSpecFingerprint(spec)
	if err != nil {
		return metadata.Task{}, false, err
	}
	if task, ok := s.meta.GetTaskByIdempotencyKey(spec.GetIdempotencyKey()); ok {
		if err := ensureIdempotencyFingerprint("task", spec.GetIdempotencyKey(), task.IdempotencyFingerprint, fingerprint); err != nil {
			return metadata.Task{}, false, err
		}
		return task, true, nil
	}

	queueHighWatermark, _, logAppendSlowLimit := s.getBackpressureConfig()
	if logAppendSlowLimit > 0 {
		lastLogAppend := time.Duration(s.lastLogAppendMs.Load()) * time.Millisecond
		if lastLogAppend >= logAppendSlowLimit {
			return metadata.Task{}, false, fmt.Errorf("backpressure: last log append latency %dms exceeds slow threshold %dms", lastLogAppend.Milliseconds(), logAppendSlowLimit.Milliseconds())
		}
	}
	if queueHighWatermark > 0 {
		backlog := 0
		if s.useSchedulerV2() {
			backlog = s.scheduler.QueueDepth()
		} else {
			s.queueMu.Lock()
			backlog = len(s.queue)
			s.queueMu.Unlock()
		}
		if backlog >= int(queueHighWatermark) {
			return metadata.Task{}, false, fmt.Errorf("backpressure: queue backlog %d exceeds high watermark %d", backlog, queueHighWatermark)
		}
	}

	// The submitted event is the source of truth; metadata and queue state are
	// derived from it during bootstrap if the process exits after the log write.
	payload, err := marshalTaskSubmittedPayload(spec)
	if err != nil {
		return metadata.Task{}, false, err
	}
	if _, err := s.appendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       taskStream(spec.GetTaskId()),
		EventType:      "TaskSubmitted",
		IdempotencyKey: spec.GetTaskId() + ":submitted",
		Payload:        payload,
	}); err != nil {
		return metadata.Task{}, false, err
	}

	taskRecord := metadata.Task{
		TaskID:                 spec.GetTaskId(),
		TaskName:               spec.GetTaskName(),
		Status:                 logservepb.TaskStatus_TASK_STATUS_QUEUED,
		WorkflowID:             spec.GetWorkflowId(),
		StepID:                 spec.GetStepId(),
		TargetWorkerID:         spec.GetTargetWorkerId(),
		ActorID:                spec.GetActorId(),
		ActorCallID:            spec.GetActorCallId(),
		ActorEpoch:             spec.GetActorEpoch(),
		TaskLeaseEpoch:         spec.GetTaskLeaseEpoch(),
		LLMModelName:           spec.GetLlmModelName(),
		LLMModelVersion:        spec.GetLlmModelVersion(),
		IdempotencyFingerprint: fingerprint,
	}
	if mutate != nil {
		mutate(&taskRecord)
	}
	task, duplicate := s.meta.CreateTask(taskRecord, spec.GetIdempotencyKey())
	if !duplicate {
		if err := s.metadataPersisted(); err != nil {
			return metadata.Task{}, false, err
		}
	}
	if duplicate {
		if err := ensureIdempotencyFingerprint("task", spec.GetIdempotencyKey(), task.IdempotencyFingerprint, fingerprint); err != nil {
			return metadata.Task{}, false, err
		}
		return task, true, nil
	}

	s.specMu.Lock()
	s.specs[task.TaskID] = cloneSpec(spec)
	s.specMu.Unlock()

	if s.useSchedulerV2() {
		s.scheduler.Enqueue(s.schedulerMetaFromTask(task))
	} else {
		s.queueMu.Lock()
		s.queue = append(s.queue, task.TaskID)
		s.queueMu.Unlock()
	}
	s.notifyTaskAvailable()
	return task, false, nil
}

// marshalTaskSubmittedPayload stores the cloned TaskSpec in compact eventcodec form.
func marshalTaskSubmittedPayload(spec *logservepb.TaskSpec) ([]byte, error) {
	specData, err := proto.Marshal(cloneSpec(spec))
	if err != nil {
		return nil, err
	}
	return eventcodec.Marshal(eventcodec.KindTaskSubmitted, map[string]any{"task_spec": specData})
}

// unmarshalTaskSubmittedSpec accepts both eventcodec records and legacy JSON
// wrappers so old task logs remain replayable.
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

// scheduleReadySteps serializes workflow scheduling, creates task records for each
// currently ready step, and records the StepScheduled event after task submission.
func (s *Service) scheduleReadySteps(ctx context.Context, workflowID string) error {
	s.workflowMu.Lock()
	defer s.workflowMu.Unlock()

	state, ok := s.meta.GetWorkflow(workflowID)
	if !ok {
		return errors.New("workflow not found")
	}
	if state.Status != logservepb.WorkflowStatus_WORKFLOW_STATUS_RUNNING {
		return nil
	}

	for {
		stepDef, step, ready := state.PopReadyStep()
		if !ready {
			return nil
		}

		argsJSON, inputHash, err := workflow.ResolveCachedArgs(stepDef, step, state, s)
		if err != nil {
			return err
		}
		attempt := step.Attempts + 1
		taskID := newTaskID()
		taskIdem := fmt.Sprintf("%s:%s:%s:attempt:%d", workflowID, stepDef.StepID, inputHash, attempt)
		spec := &logservepb.TaskSpec{
			TaskId:          taskID,
			TaskName:        stepDef.TaskName,
			FunctionName:    stepDef.FunctionName,
			FunctionSource:  stepDef.FunctionSource,
			FunctionRef:     stepDef.FunctionRef,
			FunctionHash:    stepDef.FunctionHash,
			ArgsJson:        argsJSON,
			IdempotencyKey:  taskIdem,
			WorkflowId:      workflowID,
			StepId:          stepDef.StepID,
			LlmModelName:    stepDef.LLMModelName,
			LlmModelVersion: stepDef.LLMModelVersion,
			LlmAdapter:      stepDef.LLMAdapter,
			LlmMaxTokens:    stepDef.LLMMaxTokens,
			TimeoutMs:       stepDef.TimeoutMs,
		}
		now := workflow.NowMs()
		task, _, err := s.enqueueTask(ctx, spec)
		if err != nil {
			return err
		}
		payload, _ := workflow.MarshalEventPayload(workflow.EventPayload{
			WorkflowID:       workflowID,
			StepID:           stepDef.StepID,
			TaskID:           task.TaskID,
			Attempt:          attempt,
			InputHash:        inputHash,
			ResolvedArgsJSON: argsJSON,
			TimestampMs:      now,
		})
		if _, err := s.appendLog(ctx, &logservepb.AppendLogRequest{
			StreamId:       workflowStream(workflowID),
			EventType:      "StepScheduled",
			IdempotencyKey: fmt.Sprintf("%s:%s:%s:scheduled:%d", workflowID, stepDef.StepID, inputHash, attempt),
			Payload:        payload,
		}); err != nil {
			return err
		}
		if _, err := s.meta.UpdateWorkflow(workflowID, func(current *workflow.State) error {
			current.SetStepScheduled(stepDef.StepID, task.TaskID, attempt, inputHash, argsJSON, now)
			return nil
		}); err != nil {
			return err
		}
		observability.Info("workflow_step_scheduled", map[string]any{
			"workflow_id": workflowID,
			"step_id":     stepDef.StepID,
			"attempt":     attempt,
		})
		// Re-read state after each scheduling mutation so newly unblocked steps are based
		// on persisted workflow metadata rather than the stale local snapshot.
		state, _ = s.meta.GetWorkflow(workflowID)
	}
}

// markWorkflowStepStarted records and materializes the first start of a workflow
// step unless it has already reached a terminal step state.
func (s *Service) markWorkflowStepStarted(ctx context.Context, task metadata.Task, workerID string) error {
	s.workflowMu.Lock()
	defer s.workflowMu.Unlock()

	state, ok := s.meta.GetWorkflow(task.WorkflowID)
	if !ok {
		return errors.New("workflow not found")
	}
	step, ok := state.Step(task.StepID)
	if !ok {
		return errors.New("workflow step not found")
	}
	if step.Status == logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SUCCEEDED || step.Status == logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_FAILED {
		return nil
	}
	now := workflow.NowMs()
	payload, _ := workflow.MarshalEventPayload(workflow.EventPayload{
		WorkflowID:  task.WorkflowID,
		StepID:      task.StepID,
		TaskID:      task.TaskID,
		TimestampMs: now,
	})
	if _, err := s.appendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       workflowStream(task.WorkflowID),
		EventType:      "StepStarted",
		IdempotencyKey: task.WorkflowID + ":" + task.StepID + ":" + task.TaskID + ":started:" + workerID,
		Payload:        payload,
	}); err != nil {
		return err
	}
	_, err := s.meta.UpdateWorkflow(task.WorkflowID, func(current *workflow.State) error {
		current.SetStepStarted(task.StepID, task.TaskID, now)
		return nil
	})
	return err
}

// completeWorkflowStep records step success or failure, decides whether more steps
// should be scheduled, and applies retry limits for failed steps.
func (s *Service) completeWorkflowStep(ctx context.Context, task metadata.Task, status logservepb.TaskStatus, resultJSON []byte, taskErr string) (bool, error) {
	s.workflowMu.Lock()
	defer s.workflowMu.Unlock()

	state, ok := s.meta.GetWorkflow(task.WorkflowID)
	if !ok {
		return false, errors.New("workflow not found")
	}
	step, ok := state.Step(task.StepID)
	if !ok {
		return false, errors.New("workflow step not found")
	}
	if step.Status == logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SUCCEEDED {
		return false, nil
	}

	now := workflow.NowMs()
	latencyMs := int64(0)
	if step.StartedAtMs > 0 {
		latencyMs = now - step.StartedAtMs
	}

	if status == logservepb.TaskStatus_TASK_STATUS_SUCCEEDED {
		inline, ref, err := s.materializeResult(ctx, filepath.Join("workflows", task.WorkflowID, "steps", task.StepID), resultJSON)
		if err != nil {
			return false, err
		}
		payload, _ := workflow.MarshalEventPayload(workflow.EventPayload{
			WorkflowID:  task.WorkflowID,
			StepID:      task.StepID,
			TaskID:      task.TaskID,
			ResultJSON:  inline,
			ResultRef:   ref,
			TimestampMs: now,
			LatencyMs:   latencyMs,
		})
		if _, err := s.appendLog(ctx, &logservepb.AppendLogRequest{
			StreamId:       workflowStream(task.WorkflowID),
			EventType:      "StepSucceeded",
			IdempotencyKey: task.WorkflowID + ":" + task.StepID + ":" + step.LastInputHash + ":succeeded",
			Payload:        payload,
		}); err != nil {
			return false, err
		}
		updated, err := s.meta.UpdateWorkflow(task.WorkflowID, func(current *workflow.State) error {
			current.SetStepSucceeded(task.StepID, task.TaskID, inline, ref, now, latencyMs)
			return nil
		})
		if err != nil {
			return false, err
		}
		observability.Info("workflow_step_succeeded", map[string]any{
			"workflow_id": task.WorkflowID,
			"step_id":     task.StepID,
			"latency_ms":  latencyMs,
		})
		if workflowDone(updated) {
			if err := s.completeWorkflow(ctx, updated); err != nil {
				return false, err
			}
			return false, nil
		}
		return true, nil
	}

	payload, _ := workflow.MarshalEventPayload(workflow.EventPayload{
		WorkflowID:  task.WorkflowID,
		StepID:      task.StepID,
		TaskID:      task.TaskID,
		Error:       taskErr,
		TimestampMs: now,
		LatencyMs:   latencyMs,
	})
	if _, err := s.appendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       workflowStream(task.WorkflowID),
		EventType:      "StepFailed",
		IdempotencyKey: task.WorkflowID + ":" + task.StepID + ":" + task.TaskID + ":failed",
		Payload:        payload,
	}); err != nil {
		return false, err
	}

	maxAttempts := workflow.StepMaxAttempts(state.Definition, task.StepID)
	retry := int(step.Attempts) < maxAttempts
	updated, err := s.meta.UpdateWorkflow(task.WorkflowID, func(current *workflow.State) error {
		current.SetStepFailed(task.StepID, task.TaskID, taskErr, retry, now, latencyMs)
		return nil
	})
	if err != nil {
		return false, err
	}
	if retry {
		observability.Info("workflow_step_retrying", map[string]any{
			"workflow_id": task.WorkflowID,
			"step_id":     task.StepID,
			"attempts":    step.Attempts,
		})
		return true, nil
	}
	if err := s.failWorkflow(ctx, updated, taskErr); err != nil {
		return false, err
	}
	return false, nil
}

// completeWorkflow materializes the workflow result from the configured result step
// and records the WorkflowCompleted event.
func (s *Service) completeWorkflow(ctx context.Context, state workflow.State) error {
	resultStep, ok := state.Step(state.Definition.ResultStepID)
	if !ok {
		return errors.New("workflow result step not found")
	}
	resultJSON := resultStep.ResultJSON
	resultRef := resultStep.ResultRef
	if len(resultJSON) == 0 && resultRef != "" {
		data, err := s.LoadResult(resultRef)
		if err != nil {
			return err
		}
		resultJSON = data
	}
	inline, ref, err := s.materializeResult(ctx, filepath.Join("workflows", state.WorkflowID, "result"), resultJSON)
	if err != nil {
		return err
	}
	now := workflow.NowMs()
	latencyMs := now - state.CreatedAtMs
	payload, _ := workflow.MarshalEventPayload(workflow.EventPayload{
		WorkflowID:  state.WorkflowID,
		ResultJSON:  inline,
		ResultRef:   ref,
		TimestampMs: now,
		LatencyMs:   latencyMs,
	})
	if _, err := s.appendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       workflowStream(state.WorkflowID),
		EventType:      "WorkflowCompleted",
		IdempotencyKey: state.WorkflowID + ":completed",
		Payload:        payload,
	}); err != nil {
		return err
	}
	_, err = s.meta.UpdateWorkflow(state.WorkflowID, func(current *workflow.State) error {
		current.Status = logservepb.WorkflowStatus_WORKFLOW_STATUS_COMPLETED
		current.ResultJSON = append([]byte(nil), inline...)
		current.ResultRef = ref
		current.CompletedAtMs = now
		current.Error = ""
		return nil
	})
	if err == nil {
		observability.Info("workflow_completed", map[string]any{
			"workflow_id": state.WorkflowID,
			"latency_ms":  latencyMs,
		})
	}
	return err
}

// failWorkflow records and materializes terminal workflow failure.
func (s *Service) failWorkflow(ctx context.Context, state workflow.State, taskErr string) error {
	now := workflow.NowMs()
	payload, _ := workflow.MarshalEventPayload(workflow.EventPayload{
		WorkflowID:  state.WorkflowID,
		Error:       taskErr,
		TimestampMs: now,
	})
	if _, err := s.appendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       workflowStream(state.WorkflowID),
		EventType:      "WorkflowFailed",
		IdempotencyKey: state.WorkflowID + ":failed",
		Payload:        payload,
	}); err != nil {
		return err
	}
	_, err := s.meta.UpdateWorkflow(state.WorkflowID, func(current *workflow.State) error {
		current.Status = logservepb.WorkflowStatus_WORKFLOW_STATUS_FAILED
		current.Error = taskErr
		current.CompletedAtMs = now
		return nil
	})
	return err
}

// materializeResult keeps small results inline and spills large results to the
// configured object store, returning either inline JSON or a reference.
func (s *Service) materializeResult(ctx context.Context, namespace string, resultJSON []byte) (json.RawMessage, string, error) {
	if len(resultJSON) > s.resultInlineThreshold && s.resultStore != nil {
		ref, err := objectstore.PutBytes(ctx, s.resultStore, namespace, resultJSON)
		if err != nil {
			return nil, "", err
		}
		return nil, ref, nil
	}
	return append([]byte(nil), resultJSON...), "", nil
}

// LoadResult fetches a result reference from the configured object store.
func (s *Service) LoadResult(ref string) ([]byte, error) {
	if s.resultStore == nil {
		return nil, errors.New("result store is not configured")
	}
	return objectstore.GetBytes(context.Background(), s.resultStore, ref, -1)
}

// workflowDone reports whether every workflow step has succeeded.
func workflowDone(state workflow.State) bool {
	return state.AllStepsSucceeded()
}

// isTerminalTaskStatus reports whether a task status can no longer be leased.
func isTerminalTaskStatus(status logservepb.TaskStatus) bool {
	return status == logservepb.TaskStatus_TASK_STATUS_SUCCEEDED || status == logservepb.TaskStatus_TASK_STATUS_FAILED
}

// taskStatusResponse converts metadata task state into the public RPC response.
func taskStatusResponse(task metadata.Task) *logservepb.GetTaskStatusResponse {
	return &logservepb.GetTaskStatusResponse{
		TaskId:      task.TaskID,
		Status:      task.Status,
		ResultJson:  task.ResultJSON,
		Error:       task.Error,
		WorkerId:    task.WorkerID,
		CreatedAtMs: task.CreatedAtMs,
		UpdatedAtMs: task.UpdatedAtMs,
	}
}

// workflowStepDependsOn returns a copy of the dependency IDs for response shaping.
func workflowStepDependsOn(def workflow.Definition, stepID string) []string {
	step, ok := workflow.StepDefinitionByID(def, stepID)
	if !ok || len(step.DependsOn) == 0 {
		return nil
	}
	return append([]string(nil), step.DependsOn...)
}

// workflowStatusResponse converts workflow metadata into the public RPC response
// while copying byte slices so callers cannot mutate internal state.
func workflowStatusResponse(state workflow.State) *logservepb.GetWorkflowStatusResponse {
	stepStates := state.StepStatesInOrder()
	steps := make([]*logservepb.WorkflowStepState, 0, len(stepStates))
	for _, step := range stepStates {
		steps = append(steps, &logservepb.WorkflowStepState{
			StepId:        step.StepID,
			DependsOn:     workflowStepDependsOn(state.Definition, step.StepID),
			TaskName:      step.TaskName,
			Status:        step.Status,
			Attempts:      step.Attempts,
			TaskId:        step.TaskID,
			ResultJson:    append([]byte(nil), step.ResultJSON...),
			ResultRef:     step.ResultRef,
			Error:         step.Error,
			StartedAtMs:   step.StartedAtMs,
			CompletedAtMs: step.CompletedAtMs,
			LatencyMs:     step.LatencyMs,
		})
	}
	return &logservepb.GetWorkflowStatusResponse{
		WorkflowId:    state.WorkflowID,
		WorkflowName:  state.WorkflowName,
		Status:        state.Status,
		Steps:         steps,
		ResultJson:    append([]byte(nil), state.ResultJSON...),
		ResultRef:     state.ResultRef,
		Error:         state.Error,
		CreatedAtMs:   state.CreatedAtMs,
		UpdatedAtMs:   state.UpdatedAtMs,
		CompletedAtMs: state.CompletedAtMs,
		LatencyMs:     workflow.WorkflowLatencyMs(state),
	}
}

// cloneSpec deep-copies mutable TaskSpec byte fields before specs enter shared
// metadata, logs, queues, or worker responses.
func cloneSpec(spec *logservepb.TaskSpec) *logservepb.TaskSpec {
	if spec == nil {
		return nil
	}
	return &logservepb.TaskSpec{
		TaskId:            spec.GetTaskId(),
		TaskName:          spec.GetTaskName(),
		FunctionName:      spec.GetFunctionName(),
		FunctionSource:    spec.GetFunctionSource(),
		FunctionRef:       spec.GetFunctionRef(),
		FunctionHash:      spec.GetFunctionHash(),
		ArgsJson:          append([]byte(nil), spec.GetArgsJson()...),
		IdempotencyKey:    spec.GetIdempotencyKey(),
		WorkflowId:        spec.GetWorkflowId(),
		StepId:            spec.GetStepId(),
		TargetWorkerId:    spec.GetTargetWorkerId(),
		ActorId:           spec.GetActorId(),
		ActorCallId:       spec.GetActorCallId(),
		ActorClassName:    spec.GetActorClassName(),
		ActorClassSource:  spec.GetActorClassSource(),
		ActorMethod:       spec.GetActorMethod(),
		ActorStateJson:    append([]byte(nil), spec.GetActorStateJson()...),
		ActorInitArgsJson: append([]byte(nil), spec.GetActorInitArgsJson()...),
		ActorEpoch:        spec.GetActorEpoch(),
		LlmModelName:      spec.GetLlmModelName(),
		LlmModelVersion:   spec.GetLlmModelVersion(),
		LlmAdapter:        spec.GetLlmAdapter(),
		LlmMaxTokens:      spec.GetLlmMaxTokens(),
		TimeoutMs:         spec.GetTimeoutMs(),
		TaskLeaseEpoch:    spec.GetTaskLeaseEpoch(),
	}
}

// newTaskID returns a log-friendly random task identifier.
func newTaskID() string {
	return "task-" + randomHex()
}

// newWorkflowID returns a log-friendly random workflow identifier.
func newWorkflowID() string {
	return "wf-" + randomHex()
}

// newActorID returns a log-friendly random actor identifier.
func newActorID() string {
	return "actor-" + randomHex()
}

// newActorCallID returns a log-friendly random actor call identifier.
func newActorCallID() string {
	return "actor-call-" + randomHex()
}

// randomHex returns 64 bits of random hex, falling back to a timestamp-derived
// value only if the OS random source fails.
func randomHex() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().Format("20060102150405.000000000")))
	}
	return hex.EncodeToString(b[:])
}

// taskStream maps a task ID to its durable log stream name.
func taskStream(taskID string) string {
	return "task:" + taskID
}

// workflowStream maps a workflow ID to its durable log stream name.
func workflowStream(workflowID string) string {
	return "wf:" + workflowID
}

// actorStream maps an actor ID to its durable log stream name.
func actorStream(actorID string) string {
	return "actor:" + actorID
}

// sortedWorkers returns a deterministic worker order without mutating the caller slice.
func sortedWorkers(workers []metadata.Worker) []metadata.Worker {
	out := append([]metadata.Worker(nil), workers...)
	sort.Slice(out, func(i, j int) bool {
		return out[i].WorkerID < out[j].WorkerID
	})
	return out
}
