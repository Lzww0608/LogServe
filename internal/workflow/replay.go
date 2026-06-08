package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
)

type EventPayload struct {
	WorkflowID             string          `json:"workflow_id,omitempty"`
	WorkflowName           string          `json:"workflow_name,omitempty"`
	DefinitionJSON         json.RawMessage `json:"definition_json,omitempty"`
	StepID                 string          `json:"step_id,omitempty"`
	TaskID                 string          `json:"task_id,omitempty"`
	Attempt                uint32          `json:"attempt,omitempty"`
	InputHash              string          `json:"input_hash,omitempty"`
	ResultJSON             json.RawMessage `json:"result_json,omitempty"`
	ResultRef              string          `json:"result_ref,omitempty"`
	Error                  string          `json:"error,omitempty"`
	IdempotencyKey         string          `json:"idempotency_key,omitempty"`
	IdempotencyFingerprint string          `json:"idempotency_fingerprint,omitempty"`
	TimestampMs            int64           `json:"timestamp_ms,omitempty"`
	LatencyMs              int64           `json:"latency_ms,omitempty"`
}

func Replay(workflowID string, records []*logservepb.LogRecord) (State, error) {
	var state State
	for _, rec := range records {
		var payload EventPayload
		if len(rec.GetPayload()) > 0 {
			if err := json.Unmarshal(rec.GetPayload(), &payload); err != nil {
				return State{}, err
			}
		}
		if payload.TimestampMs == 0 {
			payload.TimestampMs = rec.GetTimestampMs()
		}

		switch rec.GetEventType() {
		case "WorkflowStarted":
			def, err := ParseDefinition(payload.DefinitionJSON)
			if err != nil {
				return State{}, err
			}
			state = NewState(workflowID, def, payload.TimestampMs)
			state.IdempotencyKey = payload.IdempotencyKey
			state.IdempotencyFingerprint = payload.IdempotencyFingerprint
		case "StepScheduled":
			step := state.Steps[payload.StepID]
			step.Status = logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SCHEDULED
			step.TaskID = payload.TaskID
			step.Attempts = payload.Attempt
			step.LastInputHash = payload.InputHash
			step.LastScheduledAtMs = payload.TimestampMs
			step.Error = ""
			state.Steps[payload.StepID] = step
			state.UpdatedAtMs = payload.TimestampMs
		case "StepStarted":
			step := state.Steps[payload.StepID]
			step.Status = logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_STARTED
			step.TaskID = payload.TaskID
			step.StartedAtMs = payload.TimestampMs
			state.Steps[payload.StepID] = step
			state.UpdatedAtMs = payload.TimestampMs
		case "StepSucceeded":
			step := state.Steps[payload.StepID]
			step.Status = logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SUCCEEDED
			step.TaskID = payload.TaskID
			step.ResultJSON = append([]byte(nil), payload.ResultJSON...)
			step.ResultRef = payload.ResultRef
			step.Error = ""
			step.CompletedAtMs = payload.TimestampMs
			step.LatencyMs = payload.LatencyMs
			state.Steps[payload.StepID] = step
			state.UpdatedAtMs = payload.TimestampMs
		case "StepFailed":
			step := state.Steps[payload.StepID]
			step.Status = logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_FAILED
			step.TaskID = payload.TaskID
			step.Error = payload.Error
			step.CompletedAtMs = payload.TimestampMs
			step.LatencyMs = payload.LatencyMs
			state.Steps[payload.StepID] = step
			state.UpdatedAtMs = payload.TimestampMs
		case "WorkflowCompleted":
			state.Status = logservepb.WorkflowStatus_WORKFLOW_STATUS_COMPLETED
			state.ResultJSON = append([]byte(nil), payload.ResultJSON...)
			state.ResultRef = payload.ResultRef
			state.CompletedAtMs = payload.TimestampMs
			state.UpdatedAtMs = payload.TimestampMs
		case "WorkflowFailed":
			state.Status = logservepb.WorkflowStatus_WORKFLOW_STATUS_FAILED
			state.Error = payload.Error
			state.CompletedAtMs = payload.TimestampMs
			state.UpdatedAtMs = payload.TimestampMs
		}
	}
	if state.WorkflowID == "" {
		return State{}, errors.New("workflow start event not found")
	}
	return state, nil
}

func Consistent(a, b State) bool {
	if a.WorkflowID != b.WorkflowID || a.Status != b.Status || a.Error != b.Error {
		return false
	}
	if !bytes.Equal(a.ResultJSON, b.ResultJSON) || a.ResultRef != b.ResultRef {
		return false
	}
	if len(a.StepOrder) != len(b.StepOrder) {
		return false
	}
	for _, stepID := range a.StepOrder {
		as := a.Steps[stepID]
		bs := b.Steps[stepID]
		if as.Status != bs.Status || as.Attempts != bs.Attempts || as.TaskID != bs.TaskID || as.ResultRef != bs.ResultRef || as.Error != bs.Error {
			return false
		}
		if !bytes.Equal(as.ResultJSON, bs.ResultJSON) {
			return false
		}
	}
	return true
}

func WorkflowLatencyMs(state State) int64 {
	if state.CompletedAtMs == 0 || state.CreatedAtMs == 0 {
		return 0
	}
	return state.CompletedAtMs - state.CreatedAtMs
}

func NowMs() int64 {
	return time.Now().UnixMilli()
}
