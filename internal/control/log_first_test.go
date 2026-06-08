package control

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/metadata"
	"google.golang.org/grpc"
)

type failingLogClient struct {
	appendErr error
}

func (c failingLogClient) AppendLog(context.Context, *logservepb.AppendLogRequest, ...grpc.CallOption) (*logservepb.AppendLogResponse, error) {
	return nil, c.appendErr
}

func (c failingLogClient) ReadLog(context.Context, *logservepb.ReadLogRequest, ...grpc.CallOption) (*logservepb.ReadLogResponse, error) {
	return &logservepb.ReadLogResponse{}, nil
}

func (c failingLogClient) ListStreams(context.Context, *logservepb.ListStreamsRequest, ...grpc.CallOption) (*logservepb.ListStreamsResponse, error) {
	return &logservepb.ListStreamsResponse{}, nil
}

func TestSubmitWorkflowAppendFailureDoesNotCreateMetadataOnlyWorkflow(t *testing.T) {
	meta := metadata.NewMemoryStore()
	service := NewServiceWithResultStore(meta, failingLogClient{appendErr: errors.New("append down")}, nil, 0)

	_, err := service.SubmitWorkflow(context.Background(), &logservepb.SubmitWorkflowRequest{
		WorkflowName:   "log_first_workflow",
		DefinitionJson: minimalWorkflowDefinition(t),
	})
	if err == nil {
		t.Fatal("SubmitWorkflow succeeded despite append failure")
	}
	if workflows := meta.ListWorkflows(); len(workflows) != 0 {
		t.Fatalf("metadata-only workflows = %d, want 0", len(workflows))
	}
}

func TestCreateActorAppendFailureDoesNotCreateMetadataOnlyActor(t *testing.T) {
	meta := metadata.NewMemoryStore()
	service := NewServiceWithResultStore(meta, failingLogClient{appendErr: errors.New("append down")}, nil, 0)

	_, err := service.CreateActor(context.Background(), &logservepb.CreateActorRequest{
		ClassName:   "Counter",
		ClassSource: "class Counter:\n    pass\n",
	})
	if err == nil {
		t.Fatal("CreateActor succeeded despite append failure")
	}
	if actors := meta.ListActors(); len(actors) != 0 {
		t.Fatalf("metadata-only actors = %d, want 0", len(actors))
	}
}

func TestRegisterModelAppendFailureDoesNotUpdateRegistry(t *testing.T) {
	meta := metadata.NewMemoryStore()
	service := NewServiceWithResultStore(meta, failingLogClient{appendErr: errors.New("append down")}, nil, 0)

	_, err := service.RegisterModel(context.Background(), &logservepb.RegisterModelRequest{
		Model: &logservepb.ModelInfo{Name: "model-A", Version: "v1", Adapter: "mock"},
	})
	if err == nil {
		t.Fatal("RegisterModel succeeded despite append failure")
	}
	if _, ok := meta.GetModel("model-A", "v1"); ok {
		t.Fatal("model registry changed despite append failure")
	}
}

func TestSetSchedulingPolicyAppendFailureDoesNotChangePolicy(t *testing.T) {
	service := NewServiceWithResultStore(metadata.NewMemoryStore(), failingLogClient{appendErr: errors.New("append down")}, nil, 0)
	before := service.getSchedulingPolicy()

	_, err := service.SetSchedulingPolicy(context.Background(), &logservepb.SetSchedulingPolicyRequest{
		Policy: logservepb.SchedulingPolicy_SCHEDULING_POLICY_RESOURCE_ONLY,
	})
	if err == nil {
		t.Fatal("SetSchedulingPolicy succeeded despite append failure")
	}
	if got := service.getSchedulingPolicy(); got != before {
		t.Fatalf("policy = %s, want unchanged %s", got, before)
	}
}

func TestRegisterWorkerAppendFailureDoesNotUpdateMetadata(t *testing.T) {
	meta := metadata.NewMemoryStore()
	service := NewServiceWithResultStore(meta, failingLogClient{appendErr: errors.New("append down")}, nil, 0)

	_, err := service.RegisterWorker(context.Background(), &logservepb.RegisterWorkerRequest{
		WorkerId: "worker-1",
		Capacity: 1,
	})
	if err == nil {
		t.Fatal("RegisterWorker succeeded despite append failure")
	}
	if _, ok := meta.GetWorker("worker-1"); ok {
		t.Fatal("worker metadata changed despite append failure")
	}
}

func TestSetBackpressureAppendFailureDoesNotChangeConfig(t *testing.T) {
	service := NewServiceWithResultStore(metadata.NewMemoryStore(), failingLogClient{appendErr: errors.New("append down")}, nil, 0)
	beforeWatermark, beforeRedelivery, beforeSlow := service.getBackpressureConfig()

	_, err := service.SetBackpressure(context.Background(), &logservepb.SetBackpressureRequest{
		QueueHighWatermark:  7,
		RedeliveryTimeoutMs: 123,
		LogAppendSlowMs:     4,
	})
	if err == nil {
		t.Fatal("SetBackpressure succeeded despite append failure")
	}
	watermark, redelivery, slow := service.getBackpressureConfig()
	if watermark != beforeWatermark || redelivery != beforeRedelivery || slow != beforeSlow {
		t.Fatalf("backpressure changed to watermark=%d redelivery=%s slow=%s, want watermark=%d redelivery=%s slow=%s",
			watermark, redelivery, slow, beforeWatermark, beforeRedelivery, beforeSlow)
	}
}

func TestRedeliveryAppendFailureDoesNotRequeueTask(t *testing.T) {
	meta := metadata.NewMemoryStore()
	task, duplicate := meta.CreateTask(metadata.Task{
		TaskID:   "task-redeliver",
		TaskName: "redeliver",
		Status:   logservepb.TaskStatus_TASK_STATUS_QUEUED,
	}, "")
	if duplicate {
		t.Fatal("unexpected duplicate task")
	}
	if _, err := meta.LeaseTask(task.TaskID, "worker-1"); err != nil {
		t.Fatal(err)
	}
	service := NewServiceWithResultStore(meta, failingLogClient{appendErr: errors.New("append down")}, nil, 0)
	service.configMu.Lock()
	service.redeliveryTimeout = time.Nanosecond
	service.configMu.Unlock()
	time.Sleep(time.Millisecond)

	err := service.redeliverExpiredTasks(context.Background())
	if err == nil {
		t.Fatal("redelivery succeeded despite append failure")
	}
	current, ok := meta.GetTask(task.TaskID)
	if !ok {
		t.Fatal("task missing")
	}
	if current.Status != logservepb.TaskStatus_TASK_STATUS_RUNNING {
		t.Fatalf("task status = %s, want RUNNING", current.Status)
	}
	if len(service.queue) != 0 {
		t.Fatalf("queue len = %d, want 0", len(service.queue))
	}
}

func minimalWorkflowDefinition(t *testing.T) []byte {
	t.Helper()
	return []byte(`{
		"workflow_name":"log_first_workflow",
		"result_step_id":"finish",
		"steps":[{
			"step_id":"finish",
			"task_name":"finish",
			"function_name":"finish",
			"function_source":"def finish():\n    return \"ok\"\n",
			"args_json":{"args":[],"kwargs":{}},
			"depends_on":[],
			"max_attempts":1,
			"timeout_ms":30000
		}]
	}`)
}
