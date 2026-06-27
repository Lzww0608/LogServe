package webapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
)

type createActorRequest struct {
	ClassName      string          `json:"class_name"`
	ClassSource    string          `json:"class_source"`
	InitArgs       json.RawMessage `json:"init_args"`
	InitKwargs     json.RawMessage `json:"init_kwargs"`
	InitArgsJSON   json.RawMessage `json:"init_args_json"`
	IdempotencyKey string          `json:"idempotency_key"`
	SnapshotEvery  uint32          `json:"snapshot_every"`
}

type callActorRequest struct {
	MethodName     string          `json:"method_name"`
	Args           json.RawMessage `json:"args"`
	Kwargs         json.RawMessage `json:"kwargs"`
	ArgsJSON       json.RawMessage `json:"args_json"`
	IdempotencyKey string          `json:"idempotency_key"`
	TimeoutMs      int64           `json:"timeout_ms"`
}

func (s *Server) handleListActors(w http.ResponseWriter, r *http.Request) {
	dashboard, err := s.dashboard(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"actors": dashboard.Actors})
}

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
