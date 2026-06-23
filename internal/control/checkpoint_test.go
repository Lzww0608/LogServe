package control

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	actorpkg "github.com/logserve/logserve/internal/actor"
	"github.com/logserve/logserve/internal/metadata"
	workflowpkg "github.com/logserve/logserve/internal/workflow"
	"google.golang.org/grpc"
)

type countingReplayableLogClient struct {
	*replayableLogClient
	mu          sync.Mutex
	readCalls   map[string]int
	recordsRead int
}

func newCountingReplayableLogClient() *countingReplayableLogClient {
	return &countingReplayableLogClient{
		replayableLogClient: newReplayableLogClient(),
		readCalls:           make(map[string]int),
	}
}

func (c *countingReplayableLogClient) ReadLog(ctx context.Context, req *logservepb.ReadLogRequest, opts ...grpc.CallOption) (*logservepb.ReadLogResponse, error) {
	c.mu.Lock()
	c.readCalls[fmt.Sprintf("%s:%d", req.GetStreamId(), req.GetFromSeq())]++
	c.mu.Unlock()
	resp, err := c.replayableLogClient.ReadLog(ctx, req, opts...)
	if err == nil {
		c.mu.Lock()
		c.recordsRead += len(resp.GetRecords())
		c.mu.Unlock()
	}
	return resp, err
}

func (c *countingReplayableLogClient) readCount(streamID string, fromSeq uint64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readCalls[fmt.Sprintf("%s:%d", streamID, fromSeq)]
}

func (c *countingReplayableLogClient) resetReadCounts() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readCalls = make(map[string]int)
	c.recordsRead = 0
}

func (c *countingReplayableLogClient) totalReadCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for _, count := range c.readCalls {
		total += count
	}
	return total
}

func (c *countingReplayableLogClient) totalRecordsRead() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.recordsRead
}

func (c *countingReplayableLogClient) readCountForStreamsFromSeq(streams map[string]MetadataCheckpointStream, fromSeq uint64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for streamID := range streams {
		total += c.readCalls[fmt.Sprintf("%s:%d", streamID, fromSeq)]
	}
	return total
}

