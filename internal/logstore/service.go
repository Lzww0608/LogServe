package logstore

import (
	"context"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/logrecord"
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
		out = append(out, logRecordFromStoreRecord(rec))
	}
	return &logservepb.ReadLogResponse{Records: out}, nil
}

func (s *Service) ReadLogStream(req *logservepb.ReadLogRequest, stream logservepb.LogService_ReadLogStreamServer) error {
	return s.store.ReadRawEach(req.GetStreamId(), req.GetFromSeq(), int(req.GetLimit()), func(rec logrecord.RawRecord) error {
		if err := stream.Context().Err(); err != nil {
			return err
		}
		return stream.Send(rawRecordToProto(rec))
	})
}

func rawRecordToProto(rec logrecord.RawRecord) *logservepb.LogRecord {
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

func (s *Service) ReadLogRawEach(ctx context.Context, streamID string, fromSeq uint64, limit int, emit func(logrecord.RawRecord) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.store.ReadRawEach(streamID, fromSeq, limit, func(rec logrecord.RawRecord) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return emit(rec)
	})
}
func logRecordFromStoreRecord(rec Record) *logservepb.LogRecord {
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

func (s *Service) ListStreams(ctx context.Context, req *logservepb.ListStreamsRequest) (*logservepb.ListStreamsResponse, error) {
	return &logservepb.ListStreamsResponse{StreamIds: s.store.ListStreams(req.GetPrefix())}, nil
}

func (s *Service) TrimStream(ctx context.Context, req *logservepb.TrimStreamRequest) (*logservepb.TrimStreamResponse, error) {
	stats, err := s.store.Trim(req.GetStreamId(), req.GetBeforeSeq())
	if err != nil {
		return nil, err
	}
	return &logservepb.TrimStreamResponse{
		StreamId:           stats.StreamID,
		TrimmedBeforeSeq:   stats.TrimmedBeforeSeq,
		CompactableRecords: stats.CompactableRecords,
		CompactableBytes:   stats.CompactableBytes,
	}, nil
}

func (s *Service) GetStreamStats(ctx context.Context, req *logservepb.GetStreamStatsRequest) (*logservepb.GetStreamStatsResponse, error) {
	stats := s.store.Stats(req.GetStreamId(), req.GetPrefix())
	out := make([]*logservepb.StreamStats, 0, len(stats))
	for _, item := range stats {
		out = append(out, &logservepb.StreamStats{
			StreamId:           item.StreamID,
			FirstSeq:           item.FirstSeq,
			NextSeq:            item.NextSeq,
			TrimmedBeforeSeq:   item.TrimmedBeforeSeq,
			CompactableRecords: item.CompactableRecords,
			CompactableBytes:   item.CompactableBytes,
		})
	}
	return &logservepb.GetStreamStatsResponse{Streams: out}, nil
}
