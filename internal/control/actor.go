package control

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/actor"
	"github.com/logserve/logserve/internal/logrecord"
	"github.com/logserve/logserve/internal/metadata"
	"github.com/logserve/logserve/internal/objectstore"
	"github.com/logserve/logserve/internal/observability"
)

// This file implements actor creation, serialized actor method calls, ownership
// fencing, snapshots, and replay-backed status checks for the control service.
var errNoActiveActorWorker = errors.New("no active worker available for actor")

// CreateActor validates actor class input, records ActorCreated, and materializes
// actor metadata with idempotency fingerprint checks.
func (s *Service) CreateActor(ctx context.Context, req *logservepb.CreateActorRequest) (*logservepb.CreateActorResponse, error) {
	if req.GetClassName() == "" {
		return nil, errors.New("class_name is required")
	}
	if req.GetClassSource() == "" {
		return nil, errors.New("class_source is required")
	}
	initArgs := defaultJSON(req.GetInitArgsJson(), []byte(`{"args":[],"kwargs":{}}`))
	snapshotEvery := req.GetSnapshotEvery()
	if snapshotEvery == 0 {
		snapshotEvery = 25
	}
	fingerprint, err := actorCreateFingerprint(req, initArgs, snapshotEvery)
	if err != nil {
		return nil, err
	}
	if existing, ok := s.meta.GetActorByIdempotencyKey(req.GetIdempotencyKey()); ok {
		if err := ensureIdempotencyFingerprint("actor", req.GetIdempotencyKey(), existing.IdempotencyFingerprint, fingerprint); err != nil {
			return nil, err
		}
		return actorCreateResponse(existing), nil
	}
	actorID := newActorID()
	now := actor.NowMs()
	state := actor.NewState(actorID, req.GetClassName(), req.GetClassSource(), initArgs, snapshotEvery, now)
	state.IdempotencyKey = req.GetIdempotencyKey()
	state.IdempotencyFingerprint = fingerprint

	payload, _ := actor.MarshalEventPayload(actor.EventPayload{
		ActorID:                actorID,
		ClassName:              req.GetClassName(),
		ClassSource:            req.GetClassSource(),
		InitArgsJSON:           initArgs,
		SnapshotEvery:          state.SnapshotEvery,
		IdempotencyKey:         req.GetIdempotencyKey(),
		IdempotencyFingerprint: fingerprint,
		TimestampMs:            now,
	})
	if _, err := s.appendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       actorStream(actorID),
		EventType:      "ActorCreated",
		IdempotencyKey: actorID + ":created",
		Payload:        payload,
	}); err != nil {
		return nil, err
	}
	created, duplicate := s.meta.CreateActor(state, req.GetIdempotencyKey())
	if !duplicate {
		if err := s.metadataPersisted(); err != nil {
			return nil, err
		}
	}
	if duplicate {
		if err := ensureIdempotencyFingerprint("actor", req.GetIdempotencyKey(), created.IdempotencyFingerprint, fingerprint); err != nil {
			return nil, err
		}
		return actorCreateResponse(created), nil
	}

	owned, err := s.ensureActorOwner(ctx, actorID)
	if err != nil {
		// Actor creation is valid even if no worker is currently available.
		return actorCreateResponse(created), nil
	}
	return actorCreateResponse(owned), nil
}

// CallActor submits one actor command and waits for its task to reach a terminal
// state within the request timeout.
func (s *Service) CallActor(ctx context.Context, req *logservepb.CallActorRequest) (*logservepb.CallActorResponse, error) {
	if req.GetActorId() == "" {
		return nil, errors.New("actor_id is required")
	}
	if req.GetMethodName() == "" {
		return nil, errors.New("method_name is required")
	}

	timeout := time.Duration(req.GetTimeoutMs()) * time.Millisecond
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadlineAt := time.Now().Add(timeout)

	task, err := s.submitActorCommand(ctx, req, deadlineAt)
	if err != nil {
		return nil, err
	}

	remaining := time.Until(deadlineAt)
	if remaining <= 0 {
		return nil, errors.New("actor call timed out")
	}
	deadline := time.NewTimer(remaining)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, errors.New("actor call timed out")
		case <-ticker.C:
			current, ok := s.meta.GetTask(task.TaskID)
			if !ok {
				return nil, errors.New("actor call task not found")
			}
			switch current.Status {
			case logservepb.TaskStatus_TASK_STATUS_SUCCEEDED:
				actorState, _ := s.meta.GetActor(req.GetActorId())
				return &logservepb.CallActorResponse{
					ActorId:    req.GetActorId(),
					CallId:     task.TaskID,
					Status:     current.Status,
					ResultJson: current.ResultJSON,
					Epoch:      actorState.Epoch,
				}, nil
			case logservepb.TaskStatus_TASK_STATUS_FAILED:
				actorState, _ := s.meta.GetActor(req.GetActorId())
				return &logservepb.CallActorResponse{
					ActorId: req.GetActorId(),
					CallId:  task.TaskID,
					Status:  current.Status,
					Error:   current.Error,
					Epoch:   actorState.Epoch,
				}, nil
			}
		}
	}
}

