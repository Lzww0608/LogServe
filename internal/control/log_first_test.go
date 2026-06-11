package control

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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

func (c failingLogClient) TrimStream(context.Context, *logservepb.TrimStreamRequest, ...grpc.CallOption) (*logservepb.TrimStreamResponse, error) {
	return &logservepb.TrimStreamResponse{}, nil
}

func (c failingLogClient) GetStreamStats(context.Context, *logservepb.GetStreamStatsRequest, ...grpc.CallOption) (*logservepb.GetStreamStatsResponse, error) {
	return &logservepb.GetStreamStatsResponse{}, nil
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

func TestWorkflowScheduleBackpressureDoesNotLeavePhantomTaskID(t *testing.T) {
	meta := metadata.NewMemoryStore()
	service := NewServiceWithResultStore(meta, acceptingLogClient{}, nil, 0)
	service.configMu.Lock()
	service.queueHighWatermark = 1
	service.configMu.Unlock()
	service.queue = append(service.queue, "already-queued")

	_, err := service.SubmitWorkflow(context.Background(), &logservepb.SubmitWorkflowRequest{
		WorkflowName:   "backpressured_workflow",
		DefinitionJson: minimalWorkflowDefinition(t),
		IdempotencyKey: "backpressured-workflow",
	})
	if err == nil {
		t.Fatal("SubmitWorkflow succeeded despite queue backpressure")
	}
	if !strings.Contains(err.Error(), "backpressure") {
		t.Fatalf("error = %v, want backpressure", err)
	}
	workflows := meta.ListWorkflows()
	if len(workflows) != 1 {
		t.Fatalf("workflows = %d, want 1", len(workflows))
	}
	step := workflows[0].Steps["finish"]
	if step.TaskID != "" {
		t.Fatalf("workflow step has phantom task_id %q after enqueue failure", step.TaskID)
	}
	if tasks := meta.ListTasks(); len(tasks) != 0 {
		t.Fatalf("metadata tasks = %d, want 0", len(tasks))
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

func TestReplayTaskSpecIgnoresStaleCompletionAfterRedelivery(t *testing.T) {
	spec := &logservepb.TaskSpec{
		TaskId:         "task-stale-replay",
		TaskName:       "stale",
		FunctionName:   "stale",
		FunctionSource: "def stale():\n    return \"ok\"\n",
	}
	submittedPayload, err := marshalTaskSubmittedPayload(spec)
	if err != nil {
		t.Fatal(err)
	}
	startedPayload, _ := json.Marshal(map[string]any{
		"task_lease_epoch": uint64(1),
	})
	redeliveredPayload, _ := json.Marshal(map[string]any{
		"task_lease_epoch": uint64(1),
	})
	completedPayload, _ := json.Marshal(map[string]any{
		"task_lease_epoch": uint64(1),
		"result_json":      json.RawMessage(`"stale"`),
	})

	_, status, _, ok, err := replayTaskSpec([]*logservepb.LogRecord{
		taskRecord("TaskSubmitted", submittedPayload),
		taskRecord("TaskStarted", startedPayload),
		taskRecord("TaskRedelivered", redeliveredPayload),
		taskRecord("TaskCompleted", completedPayload),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("task spec was not replayed")
	}
	if status != logservepb.TaskStatus_TASK_STATUS_QUEUED {
		t.Fatalf("status = %s, want QUEUED after stale completion", status)
	}
}

func taskRecord(eventType string, payload []byte) *logservepb.LogRecord {
	return &logservepb.LogRecord{
		StreamId:    "task:task-stale-replay",
		Seq:         1,
		EventType:   eventType,
		Payload:     payload,
		TimestampMs: time.Now().UnixMilli(),
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
