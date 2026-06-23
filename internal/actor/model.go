package actor

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
)

type State struct {
	ActorID                string
	ClassName              string
	ClassSource            string
	InitArgsJSON           []byte
	Status                 logservepb.ActorStatus
	OwnerWorkerID          string
	Epoch                  uint64
	CommandCount           uint64
	SubmittedCommandCount  uint64
	SnapshotEvery          uint32
	SnapshotRef            string
	SnapshotCommandCount   uint64
	StateJSON              []byte
	IdempotencyKey         string
	IdempotencyFingerprint string
	CreatedAtMs            int64
	UpdatedAtMs            int64
}

type EventPayload struct {
	ActorID                string          `json:"actor_id,omitempty"`
	ClassName              string          `json:"class_name,omitempty"`
	ClassSource            string          `json:"class_source,omitempty"`
	InitArgsJSON           json.RawMessage `json:"init_args_json,omitempty"`
	WorkerID               string          `json:"worker_id,omitempty"`
	Epoch                  uint64          `json:"epoch,omitempty"`
	CallID                 string          `json:"call_id,omitempty"`
	CommandSeq             uint64          `json:"command_seq,omitempty"`
	MethodName             string          `json:"method_name,omitempty"`
	ArgsJSON               json.RawMessage `json:"args_json,omitempty"`
	ResultJSON             json.RawMessage `json:"result_json,omitempty"`
	StateJSON              json.RawMessage `json:"state_json,omitempty"`
	SnapshotRef            string          `json:"snapshot_ref,omitempty"`
	SnapshotEvery          uint32          `json:"snapshot_every,omitempty"`
	CommandCount           uint64          `json:"command_count,omitempty"`
	SnapshotCommandCount   uint64          `json:"snapshot_command_count,omitempty"`
	Error                  string          `json:"error,omitempty"`
	IdempotencyKey         string          `json:"idempotency_key,omitempty"`
	IdempotencyFingerprint string          `json:"idempotency_fingerprint,omitempty"`
	TimestampMs            int64           `json:"timestamp_ms,omitempty"`
}

type ResultLoader interface {
	LoadResult(ref string) ([]byte, error)
}

var errActorCreateEventNotFound = errors.New("actor create event not found")

func NewState(actorID, className, classSource string, initArgsJSON []byte, snapshotEvery uint32, nowMs int64) State {
	if snapshotEvery == 0 {
		snapshotEvery = 25
	}
	return State{
		ActorID:       actorID,
		ClassName:     className,
		ClassSource:   classSource,
		InitArgsJSON:  append([]byte(nil), initArgsJSON...),
		Status:        logservepb.ActorStatus_ACTOR_STATUS_ACTIVE,
		SnapshotEvery: snapshotEvery,
		CreatedAtMs:   nowMs,
		UpdatedAtMs:   nowMs,
	}
}

type ReplayResult struct {
	State                  State
	FullReplayCommands     uint64
	SnapshotReplayCommands uint64
}

type RecordIterator func(func(*logservepb.LogRecord) error) error

func Replay(actorID string, records []*logservepb.LogRecord, loader ResultLoader) (ReplayResult, error) {
	return ReplayEach(actorID, func(emit func(*logservepb.LogRecord) error) error {
		for _, rec := range records {
			if err := emit(rec); err != nil {
				return err
			}
		}
		return nil
	}, loader)
}

func ReplayEach(actorID string, iterate RecordIterator, loader ResultLoader) (ReplayResult, error) {
	full, err := replayEach(actorID, iterate, loader, false)
	if err != nil {
		if !errors.Is(err, errActorCreateEventNotFound) {
			return ReplayResult{}, err
		}
		full = ReplayResult{}
	}
	withSnapshot, err := replayEach(actorID, iterate, loader, true)
	if err != nil {
		return ReplayResult{}, err
	}
	fullReplayCommands := full.FullReplayCommands
	if withSnapshot.State.SnapshotCommandCount > 0 && fullReplayCommands <= withSnapshot.SnapshotReplayCommands {
		fullReplayCommands = withSnapshot.State.CommandCount
	}
	return ReplayResult{
		State:                  withSnapshot.State,
		FullReplayCommands:     fullReplayCommands,
		SnapshotReplayCommands: withSnapshot.SnapshotReplayCommands,
	}, nil
}

