package webapi

// This file implements workflow list, submit, detail, replay, validation, and
// wait-polling endpoints.

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/workflow"
)

// submitWorkflowRequest is the JSON shape for workflow submission and validation.
// It supports either definition or definition_json for frontend compatibility.
type submitWorkflowRequest struct {
	WorkflowName   string          `json:"workflow_name"`
	Definition     json.RawMessage `json:"definition"`
	DefinitionJSON json.RawMessage `json:"definition_json"`
	IdempotencyKey string          `json:"idempotency_key"`
}

// handleListWorkflows filters dashboard workflows by status and applies shared
// pagination.
func (s *Server) handleListWorkflows(w http.ResponseWriter, r *http.Request) {
	dashboard, err := s.dashboard(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	status := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("status")))
	out := make([]WorkflowDTO, 0, len(dashboard.Workflows))
	for _, workflow := range dashboard.Workflows {
		if status != "" && workflow.Status != status {
			continue
		}
		out = append(out, workflow)
	}
	params, err := parsePaginationParams(r, len(out))
	if err != nil {
		writeErr(w, err)
		return
	}
	page := paginate(len(out), params)
	writeJSON(w, map[string]any{
		"workflows":       out[page.Start:page.End],
		"limit":           page.Limit,
		"total_count":     page.TotalCount,
		"next_page_token": page.NextPageToken,
	})
}

// handleSubmitWorkflow validates raw workflow JSON, submits it to the control
// plane, and optionally waits for terminal status.
func (s *Server) handleSubmitWorkflow(w http.ResponseWriter, r *http.Request) {
	var input submitWorkflowRequest
	if err := decodeJSON(w, r, &input); err != nil {
		writeErr(w, err)
		return
	}
	// definition_json takes precedence for callers that already serialized the
	// workflow; definition remains for older console payloads.
	definition := defaultRaw(input.DefinitionJSON, input.Definition)
	if err := validateRawJSON("definition", definition, maxSourceBytes); err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := requestContext(r, s.cfg.RequestTimeout)
	defer cancel()
	resp, err := s.clients.Control.SubmitWorkflow(ctx, &logservepb.SubmitWorkflowRequest{
		WorkflowName:   input.WorkflowName,
		DefinitionJson: definition,
		IdempotencyKey: input.IdempotencyKey,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	dto := WorkflowDTO{WorkflowID: resp.GetWorkflowId(), Status: workflowStatusString(resp.GetStatus())}
	if waitRequested(r) {
		dto, err = s.waitWorkflow(r, resp.GetWorkflowId(), waitTimeout(r, 60*time.Second))
		if err != nil {
			writeErr(w, err)
			return
		}
	}
	writeJSON(w, dto)
}

// handleGetWorkflow reads detailed workflow status from the control plane.
func (s *Server) handleGetWorkflow(w http.ResponseWriter, r *http.Request) {
	workflowID := r.PathValue("workflow_id")
	ctx, cancel := requestContext(r, s.cfg.RequestTimeout)
	defer cancel()
	resp, err := s.clients.Control.GetWorkflowStatus(ctx, &logservepb.GetWorkflowStatusRequest{WorkflowId: workflowID})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, workflowStatusDTO(resp))
}

// handleReplayWorkflow returns the workflow state reconstructed from logs plus a
// consistency flag against metadata state.
func (s *Server) handleReplayWorkflow(w http.ResponseWriter, r *http.Request) {
	workflowID := r.PathValue("workflow_id")
	ctx, cancel := requestContext(r, s.cfg.RequestTimeout)
	defer cancel()
	resp, err := s.clients.Control.ReplayWorkflow(ctx, &logservepb.ReplayWorkflowRequest{WorkflowId: workflowID})
	if err != nil {
		writeErr(w, err)
		return
	}
	dto := workflowStatusDTO(resp.GetReplayed())
	writeJSON(w, map[string]any{
		"workflow":                 dto,
		"consistent_with_metadata": resp.GetConsistentWithMetadata(),
	})
}

// handleValidateWorkflow parses workflow JSON locally and returns a normalized
// definition without submitting it to the control plane.
func (s *Server) handleValidateWorkflow(w http.ResponseWriter, r *http.Request) {
	var input submitWorkflowRequest
	if err := decodeJSON(w, r, &input); err != nil {
		writeErr(w, err)
		return
	}
	// definition_json takes precedence for callers that already serialized the
	// workflow; definition remains for older console payloads.
	definition := defaultRaw(input.DefinitionJSON, input.Definition)
	if err := validateRawJSON("definition", definition, maxSourceBytes); err != nil {
		writeErr(w, err)
		return
	}
	def, err := workflow.ParseDefinition(definition)
	if err != nil {
		// Validation is a form-style endpoint: malformed workflow definitions are
		// returned as a valid HTTP response with valid=false instead of writeErr.
		writeJSON(w, map[string]any{
			"valid":   false,
			"message": err.Error(),
		})
		return
	}
	writeJSON(w, map[string]any{
		"valid":                 true,
		"normalized_definition": def,
		"warnings":              []string{},
	})
}

// waitWorkflow polls workflow status until completion/failure or timeout. On
// timeout it returns the latest observed workflow together with the context error.
func (s *Server) waitWorkflow(r *http.Request, workflowID string, timeout time.Duration) (WorkflowDTO, error) {
	ctx, cancel := requestContext(r, timeout)
	defer cancel()
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		resp, err := s.clients.Control.GetWorkflowStatus(ctx, &logservepb.GetWorkflowStatusRequest{WorkflowId: workflowID})
		if err != nil {
			return WorkflowDTO{}, err
		}
		dto := workflowStatusDTO(resp)
		if terminalWorkflowStatus(dto.Status) {
			return dto, nil
		}
		select {
		case <-ctx.Done():
			return dto, ctx.Err()
		case <-ticker.C:
		}
	}
}
