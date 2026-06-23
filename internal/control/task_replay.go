package control

import (
	"encoding/json"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/metadata"
	"google.golang.org/protobuf/encoding/protojson"
)

type taskReplayState struct {
	spec                  *logservepb.TaskSpec
	task                  metadata.Task
	status                logservepb.TaskStatus
	leaseEpoch            uint64
	redeliveredLeaseEpoch uint64
	ok                    bool
}

func replayTaskMetadataEach(iterate func(func(*logservepb.LogRecord) error) error, initial *taskReplayState) (*taskReplayState, error) {
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
	if err := iterate(func(rec *logservepb.LogRecord) error {
		return state.apply(rec)
	}); err != nil {
		return nil, err
	}
	return state, nil
}

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

func (s *taskReplayState) apply(rec *logservepb.LogRecord) error {
	switch rec.GetEventType() {
	case "TaskSubmitted":
		var payload taskSubmittedPayload
		if err := json.Unmarshal(rec.GetPayload(), &payload); err != nil {
			return err
		}
		if len(payload.TaskSpec) == 0 {
			return nil
		}
		decoded := &logservepb.TaskSpec{}
		if err := protojson.Unmarshal(payload.TaskSpec, decoded); err != nil {
			return err
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
		payload := decodeTaskLifecyclePayload(rec.GetPayload())
		if payload.TaskLeaseEpoch == 0 {
			if s.leaseEpoch == 0 && s.redeliveredLeaseEpoch == 0 {
				s.status = logservepb.TaskStatus_TASK_STATUS_RUNNING
				s.task.Status = s.status
				s.task.WorkerID = payload.WorkerID
			}
			return nil
		}
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
		payload := decodeTaskLifecyclePayload(rec.GetPayload())
		if payload.TaskLeaseEpoch > s.redeliveredLeaseEpoch {
			s.redeliveredLeaseEpoch = payload.TaskLeaseEpoch
		}
		if payload.TaskLeaseEpoch == 0 || payload.TaskLeaseEpoch >= s.leaseEpoch {
			s.status = logservepb.TaskStatus_TASK_STATUS_QUEUED
			s.task.Status = s.status
			s.task.WorkerID = ""
		}
	case "TaskCompleted":
		if s.terminalEventApplies(rec.GetPayload()) {
			payload := decodeTaskLifecyclePayload(rec.GetPayload())
			s.status = logservepb.TaskStatus_TASK_STATUS_SUCCEEDED
			s.task.Status = s.status
			s.task.WorkerID = payload.WorkerID
			s.task.ResultJSON = append([]byte(nil), payload.ResultJSON...)
			s.task.Error = ""
		}
	case "TaskFailed":
		if s.terminalEventApplies(rec.GetPayload()) {
			payload := decodeTaskLifecyclePayload(rec.GetPayload())
			s.status = logservepb.TaskStatus_TASK_STATUS_FAILED
			s.task.Status = s.status
			s.task.WorkerID = payload.WorkerID
			s.task.ResultJSON = append([]byte(nil), payload.ResultJSON...)
			s.task.Error = payload.Error
		}
	}
	return nil
}

func (s *taskReplayState) terminalEventApplies(payload []byte) bool {
	return taskTerminalEventApplies(s.status, s.leaseEpoch, s.redeliveredLeaseEpoch, payload)
}

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

func decodeTaskLifecyclePayload(data []byte) taskLifecyclePayload {
	var payload taskLifecyclePayload
	if len(data) > 0 {
		_ = json.Unmarshal(data, &payload)
	}
	return payload
}
