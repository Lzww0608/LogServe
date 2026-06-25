package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/eventcodec"
	"github.com/logserve/logserve/internal/logrecord"
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

type RecordIterator func(func(*logservepb.LogRecord) error) error
type RawRecordIterator func(func(logrecord.RawRecord) error) error

func MarshalEventPayload(payload EventPayload) ([]byte, error) {
	return eventcodec.Marshal(eventcodec.KindWorkflowEvent, eventPayloadMap(payload))
}

func UnmarshalEventPayload(data []byte, payload *EventPayload) error {
	if payload == nil {
		return errors.New("workflow event payload is nil")
	}
	var fields map[string]any
	encoded, err := eventcodec.Unmarshal(eventcodec.KindWorkflowEvent, data, &fields)
	if err != nil {
		return err
	}
	if encoded {
		*payload = eventPayloadFromMap(fields)
		return nil
	}
	return json.Unmarshal(data, payload)
}

func Replay(workflowID string, records []*logservepb.LogRecord) (State, error) {
	return ReplayEach(workflowID, func(emit func(*logservepb.LogRecord) error) error {
		for _, rec := range records {
			if err := emit(rec); err != nil {
				return err
			}
		}
		return nil
	})
}

func ReplayEach(workflowID string, iterate RecordIterator) (State, error) {
	if iterate == nil {
		return State{}, errors.New("record iterator is required")
	}
	return ReplayRawEach(workflowID, func(emit func(logrecord.RawRecord) error) error {
		return iterate(func(rec *logservepb.LogRecord) error {
			return emit(logrecord.FromProto(rec))
		})
	})
}

func ReplayRawEach(workflowID string, iterate RawRecordIterator) (State, error) {
	var state State
	if iterate == nil {
		return State{}, errors.New("record iterator is required")
	}
	if err := iterate(func(rec logrecord.RawRecord) error {
		var err error
		state, err = applyWorkflowRecord(state, workflowID, rec, false)
		return err
	}); err != nil {
		return State{}, err
	}
	if state.WorkflowID == "" {
		return State{}, errors.New("workflow start event not found")
	}
	state.RebuildRuntime()
	return state, nil
}

func ReplayFromEach(initial State, iterate RecordIterator) (State, error) {
	if iterate == nil {
		return CloneState(initial), nil
	}
	return ReplayFromRawEach(initial, func(emit func(logrecord.RawRecord) error) error {
		return iterate(func(rec *logservepb.LogRecord) error {
			return emit(logrecord.FromProto(rec))
		})
	})
}

func ReplayFromRawEach(initial State, iterate RawRecordIterator) (State, error) {
	state := CloneState(initial)
	if iterate != nil {
		if err := iterate(func(rec logrecord.RawRecord) error {
			var err error
			state, err = applyWorkflowRecord(state, initial.WorkflowID, rec, true)
			return err
		}); err != nil {
			return State{}, err
		}
	}
	if state.WorkflowID == "" {
		return State{}, errors.New("workflow start event not found")
	}
	state.RebuildRuntime()
	return state, nil
}

