package webapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/workflow"
)

type submitWorkflowRequest struct {
	WorkflowName   string          `json:"workflow_name"`
	Definition     json.RawMessage `json:"definition"`
	DefinitionJSON json.RawMessage `json:"definition_json"`
	IdempotencyKey string          `json:"idempotency_key"`
}

func (s *Server) handleListWorkflows(w http.ResponseWriter, r *http.Request) {
	dashboard, err := s.dashboard(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"workflows": dashboard.Workflows})
}

func (s *Server) handleSubmitWorkflow(w http.ResponseWriter, r *http.Request) {
	var input submitWorkflowRequest
	if err := decodeJSON(w, r, &input); err != nil {
		writeErr(w, err)
		return
	}
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

func (s *Server) handleValidateWorkflow(w http.ResponseWriter, r *http.Request) {
	var input submitWorkflowRequest
	if err := decodeJSON(w, r, &input); err != nil {
		writeErr(w, err)
		return
	}
	definition := defaultRaw(input.DefinitionJSON, input.Definition)
	if err := validateRawJSON("definition", definition, maxSourceBytes); err != nil {
		writeErr(w, err)
		return
	}
	def, err := workflow.ParseDefinition(definition)
	if err != nil {
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
