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
	"github.com/logserve/logserve/internal/metadata"
	"github.com/logserve/logserve/internal/objectstore"
	"github.com/logserve/logserve/internal/observability"
	"github.com/logserve/logserve/internal/workflow"
	"google.golang.org/protobuf/encoding/protojson"
)

const defaultResultInlineThreshold = 4096
const actorOwnerLease = 750 * time.Millisecond
const schedulerWorkerLease = 5 * time.Second
const localityQueueWait = 250 * time.Millisecond
const defaultQueueHighWatermark = 1024
const defaultRedeliveryTimeout = 30 * time.Second

type taskSubmittedPayload struct {
	TaskSpec json.RawMessage `json:"task_spec,omitempty"`
}

type Service struct {
	logservepb.UnimplementedControlServiceServer
	meta                  metadata.Store
	log                   logservepb.LogServiceClient
	queueMu               sync.Mutex
	queue                 []string
	specMu                sync.RWMutex
	specs                 map[string]*logservepb.TaskSpec
	workflowMu            sync.Mutex
	actorLocksMu          sync.Mutex
	actorLocks            map[string]*sync.Mutex
	resultStore           objectstore.Store
	resultInlineThreshold int
	configMu              sync.RWMutex
	schedulingPolicy      logservepb.SchedulingPolicy
	queueHighWatermark    uint32
	redeliveryTimeout     time.Duration
	logAppendSlowLimit    time.Duration
	lastLogAppendMs       atomic.Int64
}

func NewService(meta metadata.Store, logClient logservepb.LogServiceClient) *Service {
	store, _ := objectstore.OpenLocal(filepath.Join(os.TempDir(), "logserve-objectstore"))
	return NewServiceWithResultStore(meta, logClient, store, defaultResultInlineThreshold)
}

