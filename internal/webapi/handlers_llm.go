package webapi

// This file implements model registry, LLM submission/replay, and scheduling
// policy HTTP endpoints.

import (
	"net/http"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
)

// registerModelRequest is the admin JSON shape for adding model metadata to the
// control plane.
type registerModelRequest struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	SizeBytes uint64 `json:"size_bytes"`
	Path      string `json:"path"`
	Adapter   string `json:"adapter"`
}

// submitLLMRequest is the JSON shape for submitting one LLM task through the
// control-plane LLM path.
type submitLLMRequest struct {
	ModelName      string `json:"model_name"`
	ModelVersion   string `json:"model_version"`
	Prompt         string `json:"prompt"`
	MaxTokens      uint32 `json:"max_tokens"`
	Adapter        string `json:"adapter"`
	IdempotencyKey string `json:"idempotency_key"`
}

// schedulingPolicyRequest carries the scheduler policy string accepted by the
// admin scheduling endpoint.
type schedulingPolicyRequest struct {
	Policy string `json:"policy"`
}

// handleListModels returns registered model rows from the dashboard snapshot.
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	dashboard, err := s.dashboard(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"models": dashboard.Models})
}

// handleRegisterModel validates request JSON and forwards model registration to
// the control plane.
func (s *Server) handleRegisterModel(w http.ResponseWriter, r *http.Request) {
	var input registerModelRequest
	if err := decodeJSON(w, r, &input); err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := requestContext(r, s.cfg.RequestTimeout)
	defer cancel()
	resp, err := s.clients.Control.RegisterModel(ctx, &logservepb.RegisterModelRequest{Model: &logservepb.ModelInfo{
		Name:      input.Name,
		Version:   input.Version,
		SizeBytes: input.SizeBytes,
		Path:      input.Path,
		Adapter:   input.Adapter,
	}})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, modelDTO(resp.GetModel()))
}

// handleSubmitLLM submits an LLM task and optionally waits by polling the
// underlying task status endpoint.
func (s *Server) handleSubmitLLM(w http.ResponseWriter, r *http.Request) {
	var input submitLLMRequest
	if err := decodeJSON(w, r, &input); err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := requestContext(r, s.cfg.RequestTimeout)
	defer cancel()
	resp, err := s.clients.Control.SubmitLLM(ctx, &logservepb.SubmitLLMRequest{
		ModelName:      input.ModelName,
		ModelVersion:   input.ModelVersion,
		Prompt:         input.Prompt,
		MaxTokens:      input.MaxTokens,
		Adapter:        input.Adapter,
		IdempotencyKey: input.IdempotencyKey,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	dto := LLMDTO{TaskID: resp.GetTaskId(), Status: taskStatusString(resp.GetStatus())}
	if waitRequested(r) {
		task, err := s.waitTask(r, resp.GetTaskId(), waitTimeout(r, 60*time.Second))
		if err != nil {
			writeErr(w, err)
			return
		}
		dto.Status = task.Status
		dto.Result = task.Result
		dto.Error = task.Error
		dto.WorkerID = task.WorkerID
	}
	writeJSON(w, dto)
}

// handleReplayLLM returns log-derived LLM replay events and cache/latency
// diagnostics for a task.
func (s *Server) handleReplayLLM(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("task_id")
	ctx, cancel := requestContext(r, s.cfg.RequestTimeout)
	defer cancel()
	resp, err := s.clients.Control.ReplayLLM(ctx, &logservepb.ReplayLLMRequest{TaskId: taskID})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, llmReplayDTO(resp))
}

// handleSetSchedulingPolicy parses a policy alias and forwards the scheduler
// update to the control plane.
func (s *Server) handleSetSchedulingPolicy(w http.ResponseWriter, r *http.Request) {
	var input schedulingPolicyRequest
	if err := decodeJSON(w, r, &input); err != nil {
		writeErr(w, err)
		return
	}
	policy, err := parseSchedulingPolicy(input.Policy)
	if err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := requestContext(r, s.cfg.RequestTimeout)
	defer cancel()
	resp, err := s.clients.Control.SetSchedulingPolicy(ctx, &logservepb.SetSchedulingPolicyRequest{Policy: policy})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]string{"policy": schedulingPolicyString(resp.GetPolicy())})
}
