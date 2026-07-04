// Package logrecord contains the lightweight record shape shared by raw log
// readers. It keeps the protobuf boundary out of hot read paths while still
// preserving the exact wire fields exposed by the log service.
package logrecord

import "github.com/logserve/logserve/gen/logservepb"

// RawRecord is the allocation-conscious form emitted by raw log readers.
// Payload may alias a scratch buffer or mmap slice owned by the caller, so
// consumers must copy it when the data must outlive the callback that received
// the record.
type RawRecord struct {
	StreamID       string
	Seq            uint64
	EventType      string
	IdempotencyKey string
	Payload        []byte
	TimestampMs    int64
	CRC32          uint32
}

// FromProto converts a protobuf log record into the raw internal form. A nil
// protobuf message is treated as an empty record so callers can safely pass
// optional request fields through this boundary.
func FromProto(rec *logservepb.LogRecord) RawRecord {
	if rec == nil {
		return RawRecord{}
	}
	return RawRecord{
		StreamID:       rec.GetStreamId(),
		Seq:            rec.GetSeq(),
		EventType:      rec.GetEventType(),
		IdempotencyKey: rec.GetIdempotencyKey(),
		Payload:        rec.GetPayload(),
		TimestampMs:    rec.GetTimestampMs(),
		CRC32:          rec.GetCrc32(),
	}
}

// ToProto converts a raw record back to the gRPC/public representation. The
// payload slice is reused rather than copied because this helper is normally
// called at the final serialization boundary.
func ToProto(rec RawRecord) *logservepb.LogRecord {
	return &logservepb.LogRecord{
		StreamId:       rec.StreamID,
		Seq:            rec.Seq,
		EventType:      rec.EventType,
		IdempotencyKey: rec.IdempotencyKey,
		Payload:        rec.Payload,
		TimestampMs:    rec.TimestampMs,
		Crc32:          rec.CRC32,
	}
}
