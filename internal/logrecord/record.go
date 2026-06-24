package logrecord

import "github.com/logserve/logserve/gen/logservepb"

type RawRecord struct {
	StreamID       string
	Seq            uint64
	EventType      string
	IdempotencyKey string
	Payload        []byte
	TimestampMs    int64
	CRC32          uint32
}

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
