package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/actor"
	"github.com/logserve/logserve/internal/metadata"
	"github.com/logserve/logserve/internal/observability"
)

var errNoActiveActorWorker = errors.New("no active worker available for actor")

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

	payload, _ := json.Marshal(actor.EventPayload{
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

func (s *Service) submitActorCommand(ctx context.Context, req *logservepb.CallActorRequest, deadlineAt time.Time) (metadata.Task, error) {
	lock := s.actorLock(req.GetActorId())
	lock.Lock()
	defer lock.Unlock()

	state, err := s.waitActorOwner(ctx, req.GetActorId(), deadlineAt)
	if err != nil {
		return metadata.Task{}, err
	}
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
		payload, _ := json.Marshal(actor.EventPayload{
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

func (s *Service) GetActorStatus(ctx context.Context, req *logservepb.GetActorStatusRequest) (*logservepb.GetActorStatusResponse, error) {
	state, ok := s.meta.GetActor(req.GetActorId())
	if !ok {
		return nil, errors.New("actor not found")
	}
	return actorStatusResponse(state), nil
}

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
	payload, _ := json.Marshal(actor.EventPayload{
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

func (s *Service) completeActorCall(ctx context.Context, task metadata.Task, req *logservepb.CompleteTaskRequest) error {
	state, ok := s.meta.GetActor(task.ActorID)
	if !ok {
		return errors.New("actor not found")
	}
	if req.GetWorkerId() != state.OwnerWorkerID || req.GetActorEpoch() != state.Epoch {
		return fmt.Errorf("stale actor completion rejected: actor=%s task_epoch=%d request_epoch=%d current_epoch=%d owner=%s worker=%s",
			task.ActorID, task.ActorEpoch, req.GetActorEpoch(), state.Epoch, state.OwnerWorkerID, req.GetWorkerId())
	}
	commandSeq := task.ActorCommandSeq
	if commandSeq == 0 {
		commandSeq = state.CommandCount + 1
	}
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
	payload, _ := json.Marshal(actor.EventPayload{
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
			"actor_id":      task.ActorID,
			"call_id":       task.ActorCallID,
			"command_count": commandCount,
			"epoch":         req.GetActorEpoch(),
			"latency_ms":    now - task.CreatedAtMs,
		})
	}
	return nil
}

func (s *Service) failActorCommand(ctx context.Context, task metadata.Task, req *logservepb.CompleteTaskRequest, spec *logservepb.TaskSpec, commandSeq uint64) error {
	now := actor.NowMs()
	payload, _ := json.Marshal(actor.EventPayload{
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

func (s *Service) createActorSnapshot(ctx context.Context, state actor.State) error {
	if s.resultStore == nil {
		return nil
	}
	ref, err := s.resultStore.Put(ctx, filepath.Join("actors", state.ActorID, "snapshots"), state.StateJSON)
	if err != nil {
		return err
	}
	now := actor.NowMs()
	payload, _ := json.Marshal(actor.EventPayload{
		ActorID:              state.ActorID,
		SnapshotRef:          ref,
		SnapshotCommandCount: state.CommandCount,
		TimestampMs:          now,
	})
	if _, err := s.appendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       actorStream(state.ActorID),
		EventType:      "ActorSnapshotCreated",
		IdempotencyKey: fmt.Sprintf("%s:snapshot:%d", state.ActorID, state.CommandCount),
		Payload:        payload,
	}); err != nil {
		return err
	}
	_, err = s.meta.UpdateActor(state.ActorID, func(current *actor.State) error {
		current.SnapshotRef = ref
		current.SnapshotCommandCount = state.CommandCount
		return nil
	})
	return err
}

func (s *Service) replayActor(ctx context.Context, actorID string) (actor.ReplayResult, error) {
	resp, err := s.log.ReadLog(ctx, &logservepb.ReadLogRequest{
		StreamId: actorStream(actorID),
		FromSeq:  1,
		Limit:    100_000,
	})
	if err != nil {
		return actor.ReplayResult{}, err
	}
	return actor.Replay(actorID, resp.GetRecords(), s)
}

func (s *Service) specForTask(taskID string) *logservepb.TaskSpec {
	s.specMu.RLock()
	defer s.specMu.RUnlock()
	return cloneSpec(s.specs[taskID])
}

func actorCreateResponse(state actor.State) *logservepb.CreateActorResponse {
	return &logservepb.CreateActorResponse{
		ActorId:       state.ActorID,
		Status:        state.Status,
		OwnerWorkerId: state.OwnerWorkerID,
		Epoch:         state.Epoch,
	}
}

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

func defaultJSON(value []byte, fallback []byte) []byte {
	if len(value) == 0 {
		return append([]byte(nil), fallback...)
	}
	return append([]byte(nil), value...)
}
