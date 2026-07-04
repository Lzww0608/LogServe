package webapi

// This file implements actor HTTP endpoints for list, create, status, call,
// and replay operations.

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
)

// createActorRequest is the JSON shape for creating an actor from source and
// optional init args.
type createActorRequest struct {
	ClassName      string          `json:"class_name"`
	ClassSource    string          `json:"class_source"`
	InitArgs       json.RawMessage `json:"init_args"`
	InitKwargs     json.RawMessage `json:"init_kwargs"`
	InitArgsJSON   json.RawMessage `json:"init_args_json"`
	IdempotencyKey string          `json:"idempotency_key"`
	SnapshotEvery  uint32          `json:"snapshot_every"`
}

// callActorRequest is the JSON shape for invoking an actor method with optional
// args and timeout override.
type callActorRequest struct {
	MethodName     string          `json:"method_name"`
	Args           json.RawMessage `json:"args"`
	Kwargs         json.RawMessage `json:"kwargs"`
	ArgsJSON       json.RawMessage `json:"args_json"`
	IdempotencyKey string          `json:"idempotency_key"`
	TimeoutMs      int64           `json:"timeout_ms"`
}

// handleListActors returns actor rows from the dashboard snapshot.
func (s *Server) handleListActors(w http.ResponseWriter, r *http.Request) {
	dashboard, err := s.dashboard(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"actors": dashboard.Actors})
}

// handleCreateActor envelopes init args, forwards actor creation to the control
// plane, and returns initial placement metadata.
func (s *Server) handleCreateActor(w http.ResponseWriter, r *http.Request) {
	var input createActorRequest
	if err := decodeJSON(w, r, &input); err != nil {
		writeErr(w, err)
		return
	}
	initArgsJSON := []byte(input.InitArgsJSON)
	var err error
	if len(initArgsJSON) == 0 {
		initArgsJSON, err = envelopeArgs(input.InitArgs, input.InitKwargs)
		if err != nil {
			writeErr(w, err)
			return
		}
	} else if err := validateRawJSON("init_args_json", input.InitArgsJSON, maxJSONBytes); err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := requestContext(r, s.cfg.RequestTimeout)
	defer cancel()
	resp, err := s.clients.Control.CreateActor(ctx, &logservepb.CreateActorRequest{
		ClassName:      input.ClassName,
		ClassSource:    input.ClassSource,
		InitArgsJson:   initArgsJSON,
		IdempotencyKey: input.IdempotencyKey,
		SnapshotEvery:  input.SnapshotEvery,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, ActorDTO{
		ActorID:       resp.GetActorId(),
		Status:        actorStatusString(resp.GetStatus()),
		OwnerWorkerID: resp.GetOwnerWorkerId(),
		Epoch:         resp.GetEpoch(),
	})
}

// handleGetActor returns current actor status from the control plane.
func (s *Server) handleGetActor(w http.ResponseWriter, r *http.Request) {
	actorID := r.PathValue("actor_id")
	ctx, cancel := requestContext(r, s.cfg.RequestTimeout)
	defer cancel()
	resp, err := s.clients.Control.GetActorStatus(ctx, &logservepb.GetActorStatusRequest{ActorId: actorID})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, actorStatusDTO(resp))
}

// handleCallActor envelopes method args and forwards the call. When a call
// timeout is supplied, the HTTP RPC timeout is extended slightly so the backend
// can return the actor result before the client context expires.
func (s *Server) handleCallActor(w http.ResponseWriter, r *http.Request) {
	actorID := r.PathValue("actor_id")
	var input callActorRequest
	if err := decodeJSON(w, r, &input); err != nil {
		writeErr(w, err)
		return
	}
	argsJSON := []byte(input.ArgsJSON)
	var err error
	if len(argsJSON) == 0 {
		argsJSON, err = envelopeArgs(input.Args, input.Kwargs)
		if err != nil {
			writeErr(w, err)
			return
		}
	} else if err := validateRawJSON("args_json", input.ArgsJSON, maxJSONBytes); err != nil {
		writeErr(w, err)
		return
	}
	timeout := s.cfg.RequestTimeout
	if input.TimeoutMs > 0 {
		timeout = time.Duration(input.TimeoutMs+5000) * time.Millisecond
	}
	ctx, cancel := requestContext(r, timeout)
	defer cancel()
	resp, err := s.clients.Control.CallActor(ctx, &logservepb.CallActorRequest{
		ActorId:        actorID,
		MethodName:     input.MethodName,
		ArgsJson:       argsJSON,
		IdempotencyKey: input.IdempotencyKey,
		TimeoutMs:      input.TimeoutMs,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, actorCallDTO(resp))
}

// handleReplayActor returns actor state reconstructed from logs together with
// command-count diagnostics for full and snapshot replay paths.
func (s *Server) handleReplayActor(w http.ResponseWriter, r *http.Request) {
	actorID := r.PathValue("actor_id")
	ctx, cancel := requestContext(r, s.cfg.RequestTimeout)
	defer cancel()
	resp, err := s.clients.Control.ReplayActor(ctx, &logservepb.ReplayActorRequest{ActorId: actorID})
	if err != nil {
		writeErr(w, err)
		return
	}
	dto := actorStatusDTO(resp.GetReplayed())
	dto.Consistent = resp.GetConsistentWithMetadata()
	dto.FullReplayCommands = resp.GetFullReplayCommands()
	dto.SnapshotReplayCommands = resp.GetSnapshotReplayCommands()
	writeJSON(w, dto)
}