func applyWorkflowRecord(state State, fallbackWorkflowID string, rec logrecord.RawRecord, allowFallbackID bool) (State, error) {
	var payload EventPayload
	if len(rec.Payload) > 0 {
		if err := UnmarshalEventPayload(rec.Payload, &payload); err != nil {
			return state, err
		}
	}
	if payload.TimestampMs == 0 {
		payload.TimestampMs = rec.TimestampMs
	}

	switch rec.EventType {
	case "WorkflowStarted":
		def, err := ParseDefinition(payload.DefinitionJSON)
		if err != nil {
			return state, err
		}
		workflowID := fallbackWorkflowID
		if allowFallbackID && payload.WorkflowID != "" {
			workflowID = payload.WorkflowID
		}
		state = NewState(workflowID, def, payload.TimestampMs)
		state.IdempotencyKey = payload.IdempotencyKey
		state.IdempotencyFingerprint = payload.IdempotencyFingerprint
	case "StepScheduled":
		state.SetStepScheduled(payload.StepID, payload.TaskID, payload.Attempt, payload.InputHash, nil, payload.TimestampMs)
	case "StepStarted":
		state.SetStepStarted(payload.StepID, payload.TaskID, payload.TimestampMs)
	case "StepSucceeded":
		state.SetStepSucceeded(payload.StepID, payload.TaskID, payload.ResultJSON, payload.ResultRef, payload.TimestampMs, payload.LatencyMs)
	case "StepFailed":
		state.SetStepFailed(payload.StepID, payload.TaskID, payload.Error, false, payload.TimestampMs, payload.LatencyMs)
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
	return state, nil
}

func eventPayloadMap(payload EventPayload) map[string]any {
	fields := make(map[string]any, 12)
	if payload.WorkflowID != "" {
		fields["workflow_id"] = payload.WorkflowID
	}
	if payload.WorkflowName != "" {
		fields["workflow_name"] = payload.WorkflowName
	}
	if len(payload.DefinitionJSON) > 0 {
		fields["definition_json"] = []byte(payload.DefinitionJSON)
	}
	if payload.StepID != "" {
		fields["step_id"] = payload.StepID
	}
	if payload.TaskID != "" {
		fields["task_id"] = payload.TaskID
	}
	if payload.Attempt > 0 {
		fields["attempt"] = payload.Attempt
	}
	if payload.InputHash != "" {
		fields["input_hash"] = payload.InputHash
	}
	if len(payload.ResultJSON) > 0 {
		fields["result_json"] = []byte(payload.ResultJSON)
	}
	if payload.ResultRef != "" {
		fields["result_ref"] = payload.ResultRef
	}
	if payload.Error != "" {
		fields["error"] = payload.Error
	}
	if payload.IdempotencyKey != "" {
		fields["idempotency_key"] = payload.IdempotencyKey
	}
	if payload.IdempotencyFingerprint != "" {
		fields["idempotency_fingerprint"] = payload.IdempotencyFingerprint
	}
	if payload.TimestampMs != 0 {
		fields["timestamp_ms"] = payload.TimestampMs
	}
	if payload.LatencyMs != 0 {
		fields["latency_ms"] = payload.LatencyMs
	}
	return fields
}

func eventPayloadFromMap(fields map[string]any) EventPayload {
	return EventPayload{
		WorkflowID:             eventcodec.StringValue(fields["workflow_id"]),
		WorkflowName:           eventcodec.StringValue(fields["workflow_name"]),
		DefinitionJSON:         json.RawMessage(eventcodec.BytesValue(fields["definition_json"])),
		StepID:                 eventcodec.StringValue(fields["step_id"]),
		TaskID:                 eventcodec.StringValue(fields["task_id"]),
		Attempt:                eventcodec.Uint32Value(fields["attempt"]),
		InputHash:              eventcodec.StringValue(fields["input_hash"]),
		ResultJSON:             json.RawMessage(eventcodec.BytesValue(fields["result_json"])),
		ResultRef:              eventcodec.StringValue(fields["result_ref"]),
		Error:                  eventcodec.StringValue(fields["error"]),
		IdempotencyKey:         eventcodec.StringValue(fields["idempotency_key"]),
		IdempotencyFingerprint: eventcodec.StringValue(fields["idempotency_fingerprint"]),
		TimestampMs:            eventcodec.Int64Value(fields["timestamp_ms"]),
		LatencyMs:              eventcodec.Int64Value(fields["latency_ms"]),
	}
}

func Consistent(a, b State) bool {
	if a.WorkflowID != b.WorkflowID || a.Status != b.Status || a.Error != b.Error {
		return false
	}
	if !bytes.Equal(a.ResultJSON, b.ResultJSON) || a.ResultRef != b.ResultRef {
		return false
	}
	as := a.StepStatesInOrder()
	bs := b.StepStatesInOrder()
	if len(as) != len(bs) {
		return false
	}
	for i := range as {
		if as[i].StepID != bs[i].StepID || as[i].Status != bs[i].Status || as[i].Attempts != bs[i].Attempts || as[i].TaskID != bs[i].TaskID || as[i].ResultRef != bs[i].ResultRef || as[i].Error != bs[i].Error {
			return false
		}
		if !bytes.Equal(as[i].ResultJSON, bs[i].ResultJSON) {
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
