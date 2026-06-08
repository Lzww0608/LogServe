package control

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/actor"
	"github.com/logserve/logserve/internal/metadata"
	"github.com/logserve/logserve/internal/observability"
	"github.com/logserve/logserve/internal/workflow"
)

const bootstrapReadLimit = 1000

func (s *Service) BootstrapFromLog(ctx context.Context) error {
	if err := s.bootstrapModels(ctx); err != nil {
		return err
	}
	if err := s.bootstrapScheduler(ctx); err != nil {
		return err
	}
	if err := s.bootstrapBackpressure(ctx); err != nil {
		return err
	}
	if err := s.bootstrapWorkflows(ctx); err != nil {
		return err
	}
	if err := s.bootstrapActors(ctx); err != nil {
		return err
	}
	return nil
}

func (s *Service) bootstrapModels(ctx context.Context) error {
	records, err := s.readAllLog(ctx, "system:models")
	if err != nil {
		return err
	}
	for _, rec := range records {
		if rec.GetEventType() != "ModelRegistered" {
			continue
		}
		var model logservepb.ModelInfo
		if err := json.Unmarshal(rec.GetPayload(), &model); err != nil {
			return err
		}
		s.meta.RegisterModel(&model)
	}
	return nil
}

func (s *Service) bootstrapScheduler(ctx context.Context) error {
	records, err := s.readAllLog(ctx, "system:scheduler")
	if err != nil {
		return err
	}
	var policy logservepb.SchedulingPolicy
	for _, rec := range records {
		if rec.GetEventType() != "SchedulingPolicyChanged" {
			continue
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
	}
	if policy != logservepb.SchedulingPolicy_SCHEDULING_POLICY_UNSPECIFIED {
		s.configMu.Lock()
		s.schedulingPolicy = policy
		s.configMu.Unlock()
	}
	return nil
}

func (s *Service) bootstrapBackpressure(ctx context.Context) error {
	records, err := s.readAllLog(ctx, "system:backpressure")
	if err != nil {
		return err
	}
	var payload struct {
		QueueHighWatermark uint32 `json:"queue_high_watermark"`
		RedeliveryTimeout  int64  `json:"redelivery_timeout_ms"`
		LogAppendSlow      int64  `json:"log_append_slow_ms"`
	}
	for _, rec := range records {
		if rec.GetEventType() != "BackpressureConfigured" {
			continue
		}
		if err := json.Unmarshal(rec.GetPayload(), &payload); err != nil {
			return err
		}
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
	streams, err := s.listStreams(ctx, "wf:")
	if err != nil {
		return err
	}
	for _, streamID := range streams {
		workflowID := strings.TrimPrefix(streamID, "wf:")
		records, err := s.readAllLog(ctx, streamID)
		if err != nil {
			return err
		}
		state, err := workflow.Replay(workflowID, records)
		if err != nil {
			continue
		}
		s.prepareRetryableFailedSteps(&state)
		s.meta.UpsertWorkflow(state)
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
	for stepID, step := range state.Steps {
		if step.Status != logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_FAILED {
			continue
		}
		if int(step.Attempts) >= workflow.StepMaxAttempts(state.Definition, stepID) {
			continue
		}
		step.Status = logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SCHEDULED
		step.TaskID = ""
		state.Steps[stepID] = step
	}
}

func (s *Service) restoreWorkflowTasks(state workflow.State) error {
	if state.Status != logservepb.WorkflowStatus_WORKFLOW_STATUS_RUNNING {
		return nil
	}
	for _, stepDef := range state.Definition.Steps {
		step := state.Steps[stepDef.StepID]
		if step.TaskID == "" {
			continue
		}
		if step.Status != logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SCHEDULED &&
			step.Status != logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_STARTED {
			continue
		}
		argsJSON, inputHash, err := workflow.ResolveArgs(stepDef, state, s)
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
		s.meta.CreateTask(metadata.Task{
			TaskID:          spec.GetTaskId(),
			TaskName:        spec.GetTaskName(),
			Status:          logservepb.TaskStatus_TASK_STATUS_QUEUED,
			WorkflowID:      spec.GetWorkflowId(),
			StepID:          spec.GetStepId(),
			LLMModelName:    spec.GetLlmModelName(),
			LLMModelVersion: spec.GetLlmModelVersion(),
		}, spec.GetIdempotencyKey())
		s.specMu.Lock()
		s.specs[step.TaskID] = cloneSpec(spec)
		s.specMu.Unlock()
		s.queueMu.Lock()
		if !containsTaskID(s.queue, step.TaskID) {
			s.queue = append(s.queue, step.TaskID)
		}
		s.queueMu.Unlock()
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
		records, err := s.readAllLog(ctx, streamID)
		if err != nil {
			return err
		}
		replayed, err := actor.Replay(actorID, records, s)
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

func (s *Service) readAllLog(ctx context.Context, streamID string) ([]*logservepb.LogRecord, error) {
	fromSeq := uint64(1)
	out := make([]*logservepb.LogRecord, 0)
	for {
		resp, err := s.log.ReadLog(ctx, &logservepb.ReadLogRequest{
			StreamId: streamID,
			FromSeq:  fromSeq,
			Limit:    bootstrapReadLimit,
		})
		if err != nil {
			return nil, err
		}
		records := resp.GetRecords()
		if len(records) == 0 {
			return out, nil
		}
		out = append(out, records...)
		fromSeq = records[len(records)-1].GetSeq() + 1
		if len(records) < bootstrapReadLimit {
			return out, nil
		}
	}
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
