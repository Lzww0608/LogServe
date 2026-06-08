package logstore

import (
	"context"

	"github.com/logserve/logserve/gen/logservepb"
)

type Service struct {
	logservepb.UnimplementedLogServiceServer
	store *Store
}

func NewService(store *Store) *Service {
	return &Service{store: store}
}

func (s *Service) AppendLog(ctx context.Context, req *logservepb.AppendLogRequest) (*logservepb.AppendLogResponse, error) {
	rec, duplicate, err := s.store.Append(AppendRequest{
		StreamID:       req.GetStreamId(),
		EventType:      req.GetEventType(),
		IdempotencyKey: req.GetIdempotencyKey(),
		Payload:        req.GetPayload(),
	})
	if err != nil {
		return nil, err
	}
	return &logservepb.AppendLogResponse{
		Seq:         rec.Seq,
		TimestampMs: rec.TimestampMs,
		Crc32:       rec.CRC32,
		Duplicate:   duplicate,
	}, nil
}

func (s *Service) ReadLog(ctx context.Context, req *logservepb.ReadLogRequest) (*logservepb.ReadLogResponse, error) {
	records, err := s.store.Read(req.GetStreamId(), req.GetFromSeq(), int(req.GetLimit()))
	if err != nil {
		return nil, err
	}
	out := make([]*logservepb.LogRecord, 0, len(records))
	for _, rec := range records {
		out = append(out, &logservepb.LogRecord{
			StreamId:       rec.StreamID,
			Seq:            rec.Seq,
			EventType:      rec.EventType,
			IdempotencyKey: rec.IdempotencyKey,
			Payload:        rec.Payload,
			TimestampMs:    rec.TimestampMs,
			Crc32:          rec.CRC32,
		})
	}
	return &logservepb.ReadLogResponse{Records: out}, nil
}

func (s *Service) ListStreams(ctx context.Context, req *logservepb.ListStreamsRequest) (*logservepb.ListStreamsResponse, error) {
	return &logservepb.ListStreamsResponse{StreamIds: s.store.ListStreams(req.GetPrefix())}, nil
}
