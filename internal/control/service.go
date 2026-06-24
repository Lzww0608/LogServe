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
	"github.com/logserve/logserve/internal/eventcodec"
	"github.com/logserve/logserve/internal/metadata"
	"github.com/logserve/logserve/internal/objectstore"
	"github.com/logserve/logserve/internal/observability"
	"github.com/logserve/logserve/internal/workflow"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const defaultResultInlineThreshold = 4096
const actorOwnerLease = 750 * time.Millisecond
const schedulerWorkerLease = 5 * time.Second
const localityQueueWait = 250 * time.Millisecond
const defaultQueueHighWatermark = 1024
const defaultRedeliveryTimeout = 30 * time.Second
const defaultPollBatchLimit = 64
const maxPollWaitTimeout = 5 * time.Second

type taskSubmittedPayload struct {
	TaskSpec json.RawMessage `json:"task_spec,omitempty"`
}

type taskLifecyclePayload struct {
	TaskLeaseEpoch uint64          `json:"task_lease_epoch,omitempty"`
	WorkerID       string          `json:"worker_id,omitempty"`
	ResultJSON     json.RawMessage `json:"result_json,omitempty"`
	Error          string          `json:"error,omitempty"`
	TimestampMs    int64           `json:"timestamp_ms,omitempty"`
}

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
	actorLocksMu          sync.Mutex
	actorLocks            map[string]*sync.Mutex
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

func NewService(meta metadata.Store, logClient logClient) *Service {
	store, _ := objectstore.OpenLocal(filepath.Join(os.TempDir(), "logserve-objectstore"))
	return NewServiceWithResultStore(meta, logClient, store, defaultResultInlineThreshold)
}

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
		actorLocks:            make(map[string]*sync.Mutex),
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

func pollBatchLimit(requested uint32) int {
	if requested == 0 {
		return 1
	}
	if requested > defaultPollBatchLimit {
		return defaultPollBatchLimit
	}
	return int(requested)
}

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

func (s *Service) taskAvailableSignal() <-chan struct{} {
	s.taskNotifyMu.Lock()
	defer s.taskNotifyMu.Unlock()
	if s.taskNotifyCh == nil {
		s.taskNotifyCh = make(chan struct{})
	}
	return s.taskNotifyCh
}

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
	close(s.taskNotifyCh)
	s.taskNotifyCh = make(chan struct{})
}
func (s *Service) getBackpressureConfig() (uint32, time.Duration, time.Duration) {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.queueHighWatermark, s.redeliveryTimeout, s.logAppendSlowLimit
}

func (s *Service) getSchedulingPolicy() logservepb.SchedulingPolicy {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.schedulingPolicy
}

func (s *Service) useSchedulerV2() bool {
	return s != nil && s.schedulerV2 && s.scheduler != nil
}

func (s *Service) syncSchedulerWorkers() {
	if !s.useSchedulerV2() {
		return
	}
	for _, worker := range s.meta.ListWorkers() {
		s.scheduler.UpsertWorker(worker)
	}
}

func (s *Service) updateSchedulerWorker(workerID string) {
	if !s.useSchedulerV2() || workerID == "" {
		return
	}
	if worker, ok := s.meta.GetWorker(workerID); ok {
		s.scheduler.UpsertWorker(worker)
	}
}

func (s *Service) completeSchedulerRunning(taskID string) {
	if s != nil && s.scheduler != nil {
		s.scheduler.CompleteRunning(taskID)
	}
}

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

func schedulerLeaseDeadlineMs(task metadata.Task, timeout time.Duration) int64 {
	if timeout <= 0 || task.UpdatedAtMs == 0 {
		return 0
	}
	return task.UpdatedAtMs + timeout.Milliseconds()
}

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

func (s *Service) GetTaskStatus(ctx context.Context, req *logservepb.GetTaskStatusRequest) (*logservepb.GetTaskStatusResponse, error) {
	task, ok := s.meta.GetTask(req.GetTaskId())
	if !ok {
		return nil, errors.New("task not found")
	}
	return taskStatusResponse(task), nil
}

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

func (s *Service) GetWorkflowStatus(ctx context.Context, req *logservepb.GetWorkflowStatusRequest) (*logservepb.GetWorkflowStatusResponse, error) {
	state, ok := s.meta.GetWorkflow(req.GetWorkflowId())
	if !ok {
		return nil, errors.New("workflow not found")
	}
	return workflowStatusResponse(state), nil
}

