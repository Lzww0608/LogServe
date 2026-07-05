package webapi

// This file implements dashboard and admin configuration endpoints, including
// backpressure threshold updates.

import (
	"fmt"
	"net/http"

	"github.com/logserve/logserve/gen/logservepb"
)

// backpressureRequest carries admin-tunable scheduler/log thresholds from the
// console.
type backpressureRequest struct {
	QueueHighWatermark  uint32 `json:"queue_high_watermark"`
	RedeliveryTimeoutMs int64  `json:"redelivery_timeout_ms"`
	LogAppendSlowMs     int64  `json:"log_append_slow_ms"`
}

// handleDashboard returns the current aggregated dashboard snapshot.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	dto, err := s.dashboard(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, dto)
}

// handleAdminConfig returns the subset of dashboard state exposed as mutable or
// diagnostic admin configuration.
func (s *Server) handleAdminConfig(w http.ResponseWriter, r *http.Request) {
	dto, err := s.dashboard(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	// Reuse dashboard state so the admin page reports the exact thresholds and
	// materializer lag currently visible to the rest of the console.
	writeJSON(w, map[string]any{
		"scheduling_policy":       dto.SchedulingPolicy,
		"queue_high_watermark":    dto.QueueHighWatermark,
		"redelivery_timeout_ms":   dto.RedeliveryTimeoutMs,
		"log_append_slow_ms":      dto.LogAppendSlowMs,
		"metadata_materializer":   dto.MetadataMaterializerStats,
		"compactable_log_records": dto.CompactableLogRecords,
		"compactable_log_bytes":   dto.CompactableLogBytes,
	})
}

// handleSetBackpressure validates admin threshold input and forwards it to the
// control plane.
func (s *Server) handleSetBackpressure(w http.ResponseWriter, r *http.Request) {
	var input backpressureRequest
	if err := decodeJSON(w, r, &input); err != nil {
		writeErr(w, err)
		return
	}
	if err := validateBackpressureRequest(input); err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := requestContext(r, s.cfg.RequestTimeout)
	defer cancel()
	resp, err := s.clients.Control.SetBackpressure(ctx, &logservepb.SetBackpressureRequest{
		QueueHighWatermark:  input.QueueHighWatermark,
		RedeliveryTimeoutMs: input.RedeliveryTimeoutMs,
		LogAppendSlowMs:     input.LogAppendSlowMs,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"queue_high_watermark":  resp.GetQueueHighWatermark(),
		"redelivery_timeout_ms": resp.GetRedeliveryTimeoutMs(),
		"log_append_slow_ms":    resp.GetLogAppendSlowMs(),
	})
}

// validateBackpressureRequest rejects zero or negative thresholds that would make
// backpressure and redelivery behavior nonsensical.
func validateBackpressureRequest(input backpressureRequest) error {
	// Zero thresholds would effectively disable scheduling/backpressure timers in
	// surprising ways, so the HTTP layer rejects them before reaching the control plane.
	if input.QueueHighWatermark == 0 {
		return fmt.Errorf("%w: queue_high_watermark must be greater than 0", errInvalidInput)
	}
	if input.RedeliveryTimeoutMs <= 0 {
		return fmt.Errorf("%w: redelivery_timeout_ms must be greater than 0", errInvalidInput)
	}
	if input.LogAppendSlowMs <= 0 {
		return fmt.Errorf("%w: log_append_slow_ms must be greater than 0", errInvalidInput)
	}
	return nil
}