// submitActorCommand serializes command creation for one actor, chooses the next
// command sequence, records ActorCommandSubmitted, and enqueues the worker task.
func (s *Service) submitActorCommand(ctx context.Context, req *logservepb.CallActorRequest, deadlineAt time.Time) (metadata.Task, error) {
	// The per-actor lock keeps SubmittedCommandCount and mailbox task creation linear
	// even when multiple clients call the same actor concurrently.
	unlock := s.actorLocks.Lock(req.GetActorId())
	defer unlock()

	state, err := s.waitActorOwner(ctx, req.GetActorId(), deadlineAt)
	if err != nil {
		return metadata.Task{}, err
	}
	// Metadata restored from a checkpoint can know command counts before loading a
	// full state blob; replay fills state before dispatching another command.
	if state.CommandCount > 0 && len(state.StateJSON) == 0 {
		replayed, err := s.replayActor(ctx, req.GetActorId())
		if err != nil {
			return metadata.Task{}, err
		}
		state.StateJSON = replayed.State.StateJSON
	}

	callID := newActorCallID()
	argsJSON := defaultJSON(req.GetArgsJson(), []byte(`{"args":[],"kwargs":{}}`))
	idem := req.GetIdempotencyKey()
	if idem == "" {
		idem = callID
	}
	submittedCount := state.SubmittedCommandCount
	if state.CommandCount > submittedCount {
		submittedCount = state.CommandCount
	}
	commandSeq := submittedCount + 1
	spec := &logservepb.TaskSpec{
		TaskId:            callID,
		TaskName:          "actor:" + req.GetMethodName(),
		FunctionName:      req.GetMethodName(),
		ArgsJson:          argsJSON,
		IdempotencyKey:    idem,
		TargetWorkerId:    state.OwnerWorkerID,
		ActorId:           state.ActorID,
		ActorCallId:       callID,
		ActorClassName:    state.ClassName,
		ActorClassSource:  state.ClassSource,
		ActorMethod:       req.GetMethodName(),
		ActorStateJson:    append([]byte(nil), state.StateJSON...),
		ActorInitArgsJson: append([]byte(nil), state.InitArgsJSON...),
		ActorEpoch:        state.Epoch,
	}
	fingerprint, err := taskSpecFingerprint(spec)
	if err != nil {
		return metadata.Task{}, err
	}
	var task metadata.Task
	if existing, ok := s.meta.GetTaskByIdempotencyKey(idem); ok {
		if err := ensureIdempotencyFingerprint("task", idem, existing.IdempotencyFingerprint, fingerprint); err != nil {
			return metadata.Task{}, err
		}
		task = existing
	} else {
		now := actor.NowMs()
		payload, _ := actor.MarshalEventPayload(actor.EventPayload{
			ActorID:     state.ActorID,
			CallID:      callID,
			CommandSeq:  commandSeq,
			MethodName:  req.GetMethodName(),
			ArgsJSON:    argsJSON,
			WorkerID:    state.OwnerWorkerID,
			Epoch:       state.Epoch,
			TimestampMs: now,
		})
		if _, err := s.appendLog(ctx, &logservepb.AppendLogRequest{
			StreamId:       actorStream(state.ActorID),
			EventType:      "ActorCommandSubmitted",
			IdempotencyKey: state.ActorID + ":" + callID + ":submitted",
			Payload:        payload,
		}); err != nil {
			return metadata.Task{}, err
		}
		created, _, err := s.enqueueTaskWithMetadata(ctx, spec, func(task *metadata.Task) {
			task.ActorCommandSeq = commandSeq
		})
		if err != nil {
			return metadata.Task{}, err
		}
		if _, err := s.meta.UpdateActor(state.ActorID, func(current *actor.State) error {
			if commandSeq > current.SubmittedCommandCount {
				current.SubmittedCommandCount = commandSeq
			}
			return nil
		}); err != nil {
			return metadata.Task{}, err
		}
		task = created
	}
	return task, nil
}