func TestBootstrapLLMStatsUsesCheckpointAndOnlyReadsTail(t *testing.T) {
	ctx := context.Background()
	logClient := newCountingReplayableLogClient()
	first := NewServiceWithResultStore(metadata.NewMemoryStore(), logClient, nil, 0)

	appendLLMCompleted(t, logClient, "task-llm-checkpoint", llmEventPayload{
		TaskID:         "task-llm-checkpoint",
		ModelName:      "model-A",
		ModelVersion:   "v1",
		WorkerID:       "worker-1",
		CacheHit:       true,
		ModelLoadMs:    5,
		TotalLatencyMs: 100,
		TimestampMs:    1000,
	})
	if err := first.bootstrapLLMStats(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := first.CreateMetadataCheckpoint(ctx, 3); err != nil {
		t.Fatal(err)
	}
	logClient.resetReadCounts()

	appendLLMCompleted(t, logClient, "task-llm-checkpoint", llmEventPayload{
		TaskID:         "task-llm-checkpoint",
		ModelName:      "model-A",
		ModelVersion:   "v1",
		WorkerID:       "worker-1",
		CacheHit:       false,
		ModelLoadMs:    15,
		TotalLatencyMs: 40,
		TimestampMs:    2000,
	})

	restarted := NewServiceWithResultStore(metadata.NewMemoryStore(), logClient, nil, 0)
	if err := restarted.BootstrapFromLog(ctx); err != nil {
		t.Fatal(err)
	}
	stats := restarted.materializedLLMStats("model-A", "v1")["worker-1"]
	if stats.RequestCount != 2 {
		t.Fatalf("request_count = %d, want checkpointed sample plus tail sample", stats.RequestCount)
	}
	if stats.EWMATotalLatencyMs != 82 {
		t.Fatalf("ewma_total_latency_ms = %d, want 82", stats.EWMATotalLatencyMs)
	}
	if calls := logClient.readCount("llm:task-llm-checkpoint", 1); calls != 0 {
		t.Fatalf("bootstrap read checkpointed llm stream from seq 1 %d times, want tail-only read", calls)
	}
	if calls := logClient.readCount("llm:task-llm-checkpoint", 2); calls == 0 {
		t.Fatal("bootstrap did not read llm tail from checkpoint last_seq+1")
	}
}

func TestMetadataCheckpointConsistencyUsesCheckpointTailOrderForLLMStats(t *testing.T) {
	ctx := context.Background()
	logClient := newCountingReplayableLogClient()
	first := NewServiceWithResultStore(metadata.NewMemoryStore(), logClient, nil, 0)

	for i, latency := range []int64{10, 20, 30, 40} {
		taskID := fmt.Sprintf("task-llm-order-%02d", i)
		appendLLMCompleted(t, logClient, taskID, llmEventPayload{
			TaskID:         taskID,
			ModelName:      "model-A",
			ModelVersion:   "v1",
			WorkerID:       "worker-1",
			ModelLoadMs:    latency / 10,
			TotalLatencyMs: latency,
			TimestampMs:    int64(1000 + i),
		})
	}
	if err := first.bootstrapLLMStats(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := first.CreateMetadataCheckpoint(ctx, 3); err != nil {
		t.Fatal(err)
	}
	appendLLMCompleted(t, logClient, "task-llm-order-02", llmEventPayload{
		TaskID:         "task-llm-order-02",
		ModelName:      "model-A",
		ModelVersion:   "v1",
		WorkerID:       "worker-1",
		ModelLoadMs:    10,
		TotalLatencyMs: 100,
		TimestampMs:    2000,
	})
	appendLLMCompleted(t, logClient, "task-llm-order-03", llmEventPayload{
		TaskID:         "task-llm-order-03",
		ModelName:      "model-A",
		ModelVersion:   "v1",
		WorkerID:       "worker-1",
		ModelLoadMs:    20,
		TotalLatencyMs: 200,
		TimestampMs:    2001,
	})

	check, err := first.CheckMetadataCheckpointConsistency(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !check.Consistent {
		t.Fatalf("checkpoint consistency failed for order-sensitive LLM stats: %+v", check)
	}
}

func TestBootstrapFromMetadataCheckpointRestoresTaskTerminalTail(t *testing.T) {
	ctx := context.Background()
	logClient := newCountingReplayableLogClient()
	firstMeta := metadata.NewMemoryStore()
	first := NewServiceWithResultStore(firstMeta, logClient, nil, 0)

	submitted, err := first.SubmitTask(ctx, &logservepb.SubmitTaskRequest{
		TaskName:       "checkpoint_task",
		FunctionName:   "run",
		FunctionSource: "def run():\n    return 'ok'\n",
		ArgsJson:       []byte(`{"args":[],"kwargs":{}}`),
		IdempotencyKey: "checkpoint-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.CreateMetadataCheckpoint(ctx, 3); err != nil {
		t.Fatal(err)
	}
	logClient.resetReadCounts()
	if _, err := firstMeta.LeaseTask(submitted.GetTaskId(), "worker-1"); err != nil {
		t.Fatal(err)
	}
	task, ok := firstMeta.GetTask(submitted.GetTaskId())
	if !ok {
		t.Fatal("task missing after lease")
	}
	if err := first.appendTaskStarted(ctx, task, "worker-1"); err != nil {
		t.Fatal(err)
	}
	if err := first.appendTaskTerminal(ctx, task, logservepb.TaskStatus_TASK_STATUS_SUCCEEDED, []byte(`"ok"`), ""); err != nil {
		t.Fatal(err)
	}

	restartedMeta := metadata.NewMemoryStore()
	restarted := NewServiceWithResultStore(restartedMeta, logClient, nil, 0)
	if err := restarted.BootstrapFromLog(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, ok := restartedMeta.GetTask(submitted.GetTaskId())
	if !ok {
		t.Fatal("task was not restored from checkpoint plus tail")
	}
	if recovered.Status != logservepb.TaskStatus_TASK_STATUS_SUCCEEDED {
		t.Fatalf("status = %s, want SUCCEEDED", recovered.Status)
	}
	if string(recovered.ResultJSON) != `"ok"` {
		t.Fatalf("result_json = %s, want \"ok\"", recovered.ResultJSON)
	}
	if calls := logClient.readCount("task:"+submitted.GetTaskId(), 1); calls != 0 {
		t.Fatalf("bootstrap read checkpointed task stream from seq 1 %d times, want tail-only read", calls)
	}
}

func TestBootstrapFallsBackToFullReplayWhenCheckpointPayloadIsCorrupt(t *testing.T) {
	ctx := context.Background()
	logClient := newCountingReplayableLogClient()
	first := NewServiceWithResultStore(metadata.NewMemoryStore(), logClient, nil, 0)
	submitted, err := first.SubmitTask(ctx, &logservepb.SubmitTaskRequest{
		TaskName:       "corrupt_checkpoint_task",
		FunctionName:   "run",
		FunctionSource: "def run():\n    return 'ok'\n",
		ArgsJson:       []byte(`{"args":[],"kwargs":{}}`),
		IdempotencyKey: "corrupt-checkpoint-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := logClient.AppendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       metadataCheckpointStream,
		EventType:      metadataCheckpointEvent,
		IdempotencyKey: "corrupt-checkpoint",
		Payload:        []byte(`{"checkpoint_id":`),
	}); err != nil {
		t.Fatal(err)
	}

	restartedMeta := metadata.NewMemoryStore()
	restarted := NewServiceWithResultStore(restartedMeta, logClient, nil, 0)
	if err := restarted.BootstrapFromLog(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := restartedMeta.GetTask(submitted.GetTaskId()); !ok {
		t.Fatal("task was not restored by full replay after corrupt checkpoint")
	}
}
func TestMetadataCheckpointLoopCreatesRetainedCheckpoints(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logClient := newCountingReplayableLogClient()
	service := NewServiceWithResultStore(metadata.NewMemoryStore(), logClient, nil, 0)

	stop := service.StartMetadataCheckpointLoop(ctx, time.Millisecond, 2)
	defer stop()

	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		resp, err := logClient.ReadLog(ctx, &logservepb.ReadLogRequest{StreamId: metadataCheckpointStream, FromSeq: 1, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.GetRecords()) == 2 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	resp, err := logClient.ReadLog(ctx, &logservepb.ReadLogRequest{StreamId: metadataCheckpointStream, FromSeq: 1, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("checkpoint loop retained %d records, want 2", len(resp.GetRecords()))
}
func TestCreateMetadataCheckpointAppliesRetention(t *testing.T) {
	ctx := context.Background()
	logClient := newCountingReplayableLogClient()
	service := NewServiceWithResultStore(metadata.NewMemoryStore(), logClient, nil, 0)

	for i := 0; i < 4; i++ {
		if _, err := service.CreateMetadataCheckpoint(ctx, 2); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := logClient.ReadLog(ctx, &logservepb.ReadLogRequest{StreamId: metadataCheckpointStream, FromSeq: 1, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetRecords()) != 2 {
		t.Fatalf("checkpoint records after retention = %d, want 2", len(resp.GetRecords()))
	}
	if resp.GetRecords()[0].GetSeq() != 3 || resp.GetRecords()[1].GetSeq() != 4 {
		t.Fatalf("retained checkpoint seqs = %d/%d, want 3/4", resp.GetRecords()[0].GetSeq(), resp.GetRecords()[1].GetSeq())
	}
}

func TestMetadataCheckpointConsistencyComparesCheckpointTailWithFullReplay(t *testing.T) {
	ctx := context.Background()
	logClient := newCountingReplayableLogClient()
	firstMeta := metadata.NewMemoryStore()
	first := NewServiceWithResultStore(firstMeta, logClient, nil, 0)

	submitted, err := first.SubmitTask(ctx, &logservepb.SubmitTaskRequest{
		TaskName:       "consistency_task",
		FunctionName:   "run",
		FunctionSource: "def run():\n    return 'ok'\n",
		ArgsJson:       []byte(`{"args":[],"kwargs":{}}`),
		IdempotencyKey: "consistency-task",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.CreateMetadataCheckpoint(ctx, 3); err != nil {
		t.Fatal(err)
	}
	task, ok := firstMeta.GetTask(submitted.GetTaskId())
	if !ok {
		t.Fatal("task missing")
	}
	task.Status = logservepb.TaskStatus_TASK_STATUS_RUNNING
	task.WorkerID = "worker-1"
	task.TaskLeaseEpoch = 1
	if err := first.appendTaskStarted(ctx, task, "worker-1"); err != nil {
		t.Fatal(err)
	}
	if err := first.appendTaskTerminal(ctx, task, logservepb.TaskStatus_TASK_STATUS_SUCCEEDED, []byte(`"ok"`), ""); err != nil {
		t.Fatal(err)
	}

	check, err := first.CheckMetadataCheckpointConsistency(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !check.Consistent {
		t.Fatalf("checkpoint consistency failed: %+v", check)
	}
	if check.CheckedCount == 0 || check.CheckpointID == "" {
		t.Fatalf("consistency check did not inspect checkpoint state: %+v", check)
	}
}
func appendLLMCompleted(t *testing.T, logClient *countingReplayableLogClient, taskID string, payload llmEventPayload) {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := logClient.AppendLog(context.Background(), &logservepb.AppendLogRequest{
		StreamId:       llmStream(taskID),
		EventType:      "LLMCompleted",
		IdempotencyKey: fmt.Sprintf("%s:completed:%d", taskID, payload.TimestampMs),
		Payload:        data,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapFromMetadataCheckpointRestoresWorkflowAndActorTail(t *testing.T) {
	ctx := context.Background()
	logClient := newCountingReplayableLogClient()
	firstMeta := metadata.NewMemoryStore()
	first := NewServiceWithResultStore(firstMeta, logClient, nil, 0)

	workflowResp, err := first.SubmitWorkflow(ctx, &logservepb.SubmitWorkflowRequest{
		WorkflowName:   "checkpoint_workflow",
		DefinitionJson: minimalWorkflowDefinition(t),
		IdempotencyKey: "checkpoint-workflow",
	})
	if err != nil {
		t.Fatal(err)
	}
	wfState, ok := firstMeta.GetWorkflow(workflowResp.GetWorkflowId())
	if !ok {
		t.Fatal("workflow missing before checkpoint")
	}
	step := wfState.Steps["finish"]

	actorResp, err := first.CreateActor(ctx, &logservepb.CreateActorRequest{
		ClassName:      "Counter",
		ClassSource:    "class Counter:\n    pass\n",
		InitArgsJson:   []byte(`{"args":[],"kwargs":{}}`),
		IdempotencyKey: "checkpoint-actor",
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := first.CreateMetadataCheckpoint(ctx, 3); err != nil {
		t.Fatal(err)
	}
	logClient.resetReadCounts()

	appendWorkflowEvent(t, logClient, workflowResp.GetWorkflowId(), "StepStarted", workflowpkg.EventPayload{
		WorkflowID:  workflowResp.GetWorkflowId(),
		StepID:      "finish",
		TaskID:      step.TaskID,
		TimestampMs: 3000,
	})
	appendWorkflowEvent(t, logClient, workflowResp.GetWorkflowId(), "StepSucceeded", workflowpkg.EventPayload{
		WorkflowID:  workflowResp.GetWorkflowId(),
		StepID:      "finish",
		TaskID:      step.TaskID,
		ResultJSON:  []byte(`"ok"`),
		TimestampMs: 3100,
		LatencyMs:   100,
	})
	appendWorkflowEvent(t, logClient, workflowResp.GetWorkflowId(), "WorkflowCompleted", workflowpkg.EventPayload{
		WorkflowID:  workflowResp.GetWorkflowId(),
		ResultJSON:  []byte(`"ok"`),
		TimestampMs: 3200,
		LatencyMs:   200,
	})
	appendActorEvent(t, logClient, actorResp.GetActorId(), "ActorCommandApplied", actorpkg.EventPayload{
		ActorID:      actorResp.GetActorId(),
		CallID:       "call-1",
		CommandSeq:   1,
		CommandCount: 1,
		WorkerID:     "worker-1",
		Epoch:        1,
		StateJSON:    []byte(`{"n":1}`),
		TimestampMs:  3300,
	})

	restartedMeta := metadata.NewMemoryStore()
	restarted := NewServiceWithResultStore(restartedMeta, logClient, nil, 0)
	if err := restarted.BootstrapFromLog(ctx); err != nil {
		t.Fatal(err)
	}
	recoveredWorkflow, ok := restartedMeta.GetWorkflow(workflowResp.GetWorkflowId())
	if !ok {
		t.Fatal("workflow was not restored")
	}
	if recoveredWorkflow.Status != logservepb.WorkflowStatus_WORKFLOW_STATUS_COMPLETED {
		t.Fatalf("workflow status = %s, want COMPLETED", recoveredWorkflow.Status)
	}
	if string(recoveredWorkflow.ResultJSON) != `"ok"` {
		t.Fatalf("workflow result_json = %s, want ok", recoveredWorkflow.ResultJSON)
	}
	recoveredActor, ok := restartedMeta.GetActor(actorResp.GetActorId())
	if !ok {
		t.Fatal("actor was not restored")
	}
	if recoveredActor.CommandCount != 1 || recoveredActor.OwnerWorkerID != "worker-1" || recoveredActor.Epoch != 1 {
		t.Fatalf("actor command/owner/epoch = %d/%s/%d, want 1/worker-1/1", recoveredActor.CommandCount, recoveredActor.OwnerWorkerID, recoveredActor.Epoch)
	}
	if string(actorpkg.NormalizeJSON(recoveredActor.StateJSON)) != `{"n":1}` {
		t.Fatalf("actor state_json = %s, want {n:1}", recoveredActor.StateJSON)
	}
	if calls := logClient.readCount(workflowStream(workflowResp.GetWorkflowId()), 1); calls != 0 {
		t.Fatalf("bootstrap read checkpointed workflow stream from seq 1 %d times, want tail-only read", calls)
	}
	if calls := logClient.readCount(actorStream(actorResp.GetActorId()), 1); calls != 0 {
		t.Fatalf("bootstrap read checkpointed actor stream from seq 1 %d times, want tail-only read", calls)
	}
}

func appendWorkflowEvent(t *testing.T, logClient *countingReplayableLogClient, workflowID, eventType string, payload workflowpkg.EventPayload) {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := logClient.AppendLog(context.Background(), &logservepb.AppendLogRequest{
		StreamId:       workflowStream(workflowID),
		EventType:      eventType,
		IdempotencyKey: fmt.Sprintf("%s:%s:%d", workflowID, eventType, payload.TimestampMs),
		Payload:        data,
	}); err != nil {
		t.Fatal(err)
	}
}

func appendActorEvent(t *testing.T, logClient *countingReplayableLogClient, actorID, eventType string, payload actorpkg.EventPayload) {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := logClient.AppendLog(context.Background(), &logservepb.AppendLogRequest{
		StreamId:       actorStream(actorID),
		EventType:      eventType,
		IdempotencyKey: fmt.Sprintf("%s:%s:%d", actorID, eventType, payload.TimestampMs),
		Payload:        data,
	}); err != nil {
		t.Fatal(err)
	}
}
