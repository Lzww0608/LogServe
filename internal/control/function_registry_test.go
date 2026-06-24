package control

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/metadata"
	"github.com/logserve/logserve/internal/objectstore"
	"google.golang.org/grpc"
)

type functionRegistryLogClient struct {
	records map[string][]*logservepb.LogRecord
}

func newFunctionRegistryLogClient() *functionRegistryLogClient {
	return &functionRegistryLogClient{records: make(map[string][]*logservepb.LogRecord)}
}

func (c *functionRegistryLogClient) AppendLog(_ context.Context, req *logservepb.AppendLogRequest, _ ...grpc.CallOption) (*logservepb.AppendLogResponse, error) {
	seq := uint64(len(c.records[req.GetStreamId()]) + 1)
	rec := &logservepb.LogRecord{
		Seq:            seq,
		StreamId:       req.GetStreamId(),
		EventType:      req.GetEventType(),
		IdempotencyKey: req.GetIdempotencyKey(),
		Payload:        append([]byte(nil), req.GetPayload()...),
		TimestampMs:    int64(seq),
	}
	c.records[req.GetStreamId()] = append(c.records[req.GetStreamId()], rec)
	return &logservepb.AppendLogResponse{Seq: seq, TimestampMs: int64(seq)}, nil
}

func (c *functionRegistryLogClient) ReadLog(_ context.Context, req *logservepb.ReadLogRequest, _ ...grpc.CallOption) (*logservepb.ReadLogResponse, error) {
	from := req.GetFromSeq()
	if from == 0 {
		from = 1
	}
	limit := int(req.GetLimit())
	records := c.records[req.GetStreamId()]
	out := make([]*logservepb.LogRecord, 0, len(records))
	for _, rec := range records {
		if rec.GetSeq() < from {
			continue
		}
		out = append(out, rec)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return &logservepb.ReadLogResponse{Records: out}, nil
}

func (c *functionRegistryLogClient) ListStreams(_ context.Context, req *logservepb.ListStreamsRequest, _ ...grpc.CallOption) (*logservepb.ListStreamsResponse, error) {
	streams := make([]string, 0, len(c.records))
	for streamID := range c.records {
		if req.GetPrefix() == "" || len(streamID) >= len(req.GetPrefix()) && streamID[:len(req.GetPrefix())] == req.GetPrefix() {
			streams = append(streams, streamID)
		}
	}
	return &logservepb.ListStreamsResponse{StreamIds: streams}, nil
}

func (c *functionRegistryLogClient) TrimStream(context.Context, *logservepb.TrimStreamRequest, ...grpc.CallOption) (*logservepb.TrimStreamResponse, error) {
	return &logservepb.TrimStreamResponse{}, nil
}

func (c *functionRegistryLogClient) GetStreamStats(context.Context, *logservepb.GetStreamStatsRequest, ...grpc.CallOption) (*logservepb.GetStreamStatsResponse, error) {
	return &logservepb.GetStreamStatsResponse{}, nil
}

func TestSubmitTaskRegistersFunctionAndStoresTaskRef(t *testing.T) {
	ctx := context.Background()
	logClient := newFunctionRegistryLogClient()
	store, err := objectstore.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewServiceWithResultStore(metadata.NewMemoryStore(), logClient, store, 0)
	source := "def add(a, b):\n    return a + b\n"
	hash := hashFunctionSource(source)

	resp, err := service.SubmitTask(ctx, &logservepb.SubmitTaskRequest{
		TaskName:       "add",
		FunctionName:   "add",
		FunctionSource: source,
		FunctionHash:   hash,
		ArgsJson:       []byte(`{"args":[1,2],"kwargs":{}}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	functionRecords := logClient.records[functionRegistryStream]
	if len(functionRecords) != 1 {
		t.Fatalf("function registry records = %d, want 1", len(functionRecords))
	}
	var registered functionRegisteredPayload
	if err := json.Unmarshal(functionRecords[0].GetPayload(), &registered); err != nil {
		t.Fatal(err)
	}
	if registered.FunctionHash != hash || registered.SourceRef == "" || registered.Language != "python" || registered.Entrypoint != "module:add" {
		t.Fatalf("registered payload = %+v", registered)
	}
	stored, err := objectstore.GetBytes(ctx, store, registered.SourceRef, -1)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != source {
		t.Fatalf("stored source = %q, want original", string(stored))
	}

	taskRecords := logClient.records[taskStream(resp.GetTaskId())]
	if len(taskRecords) == 0 {
		t.Fatal("missing TaskSubmitted record")
	}
	spec := taskSpecFromSubmittedRecord(t, taskRecords[0])
	if spec.GetFunctionSource() != "" {
		t.Fatalf("TaskSubmitted function_source = %q, want empty", spec.GetFunctionSource())
	}
	if spec.GetFunctionHash() != hash || spec.GetFunctionRef() != registered.SourceRef {
		t.Fatalf("TaskSubmitted ref/hash = %q/%q, want %q/%q", spec.GetFunctionRef(), spec.GetFunctionHash(), registered.SourceRef, hash)
	}
}

func TestSubmitTaskIdempotencyTreatsInlineAndRegisteredRefAsSameFunction(t *testing.T) {
	ctx := context.Background()
	logClient := newFunctionRegistryLogClient()
	store, err := objectstore.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewServiceWithResultStore(metadata.NewMemoryStore(), logClient, store, 0)
	source := "def add(a, b):\n    return a + b\n"
	hash := hashFunctionSource(source)

	first, err := service.SubmitTask(ctx, &logservepb.SubmitTaskRequest{
		TaskName:       "add",
		FunctionName:   "add",
		FunctionSource: source,
		ArgsJson:       []byte(`{"args":[1,2],"kwargs":{}}`),
		IdempotencyKey: "same-function",
	})
	if err != nil {
		t.Fatal(err)
	}
	service.functionsMu.RLock()
	registered := service.functions[hash]
	service.functionsMu.RUnlock()
	if registered.SourceRef == "" {
		t.Fatal("function was not registered")
	}

	second, err := service.SubmitTask(ctx, &logservepb.SubmitTaskRequest{
		TaskName:       "add",
		FunctionName:   "add",
		FunctionRef:    registered.SourceRef,
		FunctionHash:   hash,
		ArgsJson:       []byte(`{"kwargs":{},"args":[1,2]}`),
		IdempotencyKey: "same-function",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.GetTaskId() != first.GetTaskId() {
		t.Fatalf("duplicate task id = %q, want %q", second.GetTaskId(), first.GetTaskId())
	}
	if got := len(logClient.records[functionRegistryStream]); got != 1 {
		t.Fatalf("function registry records = %d, want 1", got)
	}
}

func TestSubmitTaskRejectsMismatchedFunctionHash(t *testing.T) {
	service := NewServiceWithResultStore(metadata.NewMemoryStore(), newFunctionRegistryLogClient(), nil, 0)
	_, err := service.SubmitTask(context.Background(), &logservepb.SubmitTaskRequest{
		TaskName:       "add",
		FunctionName:   "add",
		FunctionSource: "def add(a, b):\n    return a + b\n",
		FunctionHash:   "sha256:not-the-source",
		ArgsJson:       []byte(`{"args":[1,2],"kwargs":{}}`),
	})
	if err == nil {
		t.Fatal("SubmitTask succeeded with mismatched function_hash")
	}
}

func taskSpecFromSubmittedRecord(t *testing.T, rec *logservepb.LogRecord) *logservepb.TaskSpec {
	t.Helper()
	spec, err := unmarshalTaskSubmittedSpec(rec.GetPayload())
	if err != nil {
		t.Fatal(err)
	}
	return spec
}