// GetActorStatus returns the current actor metadata materialization.
func (s *Service) GetActorStatus(ctx context.Context, req *logservepb.GetActorStatusRequest) (*logservepb.GetActorStatusResponse, error) {
	state, ok := s.meta.GetActor(req.GetActorId())
	if !ok {
		return nil, errors.New("actor not found")
	}
	return actorStatusResponse(state), nil
}

// ReplayActor rebuilds actor state from log records and reports whether it matches
// the current metadata state.
func (s *Service) ReplayActor(ctx context.Context, req *logservepb.ReplayActorRequest) (*logservepb.ReplayActorResponse, error) {
	replayed, err := s.replayActor(ctx, req.GetActorId())
	if err != nil {
		return nil, err
	}
	current, ok := s.meta.GetActor(req.GetActorId())
	consistent := ok && actor.Consistent(replayed.State, current)
	return &logservepb.ReplayActorResponse{
		Replayed:               actorStatusResponse(replayed.State),
		ConsistentWithMetadata: consistent,
		FullReplayCommands:     replayed.FullReplayCommands,
		SnapshotReplayCommands: replayed.SnapshotReplayCommands,
	}, nil
}

// waitActorOwner retries ownership assignment until a worker is available, the
// context is canceled, or the actor call deadline passes.
func (s *Service) waitActorOwner(ctx context.Context, actorID string, deadlineAt time.Time) (actor.State, error) {
	for {
		state, err := s.ensureActorOwner(ctx, actorID)
		if err == nil {
			return state, nil
		}
		if !errors.Is(err, errNoActiveActorWorker) {
			return actor.State{}, err
		}
		if time.Now().After(deadlineAt) {
			return actor.State{}, err
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return actor.State{}, ctx.Err()
		case <-timer.C:
		}
	}
}

// ensureActorOwner returns an active owner or grants ownership to the lowest worker
// ID among currently active workers, advancing the actor epoch as a fence.
func (s *Service) ensureActorOwner(ctx context.Context, actorID string) (actor.State, error) {
	state, ok := s.meta.GetActor(actorID)
	if !ok {
		return actor.State{}, errors.New("actor not found")
	}
	if state.OwnerWorkerID != "" {
		worker, ok := s.meta.GetWorker(state.OwnerWorkerID)
		if ok && time.Since(time.UnixMilli(worker.LastHeartbeat)) <= actorOwnerLease {
			return state, nil
		}
	}
	workers := sortedWorkers(s.meta.ActiveWorkers(actorOwnerLease))
	if len(workers) == 0 {
		return state, errNoActiveActorWorker
	}
	owner := workers[0].WorkerID
	epoch := state.Epoch + 1
	now := actor.NowMs()
	payload, _ := actor.MarshalEventPayload(actor.EventPayload{
		ActorID:     actorID,
		WorkerID:    owner,
		Epoch:       epoch,
		TimestampMs: now,
	})
	if _, err := s.appendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       actorStream(actorID),
		EventType:      "ActorOwnershipGranted",
		IdempotencyKey: fmt.Sprintf("%s:ownership:%d", actorID, epoch),
		Payload:        payload,
	}); err != nil {
		return actor.State{}, err
	}
	return s.meta.UpdateActor(actorID, func(current *actor.State) error {
		current.OwnerWorkerID = owner
		current.Epoch = epoch
		current.Status = logservepb.ActorStatus_ACTOR_STATUS_ACTIVE
		return nil
	})
}

