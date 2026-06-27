package webapi

import (
	"fmt"
	"net/http"

	"github.com/logserve/logserve/gen/logservepb"
)

type backpressureRequest struct {
	QueueHighWatermark  uint32 `json:"queue_high_watermark"`
	RedeliveryTimeoutMs int64  `json:"redelivery_timeout_ms"`
	LogAppendSlowMs     int64  `json:"log_append_slow_ms"`
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	dto, err := s.dashboard(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, dto)
}

func (s *Server) handleAdminConfig(w http.ResponseWriter, r *http.Request) {
	dto, err := s.dashboard(r)
	if err != nil {
		writeErr(w, err)
		return
	}
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

func validateBackpressureRequest(input backpressureRequest) error {
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