func (s *Service) ReplayWorkflow(ctx context.Context, req *logservepb.ReplayWorkflowRequest) (*logservepb.ReplayWorkflowResponse, error) {
	streamID := workflowStream(req.GetWorkflowId())
	replayed, err := workflow.ReplayEach(req.GetWorkflowId(), func(emit func(*logservepb.LogRecord) error) error {
		return s.forEachLogRecord(ctx, streamID, emit)
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

func (s *Service) PollTask(ctx context.Context, req *logservepb.PollTaskRequest) (*logservepb.PollTaskResponse, error) {
	if req.GetWorkerId() == "" {
		return nil, errors.New("worker_id is required")
	}
	maxTasks := pollBatchLimit(req.GetMaxTasks())
	wait := pollWaitTimeout(req.GetWaitTimeoutMs())
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

func (s *Service) pollTaskIndexed(workerID string, maxTasks int) ([]*logservepb.TaskSpec, error) {
	s.syncSchedulerWorkers()
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
	return leasedSpec
}

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

func (s *Service) appendTaskStarted(ctx context.Context, task metadata.Task, workerID string) error {
	payload, _ := json.Marshal(map[string]any{
		"task_id":          task.TaskID,
		"worker_id":        workerID,
		"task_lease_epoch": task.TaskLeaseEpoch,
		"timestamp_ms":     time.Now().UnixMilli(),
	})
	_, err := s.appendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       taskStream(task.TaskID),
		EventType:      "TaskStarted",
		IdempotencyKey: fmt.Sprintf("%s:started:%s:%d", task.TaskID, workerID, task.TaskLeaseEpoch),
		Payload:        payload,
	})
	return err
}

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

func taskTerminalLogPayload(task metadata.Task, status logservepb.TaskStatus, resultJSON []byte, taskErr string) []byte {
	payload := map[string]any{
		"task_id":          task.TaskID,
		"worker_id":        task.WorkerID,
		"status":           status.String(),
		"task_lease_epoch": task.TaskLeaseEpoch,
		"timestamp_ms":     time.Now().UnixMilli(),
	}
	if len(resultJSON) > 0 {
		payload["result_json"] = json.RawMessage(resultJSON)
	}
	if taskErr != "" {
		payload["error"] = taskErr
	}
	data, _ := json.Marshal(payload)
	return data
}

func taskTerminalIdempotencyKey(taskID, eventType, workerID string, leaseEpoch uint64) string {
	return fmt.Sprintf("%s:%s:%s:%d", taskID, eventType, workerID, leaseEpoch)
}
func (s *Service) enqueueTask(ctx context.Context, spec *logservepb.TaskSpec) (metadata.Task, bool, error) {
	return s.enqueueTaskWithMetadata(ctx, spec, nil)
}

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

func marshalTaskSubmittedPayload(spec *logservepb.TaskSpec) ([]byte, error) {
	specData, err := proto.Marshal(cloneSpec(spec))
	if err != nil {
		return nil, err
	}
	return eventcodec.Marshal(eventcodec.KindTaskSubmitted, map[string]any{"task_spec": specData})
}

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

	for _, stepDef := range state.Definition.Steps {
		step := state.Steps[stepDef.StepID]
		if step.Status != logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SCHEDULED || step.TaskID != "" {
			continue
		}
		if !workflow.DependenciesSucceeded(stepDef, state) {
			continue
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
			WorkflowID:  workflowID,
			StepID:      stepDef.StepID,
			TaskID:      task.TaskID,
			Attempt:     attempt,
			InputHash:   inputHash,
			TimestampMs: now,
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
			currentStep := current.Steps[stepDef.StepID]
			currentStep.Status = logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SCHEDULED
			currentStep.TaskID = task.TaskID
			currentStep.Attempts = attempt
			currentStep.LastInputHash = inputHash
			currentStep.LastScheduledAtMs = now
			currentStep.Error = ""
			current.Steps[stepDef.StepID] = currentStep
			return nil
		}); err != nil {
			return err
		}
		observability.Info("workflow_step_scheduled", map[string]any{
			"workflow_id": workflowID,
			"step_id":     stepDef.StepID,
			"attempt":     attempt,
		})
		state, _ = s.meta.GetWorkflow(workflowID)
	}
	return nil
}

