package control

import (
	"context"
	"io"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/logrecord"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type logClient interface {
	AppendLog(context.Context, *logservepb.AppendLogRequest, ...grpc.CallOption) (*logservepb.AppendLogResponse, error)
	ReadLog(context.Context, *logservepb.ReadLogRequest, ...grpc.CallOption) (*logservepb.ReadLogResponse, error)
	ListStreams(context.Context, *logservepb.ListStreamsRequest, ...grpc.CallOption) (*logservepb.ListStreamsResponse, error)
	TrimStream(context.Context, *logservepb.TrimStreamRequest, ...grpc.CallOption) (*logservepb.TrimStreamResponse, error)
	GetStreamStats(context.Context, *logservepb.GetStreamStatsRequest, ...grpc.CallOption) (*logservepb.GetStreamStatsResponse, error)
}

type rawLogClient interface {
	ReadLogRawEach(context.Context, string, uint64, int, func(logrecord.RawRecord) error) error
}
type streamingLogClient interface {
	ReadLogStream(context.Context, *logservepb.ReadLogRequest, ...grpc.CallOption) (logservepb.LogService_ReadLogStreamClient, error)
}

func (s *Service) forEachLogRecord(ctx context.Context, streamID string, emit func(*logservepb.LogRecord) error) error {
	if emit == nil {
		return nil
	}
	if streamer, ok := s.log.(streamingLogClient); ok {
		stream, err := streamer.ReadLogStream(ctx, &logservepb.ReadLogRequest{StreamId: streamID, FromSeq: 1})
		if err == nil {
			for {
				rec, err := stream.Recv()
				if err == nil {
					if err := emit(rec); err != nil {
						return err
					}
					continue
				}
				if err == io.EOF {
					return nil
				}
				if status.Code(err) == codes.Unimplemented {
					break
				}
				return err
			}
		} else if status.Code(err) != codes.Unimplemented {
			return err
		}
	}
	return s.forEachLogRecordUnary(ctx, streamID, emit)
}

func (s *Service) forEachLogRecordUnary(ctx context.Context, streamID string, emit func(*logservepb.LogRecord) error) error {
	fromSeq := uint64(1)
	for {
		resp, err := s.log.ReadLog(ctx, &logservepb.ReadLogRequest{
			StreamId: streamID,
			FromSeq:  fromSeq,
			Limit:    bootstrapReadLimit,
		})
		if err != nil {
			return err
		}
		records := resp.GetRecords()
		if len(records) == 0 {
			return nil
		}
		for _, rec := range records {
			if err := emit(rec); err != nil {
				return err
			}
		}
		fromSeq = records[len(records)-1].GetSeq() + 1
		if len(records) < bootstrapReadLimit {
			return nil
		}
	}
}

func (s *Service) forEachRawLogRecord(ctx context.Context, streamID string, fromSeq uint64, emit func(logrecord.RawRecord) error) error {
	if emit == nil {
		return nil
	}
	if fromSeq == 0 {
		fromSeq = 1
	}
	if raw, ok := s.log.(rawLogClient); ok {
		return raw.ReadLogRawEach(ctx, streamID, fromSeq, 0, emit)
	}
	if streamer, ok := s.log.(streamingLogClient); ok {
		stream, err := streamer.ReadLogStream(ctx, &logservepb.ReadLogRequest{StreamId: streamID, FromSeq: fromSeq})
		if err == nil {
			for {
				rec, err := stream.Recv()
				if err == nil {
					if err := emit(logrecord.FromProto(rec)); err != nil {
						return err
					}
					continue
				}
				if err == io.EOF {
					return nil
				}
				if status.Code(err) == codes.Unimplemented {
					break
				}
				return err
			}
		} else if status.Code(err) != codes.Unimplemented {
			return err
		}
	}
	return s.forEachRawLogRecordUnary(ctx, streamID, fromSeq, emit)
}

func (s *Service) forEachRawLogRecordUnary(ctx context.Context, streamID string, fromSeq uint64, emit func(logrecord.RawRecord) error) error {
	for {
		resp, err := s.log.ReadLog(ctx, &logservepb.ReadLogRequest{
			StreamId: streamID,
			FromSeq:  fromSeq,
			Limit:    bootstrapReadLimit,
		})
		if err != nil {
			return err
		}
		records := resp.GetRecords()
		if len(records) == 0 {
			return nil
		}
		for _, rec := range records {
			if err := emit(logrecord.FromProto(rec)); err != nil {
				return err
			}
		}
		fromSeq = records[len(records)-1].GetSeq() + 1
		if len(records) < bootstrapReadLimit {
			return nil
		}
	}
}
func (s *Service) readAllLog(ctx context.Context, streamID string) ([]*logservepb.LogRecord, error) {
	out := make([]*logservepb.LogRecord, 0)
	if err := s.forEachLogRecord(ctx, streamID, func(rec *logservepb.LogRecord) error {
		out = append(out, rec)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}