func NewServiceWithResultStore(meta metadata.Store, logClient logservepb.LogServiceClient, store objectstore.Store, threshold int) *Service {
	if threshold <= 0 {
		threshold = defaultResultInlineThreshold
	}
	return &Service{
		meta:                  meta,
		log:                   logClient,
		queue:                 make([]string, 0, 1024),
		specs:                 make(map[string]*logservepb.TaskSpec),
		actorLocks:            make(map[string]*sync.Mutex),
		resultStore:           store,
		resultInlineThreshold: threshold,
		schedulingPolicy:      logservepb.SchedulingPolicy_SCHEDULING_POLICY_LOCALITY_AWARE,
		queueHighWatermark:    defaultQueueHighWatermark,
		redeliveryTimeout:     defaultRedeliveryTimeout,
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

func (s *Service) SubmitTask(ctx context.Context, req *logservepb.SubmitTaskRequest) (*logservepb.SubmitTaskResponse, error) {
	if req.GetTaskName() == "" {
		return nil, errors.New("task_name is required")
	}
	if req.GetFunctionName() == "" {
		return nil, errors.New("function_name is required")
	}
	if req.GetFunctionSource() == "" {
		return nil, errors.New("function_source is required")
	}

	task, duplicate, err := s.enqueueTask(ctx, &logservepb.TaskSpec{
		TaskId:         newTaskID(),
		TaskName:       req.GetTaskName(),
		FunctionName:   req.GetFunctionName(),
		FunctionSource: req.GetFunctionSource(),
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

	workflowID := newWorkflowID()
	now := workflow.NowMs()
	state := workflow.NewState(workflowID, def, now)
	created, duplicate := s.meta.CreateWorkflow(state, req.GetIdempotencyKey())
	if duplicate {
		return &logservepb.SubmitWorkflowResponse{WorkflowId: created.WorkflowID, Status: created.Status}, nil
	}

	definitionJSON, err := json.Marshal(def)
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(workflow.EventPayload{
		WorkflowID:     workflowID,
		WorkflowName:   def.WorkflowName,
		DefinitionJSON: definitionJSON,
		TimestampMs:    now,
	})
	if _, err := s.appendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       workflowStream(workflowID),
		EventType:      "WorkflowStarted",
		IdempotencyKey: workflowID + ":started",
		Payload:        payload,
	}); err != nil {
		return nil, err
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
	resp, err := s.log.ReadLog(ctx, &logservepb.ReadLogRequest{
		StreamId: workflowStream(req.GetWorkflowId()),
		FromSeq:  1,
		Limit:    10_000,
	})
	if err != nil {
		return nil, err
	}
	replayed, err := workflow.Replay(req.GetWorkflowId(), resp.GetRecords())
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
	s.meta.UpsertWorker(metadata.Worker{
		WorkerID:     req.GetWorkerId(),
		Address:      req.GetAddress(),
		Labels:       req.GetLabels(),
		CachedModels: modelCacheFromProto(req.GetCachedModels()),
		Capacity:     req.GetCapacity(),
	})
	payload, _ := json.Marshal(map[string]any{
		"worker_id":     req.GetWorkerId(),
		"address":       req.GetAddress(),
		"labels":        req.GetLabels(),
		"cached_models": req.GetCachedModels(),
		"capacity":      req.GetCapacity(),
	})
	_, _ = s.appendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       "system:workers",
		EventType:      "WorkerRegistered",
		IdempotencyKey: req.GetWorkerId() + ":registered",
		Payload:        payload,
	})
	return &logservepb.RegisterWorkerResponse{Accepted: true}, nil
}

func (s *Service) Heartbeat(ctx context.Context, req *logservepb.HeartbeatRequest) (*logservepb.HeartbeatResponse, error) {
	if req.GetWorkerId() == "" {
		return nil, errors.New("worker_id is required")
	}
	s.meta.Heartbeat(req.GetWorkerId(), modelCacheFromProto(req.GetCachedModels()))
	return &logservepb.HeartbeatResponse{ServerTimeMs: time.Now().UnixMilli()}, nil
}

func (s *Service) PollTask(ctx context.Context, req *logservepb.PollTaskRequest) (*logservepb.PollTaskResponse, error) {
	if req.GetWorkerId() == "" {
		return nil, errors.New("worker_id is required")
	}
	if err := s.redeliverExpiredTasks(ctx); err != nil {
		return nil, err
	}
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	for i, taskID := range s.queue {
		s.specMu.RLock()
		spec, ok := s.specs[taskID]
		s.specMu.RUnlock()
		if !ok {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			continue
		}
		if !s.canAssignTaskToWorker(taskID, spec, req.GetWorkerId()) {
			continue
		}
		before, _ := s.meta.GetTask(taskID)
		leased, err := s.meta.LeaseTask(taskID, req.GetWorkerId())
		if err != nil {
			return nil, err
		}
		if before.Status == logservepb.TaskStatus_TASK_STATUS_QUEUED {
			s.meta.IncrementWorkerLoad(req.GetWorkerId())
		}
		s.queue = append(s.queue[:i], s.queue[i+1:]...)
		leasedSpec := cloneSpec(spec)
		leasedSpec.TaskLeaseEpoch = leased.TaskLeaseEpoch
		return &logservepb.PollTaskResponse{HasTask: true, Task: leasedSpec}, nil
	}
	return &logservepb.PollTaskResponse{HasTask: false}, nil
}

func (s *Service) StartTask(ctx context.Context, req *logservepb.StartTaskRequest) (*logservepb.StartTaskResponse, error) {
	task, err := s.meta.ValidateTaskLease(req.GetTaskId(), req.GetWorkerId(), req.GetTaskLeaseEpoch())
	if err != nil {
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
	if ok && existing.ActorID != "" {
		if isTerminalTaskStatus(existing.Status) {
			return &logservepb.CompleteTaskResponse{Accepted: true}, nil
		}
		if err := s.completeActorCall(ctx, existing, req); err != nil {
			return nil, err
		}
		if _, err := s.meta.CompleteTask(req.GetTaskId(), req.GetWorkerId(), req.GetTaskLeaseEpoch(), req.GetStatus(), req.GetResultJson(), req.GetError()); err != nil {
			return nil, err
		}
		if existing.Status == logservepb.TaskStatus_TASK_STATUS_RUNNING {
			s.meta.DecrementWorkerLoad(req.GetWorkerId())
		}
		return &logservepb.CompleteTaskResponse{Accepted: true}, nil
	}
	task, err := s.meta.CompleteTask(req.GetTaskId(), req.GetWorkerId(), req.GetTaskLeaseEpoch(), req.GetStatus(), req.GetResultJson(), req.GetError())
	if err != nil {
		return nil, err
	}
	if ok && existing.Status == logservepb.TaskStatus_TASK_STATUS_RUNNING {
		s.meta.DecrementWorkerLoad(req.GetWorkerId())
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
	return &logservepb.CompleteTaskResponse{Accepted: true}, nil
}

func (s *Service) enqueueTask(ctx context.Context, spec *logservepb.TaskSpec) (metadata.Task, bool, error) {
	if spec.GetTaskId() == "" {
		spec.TaskId = newTaskID()
	}
	if task, ok := s.meta.GetTaskByIdempotencyKey(spec.GetIdempotencyKey()); ok {
		return task, true, nil
	}

	queueHighWatermark, _, logAppendSlowLimit := s.getBackpressureConfig()
	if logAppendSlowLimit > 0 {
		lastLogAppend := time.Duration(s.lastLogAppendMs.Load()) * time.Millisecond
		if lastLogAppend >= logAppendSlowLimit {
			return metadata.Task{}, false, fmt.Errorf("backpressure: last log append latency %dms exceeds slow threshold %dms", lastLogAppend.Milliseconds(), logAppendSlowLimit.Milliseconds())
		}
	}
	s.queueMu.Lock()
	if queueHighWatermark > 0 && len(s.queue) >= int(queueHighWatermark) {
		backlog := len(s.queue)
		s.queueMu.Unlock()
		return metadata.Task{}, false, fmt.Errorf("backpressure: queue backlog %d exceeds high watermark %d", backlog, queueHighWatermark)
	}
	s.queueMu.Unlock()

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

	task, duplicate := s.meta.CreateTask(metadata.Task{
		TaskID:          spec.GetTaskId(),
		TaskName:        spec.GetTaskName(),
		Status:          logservepb.TaskStatus_TASK_STATUS_QUEUED,
		WorkflowID:      spec.GetWorkflowId(),
		StepID:          spec.GetStepId(),
		TargetWorkerID:  spec.GetTargetWorkerId(),
		ActorID:         spec.GetActorId(),
		ActorCallID:     spec.GetActorCallId(),
		ActorEpoch:      spec.GetActorEpoch(),
		TaskLeaseEpoch:  spec.GetTaskLeaseEpoch(),
		LLMModelName:    spec.GetLlmModelName(),
		LLMModelVersion: spec.GetLlmModelVersion(),
	}, spec.GetIdempotencyKey())
	if duplicate {
		return task, true, nil
	}

	s.specMu.Lock()
	s.specs[task.TaskID] = cloneSpec(spec)
	s.specMu.Unlock()

	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	s.queue = append(s.queue, task.TaskID)
	return task, false, nil
}

func marshalTaskSubmittedPayload(spec *logservepb.TaskSpec) ([]byte, error) {
	specJSON, err := protojson.Marshal(cloneSpec(spec))
	if err != nil {
		return nil, err
	}
	return json.Marshal(taskSubmittedPayload{TaskSpec: specJSON})
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

		argsJSON, inputHash, err := workflow.ResolveArgs(stepDef, state, s)
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
		payload, _ := json.Marshal(workflow.EventPayload{
			WorkflowID:  workflowID,
			StepID:      stepDef.StepID,
			TaskID:      taskID,
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
			currentStep.TaskID = taskID
			currentStep.Attempts = attempt
			currentStep.LastInputHash = inputHash
			currentStep.LastScheduledAtMs = now
			currentStep.Error = ""
			current.Steps[stepDef.StepID] = currentStep
			return nil
		}); err != nil {
			return err
		}
		if _, _, err := s.enqueueTask(ctx, spec); err != nil {
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
	payload, _ := json.Marshal(workflow.EventPayload{
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
		payload, _ := json.Marshal(workflow.EventPayload{
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

	payload, _ := json.Marshal(workflow.EventPayload{
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
	payload, _ := json.Marshal(workflow.EventPayload{
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
	payload, _ := json.Marshal(workflow.EventPayload{
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
		ref, err := s.resultStore.Put(ctx, namespace, resultJSON)
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
	return s.resultStore.Get(context.Background(), ref)
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
