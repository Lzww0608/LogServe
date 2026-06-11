package control

import (
	"context"
	"strings"
	"testing"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/metadata"
	"google.golang.org/grpc"
)

type acceptingLogClient struct{}

func (c acceptingLogClient) AppendLog(context.Context, *logservepb.AppendLogRequest, ...grpc.CallOption) (*logservepb.AppendLogResponse, error) {
	return &logservepb.AppendLogResponse{Seq: 1, TimestampMs: 1}, nil
}

func (c acceptingLogClient) ReadLog(context.Context, *logservepb.ReadLogRequest, ...grpc.CallOption) (*logservepb.ReadLogResponse, error) {
	return &logservepb.ReadLogResponse{}, nil
}

func (c acceptingLogClient) ListStreams(context.Context, *logservepb.ListStreamsRequest, ...grpc.CallOption) (*logservepb.ListStreamsResponse, error) {
	return &logservepb.ListStreamsResponse{}, nil
}

func (c acceptingLogClient) TrimStream(context.Context, *logservepb.TrimStreamRequest, ...grpc.CallOption) (*logservepb.TrimStreamResponse, error) {
	return &logservepb.TrimStreamResponse{}, nil
}

func (c acceptingLogClient) GetStreamStats(context.Context, *logservepb.GetStreamStatsRequest, ...grpc.CallOption) (*logservepb.GetStreamStatsResponse, error) {
	return &logservepb.GetStreamStatsResponse{}, nil
}

func TestSubmitTaskIdempotencyKeyAllowsSamePayload(t *testing.T) {
	service := NewServiceWithResultStore(metadata.NewMemoryStore(), acceptingLogClient{}, nil, 0)

	first, err := service.SubmitTask(context.Background(), &logservepb.SubmitTaskRequest{
		TaskName:       "add",
		FunctionName:   "add",
		FunctionSource: "def add(a, b):\n    return a + b\n",
		ArgsJson:       []byte(`{"args":[1,2],"kwargs":{}}`),
		IdempotencyKey: "idem-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.SubmitTask(context.Background(), &logservepb.SubmitTaskRequest{
		TaskName:       "add",
		FunctionName:   "add",
		FunctionSource: "def add(a, b):\n    return a + b\n",
		ArgsJson:       []byte(`{"kwargs":{},"args":[1,2]}`),
		IdempotencyKey: "idem-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.GetTaskId() != first.GetTaskId() {
		t.Fatalf("duplicate task id = %q, want %q", second.GetTaskId(), first.GetTaskId())
	}
}

func TestSubmitTaskIdempotencyKeyRejectsDifferentPayload(t *testing.T) {
	service := NewServiceWithResultStore(metadata.NewMemoryStore(), acceptingLogClient{}, nil, 0)

	_, err := service.SubmitTask(context.Background(), &logservepb.SubmitTaskRequest{
		TaskName:       "add",
		FunctionName:   "add",
		FunctionSource: "def add(a, b):\n    return a + b\n",
		ArgsJson:       []byte(`{"args":[1,2],"kwargs":{}}`),
		IdempotencyKey: "idem-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.SubmitTask(context.Background(), &logservepb.SubmitTaskRequest{
		TaskName:       "add",
		FunctionName:   "add",
		FunctionSource: "def add(a, b):\n    return a + b\n",
		ArgsJson:       []byte(`{"args":[2,3],"kwargs":{}}`),
		IdempotencyKey: "idem-task",
	})
	if err == nil {
		t.Fatal("same idempotency key with different payload succeeded")
	}
	if !strings.Contains(err.Error(), "idempotency conflict") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSubmitWorkflowIdempotencyKeyRejectsDifferentDefinition(t *testing.T) {
	service := NewServiceWithResultStore(metadata.NewMemoryStore(), acceptingLogClient{}, nil, 0)

	first := minimalWorkflowDefinition(t)
	second := []byte(strings.Replace(string(first), `return \"ok\"`, `return \"changed\"`, 1))
	_, err := service.SubmitWorkflow(context.Background(), &logservepb.SubmitWorkflowRequest{
		WorkflowName:   "idem_workflow",
		DefinitionJson: first,
		IdempotencyKey: "idem-workflow",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.SubmitWorkflow(context.Background(), &logservepb.SubmitWorkflowRequest{
		WorkflowName:   "idem_workflow",
		DefinitionJson: second,
		IdempotencyKey: "idem-workflow",
	})
	if err == nil {
		t.Fatal("same workflow idempotency key with different definition succeeded")
	}
	if !strings.Contains(err.Error(), "idempotency conflict") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreateActorIdempotencyKeyRejectsDifferentPayload(t *testing.T) {
	service := NewServiceWithResultStore(metadata.NewMemoryStore(), acceptingLogClient{}, nil, 0)

	_, err := service.CreateActor(context.Background(), &logservepb.CreateActorRequest{
		ClassName:      "Counter",
		ClassSource:    "class Counter:\n    pass\n",
		InitArgsJson:   []byte(`{"args":[],"kwargs":{}}`),
		IdempotencyKey: "idem-actor",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.CreateActor(context.Background(), &logservepb.CreateActorRequest{
		ClassName:      "Counter",
		ClassSource:    "class Counter:\n    def get(self):\n        return 1\n",
		InitArgsJson:   []byte(`{"args":[],"kwargs":{}}`),
		IdempotencyKey: "idem-actor",
	})
	if err == nil {
		t.Fatal("same actor idempotency key with different class source succeeded")
	}
	if !strings.Contains(err.Error(), "idempotency conflict") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSubmitLLMIdempotencyKeyRejectsDifferentPrompt(t *testing.T) {
	service := NewServiceWithResultStore(metadata.NewMemoryStore(), acceptingLogClient{}, nil, 0)
	if _, err := service.RegisterModel(context.Background(), &logservepb.RegisterModelRequest{
		Model: &logservepb.ModelInfo{Name: "model-A", Version: "v1", Adapter: "mock"},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := service.SubmitLLM(context.Background(), &logservepb.SubmitLLMRequest{
		ModelName:      "model-A",
		ModelVersion:   "v1",
		Prompt:         "hello",
		IdempotencyKey: "idem-llm",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.SubmitLLM(context.Background(), &logservepb.SubmitLLMRequest{
		ModelName:      "model-A",
		ModelVersion:   "v1",
		Prompt:         "different",
		IdempotencyKey: "idem-llm",
	})
	if err == nil {
		t.Fatal("same llm idempotency key with different prompt succeeded")
	}
	if !strings.Contains(err.Error(), "idempotency conflict") {
		t.Fatalf("unexpected error: %v", err)
	}
}
