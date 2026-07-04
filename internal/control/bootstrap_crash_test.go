package control

import (
	"context"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/metadata"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// replayableLogClient is an in-memory append/read/list log implementation used for
// restart and checkpoint replay tests.
type replayableLogClient struct {
	mu      sync.Mutex
	records map[string][]*logservepb.LogRecord
}

// newReplayableLogClient constructs an empty replayable log client.
func newReplayableLogClient() *replayableLogClient {
	return &replayableLogClient{records: make(map[string][]*logservepb.LogRecord)}
}

// AppendLog stores an immutable log record with the next sequence for the stream.
func (c *replayableLogClient) AppendLog(_ context.Context, req *logservepb.AppendLogRequest, _ ...grpc.CallOption) (*logservepb.AppendLogResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	seq := uint64(1)
	for _, record := range c.records[req.GetStreamId()] {
		if record.GetSeq() >= seq {
			seq = record.GetSeq() + 1
		}
	}
	timestamp := time.Now().UnixMilli()
	record := &logservepb.LogRecord{
		StreamId:       req.GetStreamId(),
		Seq:            seq,
		EventType:      req.GetEventType(),
		IdempotencyKey: req.GetIdempotencyKey(),
		Payload:        append([]byte(nil), req.GetPayload()...),
		TimestampMs:    timestamp,
	}
	c.records[req.GetStreamId()] = append(c.records[req.GetStreamId()], record)
	return &logservepb.AppendLogResponse{Seq: seq, TimestampMs: timestamp}, nil
}

// ReadLog returns cloned records from the requested sequence and optional limit.
func (c *replayableLogClient) ReadLog(_ context.Context, req *logservepb.ReadLogRequest, _ ...grpc.CallOption) (*logservepb.ReadLogResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	limit := int(req.GetLimit())
	out := make([]*logservepb.LogRecord, 0)
	for _, record := range c.records[req.GetStreamId()] {
		if req.GetFromSeq() > 0 && record.GetSeq() < req.GetFromSeq() {
			continue
		}
		out = append(out, proto.Clone(record).(*logservepb.LogRecord))
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return &logservepb.ReadLogResponse{Records: out}, nil
}

// ListStreams returns sorted stream IDs matching an optional prefix.
func (c *replayableLogClient) ListStreams(_ context.Context, req *logservepb.ListStreamsRequest, _ ...grpc.CallOption) (*logservepb.ListStreamsResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	streams := make([]string, 0)
	for streamID := range c.records {
		if req.GetPrefix() == "" || len(streamID) >= len(req.GetPrefix()) && streamID[:len(req.GetPrefix())] == req.GetPrefix() {
			streams = append(streams, streamID)
		}
	}
	sort.Strings(streams)
	return &logservepb.ListStreamsResponse{StreamIds: streams}, nil
}

// TrimStream drops records before the requested sequence and reports compactable count.
func (c *replayableLogClient) TrimStream(_ context.Context, req *logservepb.TrimStreamRequest, _ ...grpc.CallOption) (*logservepb.TrimStreamResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	records := c.records[req.GetStreamId()]
	kept := records[:0]
	var compactable uint64
	for _, record := range records {
		if record.GetSeq() < req.GetBeforeSeq() {
			compactable++
			continue
		}
		kept = append(kept, record)
	}
	c.records[req.GetStreamId()] = kept
	return &logservepb.TrimStreamResponse{
		StreamId:           req.GetStreamId(),
		TrimmedBeforeSeq:   req.GetBeforeSeq(),
		CompactableRecords: compactable,
	}, nil
}

// GetStreamStats reports first and next sequence for matching in-memory streams.
func (c *replayableLogClient) GetStreamStats(_ context.Context, req *logservepb.GetStreamStatsRequest, _ ...grpc.CallOption) (*logservepb.GetStreamStatsResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*logservepb.StreamStats, 0)
	for streamID, records := range c.records {
		if req.GetStreamId() != "" && streamID != req.GetStreamId() {
			continue
		}
		if req.GetPrefix() != "" && (len(streamID) < len(req.GetPrefix()) || streamID[:len(req.GetPrefix())] != req.GetPrefix()) {
			continue
		}
		nextSeq := uint64(1)
		firstSeq := uint64(1)
		if len(records) > 0 {
			firstSeq = records[0].GetSeq()
			nextSeq = records[len(records)-1].GetSeq() + 1
		}
		out = append(out, &logservepb.StreamStats{StreamId: streamID, FirstSeq: firstSeq, NextSeq: nextSeq})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetStreamId() < out[j].GetStreamId() })
	return &logservepb.GetStreamStatsResponse{Streams: out}, nil
}

// TestControlRestartBootstrapsTaskAfterMetadataWriteLoss proves task submission is
// recoverable from the log even if metadata is lost after append.
func TestControlRestartBootstrapsTaskAfterMetadataWriteLoss(t *testing.T) {
	logClient := newReplayableLogClient()
	first := NewServiceWithResultStore(metadata.NewMemoryStore(), logClient, nil, 0)
	resp, err := first.SubmitTask(context.Background(), &logservepb.SubmitTaskRequest{
		TaskName:       "crash_safe_task",
		FunctionName:   "run",
		FunctionSource: "def run():\n    return 'ok'\n",
		ArgsJson:       []byte(`{"args":[],"kwargs":{}}`),
		IdempotencyKey: "crash-safe-task",
	})
	if err != nil {
		t.Fatal(err)
	}

	restartedMeta := metadata.NewMemoryStore()
	restarted := NewServiceWithResultStore(restartedMeta, logClient, nil, 0)
	if err := restarted.BootstrapFromLog(context.Background()); err != nil {
		t.Fatal(err)
	}
	task, ok := restartedMeta.GetTask(resp.GetTaskId())
	if !ok {
		t.Fatalf("task %s was not rebuilt from log", resp.GetTaskId())
	}
	if task.Status != logservepb.TaskStatus_TASK_STATUS_QUEUED {
		t.Fatalf("task status = %s, want QUEUED", task.Status)
	}
	if task.IdempotencyKey != "crash-safe-task" {
		t.Fatalf("idempotency key = %q, want crash-safe-task", task.IdempotencyKey)
	}
}
