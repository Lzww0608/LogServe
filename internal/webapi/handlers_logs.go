package webapi

// This file implements log explorer endpoints and converts log payload bytes
// into JSON, text, or base64 views.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/logserve/logserve/gen/logservepb"
)

const maxLogReadLimit uint32 = 1000

// logRecordDTO is the log explorer representation of one log record. Payload is
// exposed as JSON, text, or base64 depending on its bytes.
type logRecordDTO struct {
	StreamID       string          `json:"stream_id"`
	Seq            uint64          `json:"seq"`
	EventType      string          `json:"event_type,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	PayloadJSON    json.RawMessage `json:"payload_json,omitempty"`
	PayloadText    string          `json:"payload_text,omitempty"`
	PayloadBase64  string          `json:"payload_base64,omitempty"`
	TimestampMs    int64           `json:"timestamp_ms,omitempty"`
	CRC32          uint32          `json:"crc32,omitempty"`
}

// streamStatsDTO mirrors log-service stream stats for the log explorer UI.
type streamStatsDTO struct {
	StreamID           string `json:"stream_id"`
	FirstSeq           uint64 `json:"first_seq"`
	NextSeq            uint64 `json:"next_seq"`
	TrimmedBeforeSeq   uint64 `json:"trimmed_before_seq"`
	CompactableRecords uint64 `json:"compactable_records"`
	CompactableBytes   uint64 `json:"compactable_bytes"`
}

// handleListLogStreams returns stream IDs plus compactability stats for an
// optional prefix.
func (s *Server) handleListLogStreams(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	ctx, cancel := requestContext(r, s.cfg.RequestTimeout)
	defer cancel()
	streams, err := s.clients.Log.ListStreams(ctx, &logservepb.ListStreamsRequest{Prefix: prefix})
	if err != nil {
		writeErr(w, err)
		return
	}
	stats, err := s.clients.Log.GetStreamStats(ctx, &logservepb.GetStreamStatsRequest{Prefix: prefix})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{
		"stream_ids": streams.GetStreamIds(),
		"stats":      streamStatsDTOs(stats.GetStreams()),
	})
}

// handleReadLogStream reads a bounded page from one log stream and computes the
// next sequence cursor used by the explorer.
func (s *Server) handleReadLogStream(w http.ResponseWriter, r *http.Request) {
	streamID := r.PathValue("stream_id")
	if streamID == "" {
		writeErr(w, fmt.Errorf("%w: stream_id is required", errInvalidInput))
		return
	}
	fromSeq, err := uint64Query(r, "from_seq", 1)
	if err != nil {
		writeErr(w, err)
		return
	}
	limit, err := uint32Query(r, "limit", 100)
	if err != nil {
		writeErr(w, err)
		return
	}
	if limit == 0 || limit > maxLogReadLimit {
		writeErr(w, fmt.Errorf("%w: limit must be between 1 and %d", errInvalidInput, maxLogReadLimit))
		return
	}
	ctx, cancel := requestContext(r, s.cfg.RequestTimeout)
	defer cancel()
	records, err := s.clients.Log.ReadLog(ctx, &logservepb.ReadLogRequest{StreamId: streamID, FromSeq: fromSeq, Limit: limit})
	if err != nil {
		writeErr(w, err)
		return
	}
	stats, err := s.clients.Log.GetStreamStats(ctx, &logservepb.GetStreamStatsRequest{StreamId: streamID})
	if err != nil {
		writeErr(w, err)
		return
	}
	recordDTOs := logRecordDTOs(records.GetRecords())
	nextSeq := fromSeq
	for _, record := range recordDTOs {
		if record.Seq >= nextSeq {
			nextSeq = record.Seq + 1
		}
	}
	var stat any
	var statDTO streamStatsDTO
	hasStat := false
	if items := streamStatsDTOs(stats.GetStreams()); len(items) > 0 {
		statDTO = items[0]
		stat = statDTO
		hasStat = true
	}
	// If the requested cursor is before a trimmed prefix and no records are returned,
	// jump the next cursor to the first retained sequence to keep polling moving.
	if len(recordDTOs) == 0 && hasStat && nextSeq < statDTO.FirstSeq && nextSeq < statDTO.NextSeq {
		nextSeq = statDTO.FirstSeq
	}
	hasMore := false
	if hasStat {
		hasMore = nextSeq < statDTO.NextSeq
	}
	writeJSON(w, map[string]any{
		"stream_id": streamID,
		"from_seq":  fromSeq,
		"limit":     limit,
		"records":   recordDTOs,
		"stats":     stat,
		"next_seq":  nextSeq,
		"has_more":  hasMore,
	})
}

// handleLogStats returns per-stream retention and compactability stats for one
// stream or prefix.
func (s *Server) handleLogStats(w http.ResponseWriter, r *http.Request) {
	streamID := r.URL.Query().Get("stream_id")
	prefix := r.URL.Query().Get("prefix")
	ctx, cancel := requestContext(r, s.cfg.RequestTimeout)
	defer cancel()
	stats, err := s.clients.Log.GetStreamStats(ctx, &logservepb.GetStreamStatsRequest{StreamId: streamID, Prefix: prefix})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"streams": streamStatsDTOs(stats.GetStreams())})
}

// logRecordDTOs converts protobuf log records into explorer DTOs.
func logRecordDTOs(records []*logservepb.LogRecord) []logRecordDTO {
	out := make([]logRecordDTO, 0, len(records))
	for _, record := range records {
		out = append(out, logRecordDTOFromProto(record))
	}
	return out
}

// logRecordDTOFromProto chooses the most useful payload representation without
// losing binary data.
func logRecordDTOFromProto(record *logservepb.LogRecord) logRecordDTO {
	out := logRecordDTO{
		StreamID:       record.GetStreamId(),
		Seq:            record.GetSeq(),
		EventType:      record.GetEventType(),
		IdempotencyKey: record.GetIdempotencyKey(),
		TimestampMs:    record.GetTimestampMs(),
		CRC32:          record.GetCrc32(),
	}
	payload := record.GetPayload()
	if len(payload) == 0 {
		return out
	}
	if json.Valid(payload) {
		out.PayloadJSON = append(json.RawMessage(nil), payload...)
		return out
	}
	if utf8.Valid(payload) {
		out.PayloadText = string(payload)
		return out
	}
	out.PayloadBase64 = base64.StdEncoding.EncodeToString(payload)
	return out
}

// streamStatsDTOs converts protobuf stream stats into JSON DTOs.
func streamStatsDTOs(stats []*logservepb.StreamStats) []streamStatsDTO {
	out := make([]streamStatsDTO, 0, len(stats))
	for _, stat := range stats {
		out = append(out, streamStatsDTO{
			StreamID:           stat.GetStreamId(),
			FirstSeq:           stat.GetFirstSeq(),
			NextSeq:            stat.GetNextSeq(),
			TrimmedBeforeSeq:   stat.GetTrimmedBeforeSeq(),
			CompactableRecords: stat.GetCompactableRecords(),
			CompactableBytes:   stat.GetCompactableBytes(),
		})
	}
	return out
}

// uint64Query parses an unsigned integer query parameter with a fallback.
func uint64Query(r *http.Request, name string, fallback uint64) (uint64, error) {
	value := r.URL.Query().Get(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %s must be an unsigned integer", errInvalidInput, name)
	}
	return parsed, nil
}

// uint32Query parses an unsigned integer query parameter and rejects values that
// cannot fit into uint32.
func uint32Query(r *http.Request, name string, fallback uint32) (uint32, error) {
	value, err := uint64Query(r, name, uint64(fallback))
	if err != nil {
		return 0, err
	}
	if value > uint64(^uint32(0)) {
		return 0, fmt.Errorf("%w: %s is too large", errInvalidInput, name)
	}
	return uint32(value), nil
}
