package control

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/actor"
	"github.com/logserve/logserve/internal/logrecord"
	"github.com/logserve/logserve/internal/metadata"
	"github.com/logserve/logserve/internal/observability"
	"github.com/logserve/logserve/internal/workflow"
)

const bootstrapReadLimit = 1000

func (s *Service) BootstrapFromLog(ctx context.Context) error {
	if err := s.bootstrapModels(ctx); err != nil {
		return err
	}
	if err := s.bootstrapWorkers(ctx); err != nil {
		return err
	}
	if err := s.bootstrapScheduler(ctx); err != nil {
		return err
	}
	if err := s.bootstrapBackpressure(ctx); err != nil {
		return err
	}
	if err := s.bootstrapFunctions(ctx); err != nil {
		return err
	}
	checkpoint, err := s.loadLatestMetadataCheckpoint(ctx)
	if err != nil {
		return err
	}
	if checkpoint != nil {
		checkpoint.normalizeStreamKinds()
		if err := s.bootstrapMetadataFromCheckpoint(ctx, *checkpoint); err != nil {
			observability.Error("metadata_checkpoint_bootstrap_failed", err, map[string]any{"checkpoint_id": checkpoint.ID})
		} else {
			return nil
		}
	}
	if err := s.bootstrapTasks(ctx); err != nil {
		return err
	}
	if err := s.bootstrapWorkflows(ctx); err != nil {
		return err
	}
	if err := s.bootstrapActors(ctx); err != nil {
		return err
	}
	if err := s.bootstrapLLMStats(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Service) bootstrapTasks(ctx context.Context) error {
	streams, err := s.listStreams(ctx, "task:")
	if err != nil {
		return err
	}
	for _, streamID := range streams {
		state, err := replayTaskMetadataRawEach(func(emit func(logrecord.RawRecord) error) error {
			return s.forEachRawLogRecord(ctx, streamID, 1, emit)
		}, nil)
		if err != nil {
			return err
		}
		if state == nil || !state.ok || state.spec == nil {
			continue
		}
		if err := s.restoreTaskReplayState(state); err != nil {
			return err
		}
	}
	return nil
}
func replayTaskSpec(records []*logservepb.LogRecord) (*logservepb.TaskSpec, logservepb.TaskStatus, uint64, bool, error) {
	return replayTaskSpecEach(func(emit func(*logservepb.LogRecord) error) error {
		for _, rec := range records {
			if err := emit(rec); err != nil {
				return err
			}
		}
		return nil
	})
}

func replayTaskSpecEach(iterate func(func(*logservepb.LogRecord) error) error) (*logservepb.TaskSpec, logservepb.TaskStatus, uint64, bool, error) {
	var spec *logservepb.TaskSpec
	status := logservepb.TaskStatus_TASK_STATUS_QUEUED
	var leaseEpoch uint64
	var redeliveredLeaseEpoch uint64
	if iterate == nil {
		return nil, status, leaseEpoch, false, nil
	}
	if err := iterate(func(rec *logservepb.LogRecord) error {
		switch rec.GetEventType() {
		case "TaskSubmitted":
			decoded, err := unmarshalTaskSubmittedSpec(rec.GetPayload())
			if err != nil {
				return err
			}
			if decoded.GetTaskId() == "" {
				return nil
			}
			spec = decoded
			status = logservepb.TaskStatus_TASK_STATUS_QUEUED
		case "TaskStarted":
			if isTerminalTaskStatus(status) {
				return nil
			}
			payloadEpoch := taskEventLeaseEpoch(rec.GetPayload())
			if payloadEpoch == 0 {
				if leaseEpoch == 0 && redeliveredLeaseEpoch == 0 {
					status = logservepb.TaskStatus_TASK_STATUS_RUNNING
				}
				return nil
			}
			if payloadEpoch <= redeliveredLeaseEpoch {
				return nil
			}
			if payloadEpoch >= leaseEpoch {
				leaseEpoch = payloadEpoch
				status = logservepb.TaskStatus_TASK_STATUS_RUNNING
			}
		case "TaskRedelivered":
			if isTerminalTaskStatus(status) {
				return nil
			}
			payloadEpoch := taskEventLeaseEpoch(rec.GetPayload())
			if payloadEpoch > redeliveredLeaseEpoch {
				redeliveredLeaseEpoch = payloadEpoch
			}
			if payloadEpoch == 0 || payloadEpoch >= leaseEpoch {
				status = logservepb.TaskStatus_TASK_STATUS_QUEUED
			}
		case "TaskCompleted":
			if taskTerminalEventApplies(status, leaseEpoch, redeliveredLeaseEpoch, rec.GetPayload()) {
				status = logservepb.TaskStatus_TASK_STATUS_SUCCEEDED
			}
		case "TaskFailed":
			if taskTerminalEventApplies(status, leaseEpoch, redeliveredLeaseEpoch, rec.GetPayload()) {
				status = logservepb.TaskStatus_TASK_STATUS_FAILED
			}
		}
		return nil
	}); err != nil {
		return nil, 0, 0, false, err
	}
	if spec == nil {
		return nil, status, leaseEpoch, false, nil
	}
	spec.TaskLeaseEpoch = leaseEpoch
	if status == logservepb.TaskStatus_TASK_STATUS_RUNNING {
		status = logservepb.TaskStatus_TASK_STATUS_QUEUED
	}
	return spec, status, leaseEpoch, true, nil
}
func taskTerminalEventApplies(status logservepb.TaskStatus, currentLeaseEpoch, redeliveredLeaseEpoch uint64, payload []byte) bool {
	if isTerminalTaskStatus(status) {
		return false
	}
	eventLeaseEpoch := taskEventLeaseEpoch(payload)
	if eventLeaseEpoch == 0 {
		return currentLeaseEpoch == 0 && redeliveredLeaseEpoch == 0
	}
	if status != logservepb.TaskStatus_TASK_STATUS_RUNNING {
		return false
	}
	return eventLeaseEpoch == currentLeaseEpoch && eventLeaseEpoch > redeliveredLeaseEpoch
}

func taskEventLeaseEpoch(payload []byte) uint64 {
	var decoded taskLifecyclePayload
	if len(payload) == 0 {
		return 0
	}
	_ = json.Unmarshal(payload, &decoded)
	return decoded.TaskLeaseEpoch
}

func (s *Service) bootstrapModels(ctx context.Context) error {
	return s.forEachLogRecord(ctx, "system:models", func(rec *logservepb.LogRecord) error {
		if rec.GetEventType() != "ModelRegistered" {
			return nil
		}
		var model logservepb.ModelInfo
		if err := json.Unmarshal(rec.GetPayload(), &model); err != nil {
			return err
		}
		s.meta.RegisterModel(&model)
		return nil
	})
}

func (s *Service) bootstrapWorkers(ctx context.Context) error {
	return s.forEachLogRecord(ctx, "system:workers", func(rec *logservepb.LogRecord) error {
		if rec.GetEventType() != "WorkerRegistered" {
			return nil
		}
		var payload struct {
			WorkerID     string `json:"worker_id"`
			Address      string `json:"address"`
			Labels       map[string]string
			CachedModels []struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"cached_models"`
			Capacity uint32 `json:"capacity"`
		}
		if err := json.Unmarshal(rec.GetPayload(), &payload); err != nil {
			return err
		}
		if payload.WorkerID == "" {
			return nil
		}
		cachedModels := make(map[string]bool, len(payload.CachedModels))
		for _, model := range payload.CachedModels {
			if model.Name == "" {
				continue
			}
			cachedModels[metadata.ModelKey(model.Name, model.Version)] = true
		}
		s.meta.UpsertWorker(metadata.Worker{
			WorkerID:      payload.WorkerID,
			Address:       payload.Address,
			Labels:        payload.Labels,
			CachedModels:  cachedModels,
			Capacity:      payload.Capacity,
			LastHeartbeat: rec.GetTimestampMs(),
		})
		s.updateSchedulerWorker(payload.WorkerID)
		return nil
	})
}

func (s *Service) bootstrapScheduler(ctx context.Context) error {
	var policy logservepb.SchedulingPolicy
	if err := s.forEachLogRecord(ctx, "system:scheduler", func(rec *logservepb.LogRecord) error {
		if rec.GetEventType() != "SchedulingPolicyChanged" {
			return nil
		}
		var payload struct {
			Policy string `json:"policy"`
		}
		if err := json.Unmarshal(rec.GetPayload(), &payload); err != nil {
			return err
		}
		if value, ok := logservepb.SchedulingPolicy_value[payload.Policy]; ok {
			policy = logservepb.SchedulingPolicy(value)
		}
		return nil
	}); err != nil {
		return err
	}
	if policy != logservepb.SchedulingPolicy_SCHEDULING_POLICY_UNSPECIFIED {
		s.configMu.Lock()
		s.schedulingPolicy = policy
		s.configMu.Unlock()
	}
	return nil
}

func (s *Service) bootstrapBackpressure(ctx context.Context) error {
	var payload struct {
		QueueHighWatermark uint32 `json:"queue_high_watermark"`
		RedeliveryTimeout  int64  `json:"redelivery_timeout_ms"`
		LogAppendSlow      int64  `json:"log_append_slow_ms"`
	}
	if err := s.forEachLogRecord(ctx, "system:backpressure", func(rec *logservepb.LogRecord) error {
		if rec.GetEventType() != "BackpressureConfigured" {
			return nil
		}
		return json.Unmarshal(rec.GetPayload(), &payload)
	}); err != nil {
		return err
	}
	s.configMu.Lock()
	if payload.QueueHighWatermark > 0 {
		s.queueHighWatermark = payload.QueueHighWatermark
	}
	if payload.RedeliveryTimeout > 0 {
		s.redeliveryTimeout = time.Duration(payload.RedeliveryTimeout) * time.Millisecond
	}
	if payload.LogAppendSlow > 0 {
		s.logAppendSlowLimit = time.Duration(payload.LogAppendSlow) * time.Millisecond
	}
	s.configMu.Unlock()
	return nil
}

func (s *Service) bootstrapWorkflows(ctx context.Context) error {
	return s.bootstrapWorkflowsWithScheduling(ctx, true)
}

func (s *Service) bootstrapWorkflowsWithScheduling(ctx context.Context, schedule bool) error {
	streams, err := s.listStreams(ctx, "wf:")
	if err != nil {
		return err
	}
	for _, streamID := range streams {
		workflowID := strings.TrimPrefix(streamID, "wf:")
		state, err := workflow.ReplayRawEach(workflowID, func(emit func(logrecord.RawRecord) error) error {
			return s.forEachRawLogRecord(ctx, streamID, 1, emit)
		})
		if err != nil {
			continue
		}
		s.prepareRetryableFailedSteps(&state)
		s.meta.UpsertWorkflow(state)
		if !schedule {
			continue
		}
		if err := s.restoreWorkflowTasks(state); err != nil {
			return err
		}
		if state.Status == logservepb.WorkflowStatus_WORKFLOW_STATUS_RUNNING {
			if err := s.scheduleReadySteps(ctx, state.WorkflowID); err != nil {
				return err
			}
		}
	}
	return nil
}
func (s *Service) prepareRetryableFailedSteps(state *workflow.State) {
	if state.Status != logservepb.WorkflowStatus_WORKFLOW_STATUS_RUNNING {
		return
	}
	for _, step := range state.StepStatesInOrder() {
		if step.Status != logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_FAILED {
			continue
		}
		if int(step.Attempts) >= workflow.StepMaxAttempts(state.Definition, step.StepID) {
			continue
		}
		state.SetStepFailed(step.StepID, step.TaskID, step.Error, true, step.CompletedAtMs, step.LatencyMs)
	}
}

func (s *Service) restoreWorkflowTasks(state workflow.State) error {
	if state.Status != logservepb.WorkflowStatus_WORKFLOW_STATUS_RUNNING {
		return nil
	}
	for _, stepDef := range state.Definition.Steps {
		step, ok := state.Step(stepDef.StepID)
		if !ok || step.TaskID == "" {
			continue
		}
		if step.Status != logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SCHEDULED &&
			step.Status != logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_STARTED {
			continue
		}
		argsJSON, inputHash, err := workflow.ResolveCachedArgs(stepDef, step, state, s)
		if err != nil {
			return err
		}
		attempt := step.Attempts
		if attempt == 0 {
			attempt = 1
		}
		spec := &logservepb.TaskSpec{
			TaskId:          step.TaskID,
			TaskName:        stepDef.TaskName,
			FunctionName:    stepDef.FunctionName,
			FunctionSource:  stepDef.FunctionSource,
			FunctionRef:     stepDef.FunctionRef,
			FunctionHash:    stepDef.FunctionHash,
			ArgsJson:        argsJSON,
			IdempotencyKey:  state.WorkflowID + ":" + stepDef.StepID + ":" + inputHash + ":attempt:" + strconv.FormatUint(uint64(attempt), 10),
			WorkflowId:      state.WorkflowID,
			StepId:          stepDef.StepID,
			LlmModelName:    stepDef.LLMModelName,
			LlmModelVersion: stepDef.LLMModelVersion,
			LlmAdapter:      stepDef.LLMAdapter,
			LlmMaxTokens:    stepDef.LLMMaxTokens,
			TimeoutMs:       stepDef.TimeoutMs,
		}
		fingerprint, err := taskSpecFingerprint(spec)
		if err != nil {
			return err
		}
		created, _ := s.meta.CreateTask(metadata.Task{
			TaskID:                 spec.GetTaskId(),
			TaskName:               spec.GetTaskName(),
			Status:                 logservepb.TaskStatus_TASK_STATUS_QUEUED,
			WorkflowID:             spec.GetWorkflowId(),
			StepID:                 spec.GetStepId(),
			LLMModelName:           spec.GetLlmModelName(),
			LLMModelVersion:        spec.GetLlmModelVersion(),
			IdempotencyFingerprint: fingerprint,
		}, spec.GetIdempotencyKey())
		s.specMu.Lock()
		s.specs[step.TaskID] = cloneSpec(spec)
		s.specMu.Unlock()
		if s.useSchedulerV2() {
			s.scheduler.Enqueue(s.schedulerMetaFromTask(created))
		} else {
			s.queueMu.Lock()
			if !containsTaskID(s.queue, step.TaskID) {
				s.queue = append(s.queue, step.TaskID)
			}
			s.queueMu.Unlock()
		}
	}
	return nil
}

func (s *Service) bootstrapActors(ctx context.Context) error {
	streams, err := s.listStreams(ctx, "actor:")
	if err != nil {
		return err
	}
	for _, streamID := range streams {
		actorID := strings.TrimPrefix(streamID, "actor:")
		replayed, err := actor.ReplayRawEach(actorID, func(emit func(logrecord.RawRecord) error) error {
			return s.forEachRawLogRecord(ctx, streamID, 1, emit)
		}, s)
		if err != nil {
			continue
		}
		s.meta.UpsertActor(replayed.State)
	}
	return nil
}

func (s *Service) listStreams(ctx context.Context, prefix string) ([]string, error) {
	resp, err := s.log.ListStreams(ctx, &logservepb.ListStreamsRequest{Prefix: prefix})
	if err != nil {
		return nil, err
	}
	return resp.GetStreamIds(), nil
}

func containsTaskID(taskIDs []string, taskID string) bool {
	for _, existing := range taskIDs {
		if existing == taskID {
			return true
		}
	}
	return false
}

func (s *Service) LogBootstrapResult(err error) error {
	if err != nil {
		observability.Error("control_bootstrap_failed", err, nil)
		return err
	}
	observability.Info("control_bootstrap_completed", nil)
	return nil
}