func (s *Service) markWorkflowStepStarted(ctx context.Context, task metadata.Task, workerID string) error {
	s.workflowMu.Lock()
	defer s.workflowMu.Unlock()

	state, ok := s.meta.GetWorkflow(task.WorkflowID)
	if !ok {
		return errors.New("workflow not found")
	}
	step := state.Steps[task.StepID]
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
		currentStep := current.Steps[task.StepID]
		if currentStep.Status == logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SUCCEEDED {
			return nil
		}
		currentStep.Status = logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_STARTED
		currentStep.TaskID = task.TaskID
		currentStep.StartedAtMs = now
		current.Steps[task.StepID] = currentStep
		return nil
	})
	return err
}

func (s *Service) completeWorkflowStep(ctx context.Context, task metadata.Task, status logservepb.TaskStatus, resultJSON []byte, taskErr string) (bool, error) {
	s.workflowMu.Lock()
	defer s.workflowMu.Unlock()

	state, ok := s.meta.GetWorkflow(task.WorkflowID)
	if !ok {
		return false, errors.New("workflow not found")
	}
	step := state.Steps[task.StepID]
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
			currentStep := current.Steps[task.StepID]
			currentStep.Status = logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SUCCEEDED
			currentStep.TaskID = task.TaskID
			currentStep.ResultJSON = append([]byte(nil), inline...)
			currentStep.ResultRef = ref
			currentStep.Error = ""
			currentStep.CompletedAtMs = now
			currentStep.LatencyMs = latencyMs
			current.Steps[task.StepID] = currentStep
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
		currentStep := current.Steps[task.StepID]
		currentStep.Status = logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_FAILED
		currentStep.TaskID = task.TaskID
		currentStep.Error = taskErr
		currentStep.CompletedAtMs = now
		currentStep.LatencyMs = latencyMs
		if retry {
			currentStep.Status = logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SCHEDULED
			currentStep.TaskID = ""
		}
		current.Steps[task.StepID] = currentStep
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

func (s *Service) completeWorkflow(ctx context.Context, state workflow.State) error {
	resultStep := state.Steps[state.Definition.ResultStepID]
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

func (s *Service) LoadResult(ref string) ([]byte, error) {
	if s.resultStore == nil {
		return nil, errors.New("result store is not configured")
	}
	return objectstore.GetBytes(context.Background(), s.resultStore, ref, -1)
}

func workflowDone(state workflow.State) bool {
	for _, stepID := range state.StepOrder {
		if state.Steps[stepID].Status != logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SUCCEEDED {
			return false
		}
	}
	return true
}

func isTerminalTaskStatus(status logservepb.TaskStatus) bool {
	return status == logservepb.TaskStatus_TASK_STATUS_SUCCEEDED || status == logservepb.TaskStatus_TASK_STATUS_FAILED
}

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

func workflowStatusResponse(state workflow.State) *logservepb.GetWorkflowStatusResponse {
	steps := make([]*logservepb.WorkflowStepState, 0, len(state.StepOrder))
	for _, stepID := range state.StepOrder {
		step := state.Steps[stepID]
		steps = append(steps, &logservepb.WorkflowStepState{
			StepId:        step.StepID,
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

func newTaskID() string {
	return "task-" + randomHex()
}

func newWorkflowID() string {
	return "wf-" + randomHex()
}

func newActorID() string {
	return "actor-" + randomHex()
}

func newActorCallID() string {
	return "actor-call-" + randomHex()
}

func randomHex() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().Format("20060102150405.000000000")))
	}
	return hex.EncodeToString(b[:])
}

func taskStream(taskID string) string {
	return "task:" + taskID
}

func workflowStream(workflowID string) string {
	return "wf:" + workflowID
}

func actorStream(actorID string) string {
	return "actor:" + actorID
}

func (s *Service) actorLock(actorID string) *sync.Mutex {
	s.actorLocksMu.Lock()
	defer s.actorLocksMu.Unlock()
	lock, ok := s.actorLocks[actorID]
	if !ok {
		lock = &sync.Mutex{}
		s.actorLocks[actorID] = lock
	}
	return lock
}

func sortedWorkers(workers []metadata.Worker) []metadata.Worker {
	out := append([]metadata.Worker(nil), workers...)
	sort.Slice(out, func(i, j int) bool {
		return out[i].WorkerID < out[j].WorkerID
	})
	return out
}