func replayEach(actorID string, iterate RecordIterator, loader ResultLoader, useSnapshot bool) (ReplayResult, error) {
	if iterate == nil {
		return ReplayResult{}, errors.New("record iterator is required")
	}
	var state State
	var snapshotCommandCount uint64
	if useSnapshot {
		if err := iterate(func(rec *logservepb.LogRecord) error {
			if rec.GetEventType() != "ActorSnapshotCreated" {
				return nil
			}
			var payload EventPayload
			if err := json.Unmarshal(rec.GetPayload(), &payload); err != nil {
				return err
			}
			if payload.SnapshotRef == "" {
				return nil
			}
			if loader == nil {
				return errors.New("snapshot loader is required")
			}
			data, err := loader.LoadResult(payload.SnapshotRef)
			if err != nil {
				return err
			}
			state.ActorID = actorID
			state.ClassName = payload.ClassName
			state.ClassSource = payload.ClassSource
			state.InitArgsJSON = append([]byte(nil), payload.InitArgsJSON...)
			state.Status = logservepb.ActorStatus_ACTOR_STATUS_ACTIVE
			state.OwnerWorkerID = payload.WorkerID
			state.Epoch = payload.Epoch
			state.StateJSON = NormalizeJSON(data)
			state.SnapshotRef = payload.SnapshotRef
			state.SnapshotCommandCount = payload.SnapshotCommandCount
			state.CommandCount = payload.SnapshotCommandCount
			state.SubmittedCommandCount = payload.SnapshotCommandCount
			state.SnapshotEvery = payload.SnapshotEvery
			state.UpdatedAtMs = payload.TimestampMs
			snapshotCommandCount = payload.SnapshotCommandCount
			return nil
		}); err != nil {
			return ReplayResult{}, err
		}
	}

	var commands uint64
	if err := iterate(func(rec *logservepb.LogRecord) error {
		var payload EventPayload
		if len(rec.GetPayload()) > 0 {
			if err := json.Unmarshal(rec.GetPayload(), &payload); err != nil {
				return err
			}
		}
		if payload.TimestampMs == 0 {
			payload.TimestampMs = rec.GetTimestampMs()
		}

		switch rec.GetEventType() {
		case "ActorCreated":
			if state.ActorID == "" {
				state = NewState(actorID, payload.ClassName, payload.ClassSource, payload.InitArgsJSON, payload.SnapshotEvery, payload.TimestampMs)
			} else {
				state.ActorID = actorID
				state.ClassName = payload.ClassName
				state.ClassSource = payload.ClassSource
				state.InitArgsJSON = append([]byte(nil), payload.InitArgsJSON...)
				state.Status = logservepb.ActorStatus_ACTOR_STATUS_ACTIVE
				state.SnapshotEvery = payload.SnapshotEvery
				if state.CreatedAtMs == 0 {
					state.CreatedAtMs = payload.TimestampMs
				}
			}
			state.IdempotencyKey = payload.IdempotencyKey
			state.IdempotencyFingerprint = payload.IdempotencyFingerprint
		case "ActorOwnershipGranted":
			state.OwnerWorkerID = payload.WorkerID
			state.Epoch = payload.Epoch
			state.UpdatedAtMs = payload.TimestampMs
		case "ActorCommandSubmitted":
			if payload.CommandSeq > state.SubmittedCommandCount {
				state.SubmittedCommandCount = payload.CommandSeq
			}
			state.UpdatedAtMs = payload.TimestampMs
		case "ActorCommandApplied":
			if useSnapshot && payload.CommandCount <= snapshotCommandCount {
				return nil
			}
			if payload.CommandSeq > 0 {
				state.CommandCount = payload.CommandSeq
			} else {
				state.CommandCount = payload.CommandCount
			}
			if state.CommandCount > state.SubmittedCommandCount {
				state.SubmittedCommandCount = state.CommandCount
			}
			if payload.WorkerID != "" {
				state.OwnerWorkerID = payload.WorkerID
			}
			if payload.Epoch != 0 {
				state.Epoch = payload.Epoch
			}
			state.StateJSON = NormalizeJSON(payload.StateJSON)
			state.UpdatedAtMs = payload.TimestampMs
			commands++
		case "ActorCommandFailed":
			if useSnapshot && payload.CommandCount <= snapshotCommandCount {
				return nil
			}
			if payload.CommandSeq > 0 {
				state.CommandCount = payload.CommandSeq
			} else {
				state.CommandCount = payload.CommandCount
			}
			if state.CommandCount > state.SubmittedCommandCount {
				state.SubmittedCommandCount = state.CommandCount
			}
			if payload.WorkerID != "" {
				state.OwnerWorkerID = payload.WorkerID
			}
			if payload.Epoch != 0 {
				state.Epoch = payload.Epoch
			}
			state.UpdatedAtMs = payload.TimestampMs
			commands++
		case "ActorSnapshotCreated":
			state.ActorID = actorID
			if payload.ClassName != "" {
				state.ClassName = payload.ClassName
			}
			if payload.ClassSource != "" {
				state.ClassSource = payload.ClassSource
			}
			if len(payload.InitArgsJSON) > 0 {
				state.InitArgsJSON = append([]byte(nil), payload.InitArgsJSON...)
			}
			if payload.SnapshotEvery > 0 {
				state.SnapshotEvery = payload.SnapshotEvery
			}
			state.Status = logservepb.ActorStatus_ACTOR_STATUS_ACTIVE
			if payload.WorkerID != "" {
				state.OwnerWorkerID = payload.WorkerID
			}
			if payload.Epoch != 0 {
				state.Epoch = payload.Epoch
			}
			state.SnapshotRef = payload.SnapshotRef
			state.SnapshotCommandCount = payload.SnapshotCommandCount
			state.UpdatedAtMs = payload.TimestampMs
			if useSnapshot && payload.SnapshotRef != "" {
				if loader == nil {
					return errors.New("snapshot loader is required")
				}
				data, err := loader.LoadResult(payload.SnapshotRef)
				if err != nil {
					return err
				}
				state.StateJSON = NormalizeJSON(data)
			}
		}
		return nil
	}); err != nil {
		return ReplayResult{}, err
	}
	if state.ActorID == "" {
		return ReplayResult{}, errActorCreateEventNotFound
	}
	if useSnapshot {
		return ReplayResult{State: state, SnapshotReplayCommands: commands}, nil
	}
	return ReplayResult{State: state, FullReplayCommands: commands}, nil
}