// completeActorCall applies a terminal actor command if the worker still owns the
// actor epoch and the command sequence is exactly the next mailbox entry.
func (s *Service) completeActorCall(ctx context.Context, task metadata.Task, req *logservepb.CompleteTaskRequest) error {
	state, ok := s.meta.GetActor(task.ActorID)
	if !ok {
		return errors.New("actor not found")
	}
	// Worker ID and epoch together fence stale completions from a previous owner.
	if req.GetWorkerId() != state.OwnerWorkerID || req.GetActorEpoch() != state.Epoch {
		return fmt.Errorf("stale actor completion rejected: actor=%s task_epoch=%d request_epoch=%d current_epoch=%d owner=%s worker=%s",
			task.ActorID, task.ActorEpoch, req.GetActorEpoch(), state.Epoch, state.OwnerWorkerID, req.GetWorkerId())
	}
	commandSeq := task.ActorCommandSeq
	if commandSeq == 0 {
		commandSeq = state.CommandCount + 1
	}
	// Actor state is only valid when commands apply in mailbox order.
	if commandSeq != state.CommandCount+1 {
		return fmt.Errorf("out-of-order actor command rejected: actor=%s call_id=%s command_seq=%d expected=%d",
			task.ActorID, task.ActorCallID, commandSeq, state.CommandCount+1)
	}
	spec := s.specForTask(task.TaskID)
	if spec == nil {
		return errors.New("actor task spec not found")
	}
	if req.GetStatus() == logservepb.TaskStatus_TASK_STATUS_FAILED {
		return s.failActorCommand(ctx, task, req, spec, commandSeq)
	}

	stateJSON := actor.NormalizeJSON(req.GetActorStateJson())
	commandCount := commandSeq
	now := actor.NowMs()
	payload, _ := actor.MarshalEventPayload(actor.EventPayload{
		ActorID:      task.ActorID,
		CallID:       task.ActorCallID,
		CommandSeq:   commandSeq,
		MethodName:   spec.GetActorMethod(),
		ArgsJSON:     spec.GetArgsJson(),
		ResultJSON:   req.GetResultJson(),
		StateJSON:    stateJSON,
		WorkerID:     req.GetWorkerId(),
		Epoch:        req.GetActorEpoch(),
		CommandCount: commandCount,
		TimestampMs:  now,
	})
	if _, err := s.appendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       actorStream(task.ActorID),
		EventType:      "ActorCommandApplied",
		IdempotencyKey: task.ActorID + ":" + task.ActorCallID + ":applied",
		Payload:        payload,
	}); err != nil {
		return err
	}
	updated, err := s.meta.UpdateActor(task.ActorID, func(current *actor.State) error {
		current.CommandCount = commandCount
		current.StateJSON = stateJSON
		current.OwnerWorkerID = req.GetWorkerId()
		current.Epoch = req.GetActorEpoch()
		return nil
	})
	if err != nil {
		return err
	}
	if updated.SnapshotEvery > 0 && updated.CommandCount%uint64(updated.SnapshotEvery) == 0 {
		if err := s.createActorSnapshot(ctx, updated); err != nil {
			return err
		}
	}
	if commandCount == 1 || commandCount%100 == 0 {
		observability.Info("actor_command_applied", map[string]any{
			"actor_id":          task.ActorID,
			"call_id":           task.ActorCallID,
			"command_count":     commandCount,
			"epoch":             req.GetActorEpoch(),
			"latency_ms":        now - task.CreatedAtMs,
			"state_json_bytes":  len(stateJSON),
			"args_json_bytes":   len(spec.GetArgsJson()),
			"result_json_bytes": len(req.GetResultJson()),
		})
	}
	return nil
}

// failActorCommand records a failed actor command and advances the command counter
// so later commands are not blocked behind a terminal failure.
func (s *Service) failActorCommand(ctx context.Context, task metadata.Task, req *logservepb.CompleteTaskRequest, spec *logservepb.TaskSpec, commandSeq uint64) error {
	now := actor.NowMs()
	payload, _ := actor.MarshalEventPayload(actor.EventPayload{
		ActorID:      task.ActorID,
		CallID:       task.ActorCallID,
		CommandSeq:   commandSeq,
		MethodName:   spec.GetActorMethod(),
		ArgsJSON:     spec.GetArgsJson(),
		Error:        req.GetError(),
		WorkerID:     req.GetWorkerId(),
		Epoch:        req.GetActorEpoch(),
		CommandCount: commandSeq,
		TimestampMs:  now,
	})
	if _, err := s.appendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       actorStream(task.ActorID),
		EventType:      "ActorCommandFailed",
		IdempotencyKey: task.ActorID + ":" + task.ActorCallID + ":failed",
		Payload:        payload,
	}); err != nil {
		return err
	}
	_, err := s.meta.UpdateActor(task.ActorID, func(current *actor.State) error {
		current.CommandCount = commandSeq
		if commandSeq > current.SubmittedCommandCount {
			current.SubmittedCommandCount = commandSeq
		}
		current.OwnerWorkerID = req.GetWorkerId()
		current.Epoch = req.GetActorEpoch()
		return nil
	})
	return err
}

