package control

import (
	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/logrecord"
	"github.com/logserve/logserve/internal/metadata"
)

// This file defines the task lifecycle replay state machine used by bootstrap and
// metadata checkpoint tail replay.
type taskReplayState struct {
	spec                  *logservepb.TaskSpec
	task                  metadata.Task
	status                logservepb.TaskStatus
	leaseEpoch            uint64
	redeliveredLeaseEpoch uint64
	ok                    bool
}

// replayTaskMetadataEach adapts protobuf log records into raw task replay state.
func replayTaskMetadataEach(iterate func(func(*logservepb.LogRecord) error) error, initial *taskReplayState) (*taskReplayState, error) {
	if iterate == nil {
		return replayTaskMetadataRawEach(nil, initial)
	}
	return replayTaskMetadataRawEach(func(emit func(logrecord.RawRecord) error) error {
		return iterate(func(rec *logservepb.LogRecord) error {
			return emit(logrecord.FromProto(rec))
		})
	}, initial)
}

// replayTaskMetadataRawEach replays raw task records from an optional checkpointed
// initial state.
func replayTaskMetadataRawEach(iterate func(func(logrecord.RawRecord) error) error, initial *taskReplayState) (*taskReplayState, error) {
	state := &taskReplayState{status: logservepb.TaskStatus_TASK_STATUS_QUEUED}
	if initial != nil {
		clone := *initial
		clone.spec = cloneSpec(initial.spec)
		clone.task.ResultJSON = append([]byte(nil), initial.task.ResultJSON...)
		state = &clone
	}
	if iterate == nil {
		return state, nil
	}
	if err := iterate(func(rec logrecord.RawRecord) error {
		return state.applyRaw(rec)
	}); err != nil {
		return nil, err
	}
	return state, nil
}

// taskReplayStateFromCheckpoint seeds replay with checkpointed task metadata and spec.
func taskReplayStateFromCheckpoint(task metadata.Task, spec *logservepb.TaskSpec) *taskReplayState {
	if task.Status == logservepb.TaskStatus_TASK_STATUS_UNSPECIFIED {
		task.Status = logservepb.TaskStatus_TASK_STATUS_QUEUED
	}
	return &taskReplayState{
		spec:       cloneSpec(spec),
		task:       task,
		status:     task.Status,
		leaseEpoch: task.TaskLeaseEpoch,
		ok:         task.TaskID != "",
	}
}

// apply converts a protobuf record into raw form before applying it to replay state.
func (s *taskReplayState) apply(rec *logservepb.LogRecord) error {
	return s.applyRaw(logrecord.FromProto(rec))
}

// applyRaw folds one task lifecycle record into replay state while respecting lease
// epochs and redelivery fences.
func (s *taskReplayState) applyRaw(rec logrecord.RawRecord) error {
	switch rec.EventType {
	case "TaskSubmitted":
		decoded, err := unmarshalTaskSubmittedSpec(rec.Payload)
		if err != nil {
			return err
		}
		if decoded.GetTaskId() == "" {
			return nil
		}
		fingerprint, err := taskSpecFingerprint(decoded)
		if err != nil {
			return err
		}
		s.spec = decoded
		s.status = logservepb.TaskStatus_TASK_STATUS_QUEUED
		s.leaseEpoch = decoded.GetTaskLeaseEpoch()
		s.redeliveredLeaseEpoch = 0
		s.task = metadata.Task{
			TaskID:                 decoded.GetTaskId(),
			TaskName:               decoded.GetTaskName(),
			Status:                 s.status,
			WorkflowID:             decoded.GetWorkflowId(),
			StepID:                 decoded.GetStepId(),
			TargetWorkerID:         decoded.GetTargetWorkerId(),
			ActorID:                decoded.GetActorId(),
			ActorCallID:            decoded.GetActorCallId(),
			ActorEpoch:             decoded.GetActorEpoch(),
			TaskLeaseEpoch:         s.leaseEpoch,
			LLMModelName:           decoded.GetLlmModelName(),
			LLMModelVersion:        decoded.GetLlmModelVersion(),
			IdempotencyKey:         decoded.GetIdempotencyKey(),
			IdempotencyFingerprint: fingerprint,
		}
		s.ok = true
	case "TaskStarted":
		if isTerminalTaskStatus(s.status) {
			return nil
		}
		payload := decodeTaskLifecyclePayload(rec.Payload)
		if payload.TaskLeaseEpoch == 0 {
			if s.leaseEpoch == 0 && s.redeliveredLeaseEpoch == 0 {
				s.status = logservepb.TaskStatus_TASK_STATUS_RUNNING
				s.task.Status = s.status
				s.task.WorkerID = payload.WorkerID
			}
			return nil
		}
		// A start from an epoch that has already been redelivered is stale and must not
		// move the task back to running during replay.
		if payload.TaskLeaseEpoch <= s.redeliveredLeaseEpoch {
			return nil
		}
		if payload.TaskLeaseEpoch >= s.leaseEpoch {
			s.leaseEpoch = payload.TaskLeaseEpoch
			s.status = logservepb.TaskStatus_TASK_STATUS_RUNNING
			s.task.Status = s.status
			s.task.WorkerID = payload.WorkerID
			s.task.TaskLeaseEpoch = s.leaseEpoch
		}
	case "TaskRedelivered":
		if isTerminalTaskStatus(s.status) {
			return nil
		}
		payload := decodeTaskLifecyclePayload(rec.Payload)
		if payload.TaskLeaseEpoch > s.redeliveredLeaseEpoch {
			s.redeliveredLeaseEpoch = payload.TaskLeaseEpoch
		}
		if payload.TaskLeaseEpoch == 0 || payload.TaskLeaseEpoch >= s.leaseEpoch {
			s.status = logservepb.TaskStatus_TASK_STATUS_QUEUED
			s.task.Status = s.status
			s.task.WorkerID = ""
		}
	case "TaskCompleted":
		if s.terminalEventApplies(rec.Payload) {
			payload := decodeTaskLifecyclePayload(rec.Payload)
			s.status = logservepb.TaskStatus_TASK_STATUS_SUCCEEDED
			s.task.Status = s.status
			s.task.WorkerID = payload.WorkerID
			s.task.ResultJSON = append([]byte(nil), payload.ResultJSON...)
			s.task.Error = ""
		}
	case "TaskFailed":
		if s.terminalEventApplies(rec.Payload) {
			payload := decodeTaskLifecyclePayload(rec.Payload)
			s.status = logservepb.TaskStatus_TASK_STATUS_FAILED
			s.task.Status = s.status
			s.task.WorkerID = payload.WorkerID
			s.task.ResultJSON = append([]byte(nil), payload.ResultJSON...)
			s.task.Error = payload.Error
		}
	}
	return nil
}

// terminalEventApplies delegates lease-epoch fencing for completion/failure records.
func (s *taskReplayState) terminalEventApplies(payload []byte) bool {
	return taskTerminalEventApplies(s.status, s.leaseEpoch, s.redeliveredLeaseEpoch, payload)
}

// finalTask converts replay state into metadata and requeues in-flight running work
// because no worker lease can be trusted across control restart.
func (s *taskReplayState) finalTask() metadata.Task {
	task := s.task
	if s.spec != nil {
		s.spec.TaskLeaseEpoch = s.leaseEpoch
	}
	task.TaskLeaseEpoch = s.leaseEpoch
	if s.status == logservepb.TaskStatus_TASK_STATUS_RUNNING {
		task.Status = logservepb.TaskStatus_TASK_STATUS_QUEUED
		task.WorkerID = ""
	} else {
		task.Status = s.status
	}
	return task
}
