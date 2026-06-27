package webapi

import "net/http"

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