// createActorSnapshot stores actor state in the object store, records a snapshot
// event, trims older actor log records, and updates snapshot metadata.
func (s *Service) createActorSnapshot(ctx context.Context, state actor.State) error {
	if s.resultStore == nil {
		return nil
	}
	ref, err := objectstore.PutBytes(ctx, s.resultStore, filepath.Join("actors", state.ActorID, "snapshots"), state.StateJSON)
	if err != nil {
		return err
	}
	now := actor.NowMs()
	payload, _ := actor.MarshalEventPayload(actor.EventPayload{
		ActorID:              state.ActorID,
		ClassName:            state.ClassName,
		ClassSource:          state.ClassSource,
		InitArgsJSON:         state.InitArgsJSON,
		WorkerID:             state.OwnerWorkerID,
		Epoch:                state.Epoch,
		SnapshotRef:          ref,
		SnapshotEvery:        state.SnapshotEvery,
		SnapshotCommandCount: state.CommandCount,
		TimestampMs:          now,
	})
	snapshotResp, err := s.appendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       actorStream(state.ActorID),
		EventType:      "ActorSnapshotCreated",
		IdempotencyKey: fmt.Sprintf("%s:snapshot:%d", state.ActorID, state.CommandCount),
		Payload:        payload,
	})
	if err != nil {
		return err
	}
	// After a snapshot, older actor events are no longer needed for replay and can be
	// trimmed while keeping the snapshot event itself as the new stream base.
	if snapshotResp.GetSeq() > 1 {
		if _, err := s.log.TrimStream(ctx, &logservepb.TrimStreamRequest{
			StreamId:  actorStream(state.ActorID),
			BeforeSeq: snapshotResp.GetSeq(),
		}); err != nil {
			observability.Error("actor_stream_trim_failed", err, map[string]any{
				"actor_id":   state.ActorID,
				"before_seq": snapshotResp.GetSeq(),
			})
		}
	}
	_, err = s.meta.UpdateActor(state.ActorID, func(current *actor.State) error {
		current.SnapshotRef = ref
		current.SnapshotCommandCount = state.CommandCount
		return nil
	})
	return err
}

// replayActor streams an actor log through the actor replay engine.
func (s *Service) replayActor(ctx context.Context, actorID string) (actor.ReplayResult, error) {
	streamID := actorStream(actorID)
	return actor.ReplayRawEach(actorID, func(emit func(logrecord.RawRecord) error) error {
		return s.forEachRawLogRecord(ctx, streamID, 1, emit)
	}, s)
}

// specForTask returns a cloned task spec from the in-memory spec cache.
func (s *Service) specForTask(taskID string) *logservepb.TaskSpec {
	s.specMu.RLock()
	defer s.specMu.RUnlock()
	return cloneSpec(s.specs[taskID])
}

// actorCreateResponse shapes actor metadata for CreateActor responses.
func actorCreateResponse(state actor.State) *logservepb.CreateActorResponse {
	return &logservepb.CreateActorResponse{
		ActorId:       state.ActorID,
		Status:        state.Status,
		OwnerWorkerId: state.OwnerWorkerID,
		Epoch:         state.Epoch,
	}
}

// actorStatusResponse shapes actor metadata for status and replay responses.
func actorStatusResponse(state actor.State) *logservepb.GetActorStatusResponse {
	return &logservepb.GetActorStatusResponse{
		ActorId:              state.ActorID,
		ClassName:            state.ClassName,
		Status:               state.Status,
		OwnerWorkerId:        state.OwnerWorkerID,
		Epoch:                state.Epoch,
		CommandCount:         state.CommandCount,
		SnapshotRef:          state.SnapshotRef,
		SnapshotCommandCount: state.SnapshotCommandCount,
		StateJson:            append([]byte(nil), state.StateJSON...),
		CreatedAtMs:          state.CreatedAtMs,
		UpdatedAtMs:          state.UpdatedAtMs,
	}
}

// defaultJSON returns a defensive copy of value or fallback when the caller omitted
// JSON input.
func defaultJSON(value []byte, fallback []byte) []byte {
	if len(value) == 0 {
		return append([]byte(nil), fallback...)
	}
	return append([]byte(nil), value...)
}