func ReplayFromStateEach(actorID string, initial State, iterate RecordIterator, loader ResultLoader) (ReplayResult, error) {
	state := cloneStateForReplay(initial)
	if state.ActorID == "" {
		state.ActorID = actorID
	}
	var commands uint64
	if iterate != nil {
		if err := iterate(func(rec *logservepb.LogRecord) error {
			var payload EventPayload
			if len(rec.GetPayload()) > 0 {
				if err := json.Unmarshal(rec.GetPayload(), &payload); err != nil {
					return err
				}
			}
			if payload.TimestampMs == 0 {
				payload.TimestampMs = rec.GetTimestampMs()
			}

			switch rec.GetEventType() {
			case "ActorCreated":
				if state.ActorID == "" {
					state = NewState(actorID, payload.ClassName, payload.ClassSource, payload.InitArgsJSON, payload.SnapshotEvery, payload.TimestampMs)
				} else {
					state.ActorID = actorID
					state.ClassName = payload.ClassName
					state.ClassSource = payload.ClassSource
					state.InitArgsJSON = append([]byte(nil), payload.InitArgsJSON...)
					state.Status = logservepb.ActorStatus_ACTOR_STATUS_ACTIVE
					state.SnapshotEvery = payload.SnapshotEvery
					if state.CreatedAtMs == 0 {
						state.CreatedAtMs = payload.TimestampMs
					}
				}
				state.IdempotencyKey = payload.IdempotencyKey
				state.IdempotencyFingerprint = payload.IdempotencyFingerprint
			case "ActorOwnershipGranted":
				state.OwnerWorkerID = payload.WorkerID
				state.Epoch = payload.Epoch
				state.UpdatedAtMs = payload.TimestampMs
			case "ActorCommandSubmitted":
				if payload.CommandSeq > state.SubmittedCommandCount {
					state.SubmittedCommandCount = payload.CommandSeq
				}
				state.UpdatedAtMs = payload.TimestampMs
			case "ActorCommandApplied":
				if payload.CommandSeq > 0 {
					state.CommandCount = payload.CommandSeq
				} else {
					state.CommandCount = payload.CommandCount
				}
				if state.CommandCount > state.SubmittedCommandCount {
					state.SubmittedCommandCount = state.CommandCount
				}
				if payload.WorkerID != "" {
					state.OwnerWorkerID = payload.WorkerID
				}
				if payload.Epoch != 0 {
					state.Epoch = payload.Epoch
				}
				state.StateJSON = NormalizeJSON(payload.StateJSON)
				state.UpdatedAtMs = payload.TimestampMs
				commands++
			case "ActorCommandFailed":
				if payload.CommandSeq > 0 {
					state.CommandCount = payload.CommandSeq
				} else {
					state.CommandCount = payload.CommandCount
				}
				if state.CommandCount > state.SubmittedCommandCount {
					state.SubmittedCommandCount = state.CommandCount
				}
				if payload.WorkerID != "" {
					state.OwnerWorkerID = payload.WorkerID
				}
				if payload.Epoch != 0 {
					state.Epoch = payload.Epoch
				}
				state.UpdatedAtMs = payload.TimestampMs
				commands++
			case "ActorSnapshotCreated":
				state.ActorID = actorID
				if payload.ClassName != "" {
					state.ClassName = payload.ClassName
				}
				if payload.ClassSource != "" {
					state.ClassSource = payload.ClassSource
				}
				if len(payload.InitArgsJSON) > 0 {
					state.InitArgsJSON = append([]byte(nil), payload.InitArgsJSON...)
				}
				if payload.SnapshotEvery > 0 {
					state.SnapshotEvery = payload.SnapshotEvery
				}
				state.Status = logservepb.ActorStatus_ACTOR_STATUS_ACTIVE
				if payload.WorkerID != "" {
					state.OwnerWorkerID = payload.WorkerID
				}
				if payload.Epoch != 0 {
					state.Epoch = payload.Epoch
				}
				state.SnapshotRef = payload.SnapshotRef
				state.SnapshotCommandCount = payload.SnapshotCommandCount
				state.UpdatedAtMs = payload.TimestampMs
				if payload.SnapshotRef != "" {
					if loader == nil {
						return errors.New("snapshot loader is required")
					}
					data, err := loader.LoadResult(payload.SnapshotRef)
					if err != nil {
						return err
					}
					state.StateJSON = NormalizeJSON(data)
				}
			}
			return nil
		}); err != nil {
			return ReplayResult{}, err
		}
	}
	if state.ActorID == "" {
		return ReplayResult{}, errActorCreateEventNotFound
	}
	return ReplayResult{State: state, SnapshotReplayCommands: commands}, nil
}

func cloneStateForReplay(state State) State {
	state.ClassSource = string([]byte(state.ClassSource))
	state.InitArgsJSON = append([]byte(nil), state.InitArgsJSON...)
	state.StateJSON = append([]byte(nil), state.StateJSON...)
	return state
}
func Consistent(a, b State) bool {
	return a.ActorID == b.ActorID &&
		a.ClassName == b.ClassName &&
		a.Status == b.Status &&
		a.OwnerWorkerID == b.OwnerWorkerID &&
		a.Epoch == b.Epoch &&
		a.CommandCount == b.CommandCount &&
		a.SubmittedCommandCount == b.SubmittedCommandCount &&
		a.SnapshotRef == b.SnapshotRef &&
		a.SnapshotCommandCount == b.SnapshotCommandCount &&
		bytes.Equal(NormalizeJSON(a.StateJSON), NormalizeJSON(b.StateJSON))
}

func NowMs() int64 {
	return time.Now().UnixMilli()
}

func NormalizeJSON(data []byte) []byte {
	if len(data) == 0 {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, data); err != nil {
		return append([]byte(nil), data...)
	}
	return buf.Bytes()
}
