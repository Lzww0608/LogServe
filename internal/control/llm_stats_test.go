package control

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/metadata"
	"google.golang.org/grpc"
)

// llmStatsProbeLogClient detects accidental LLM stream scans during scheduling.
type llmStatsProbeLogClient struct {
	acceptingLogClient
	listStreamsCalls atomic.Int64
}

// ListStreams counts calls so predicted scheduling can assert it uses materialized stats.
func (c *llmStatsProbeLogClient) ListStreams(context.Context, *logservepb.ListStreamsRequest, ...grpc.CallOption) (*logservepb.ListStreamsResponse, error) {
	c.listStreamsCalls.Add(1)
	return &logservepb.ListStreamsResponse{}, nil
}

// llmBootstrapLogClient serves fixed LLM streams for stats bootstrap tests.
type llmBootstrapLogClient struct {
	acceptingLogClient
	records map[string][]*logservepb.LogRecord
}

// ListStreams returns only synthetic LLM streams for llm: prefix scans.
func (c llmBootstrapLogClient) ListStreams(_ context.Context, req *logservepb.ListStreamsRequest, _ ...grpc.CallOption) (*logservepb.ListStreamsResponse, error) {
	if req.GetPrefix() != "llm:" {
		return &logservepb.ListStreamsResponse{}, nil
	}
	streams := make([]string, 0, len(c.records))
	for streamID := range c.records {
		streams = append(streams, streamID)
	}
	return &logservepb.ListStreamsResponse{StreamIds: streams}, nil
}

// ReadLog returns synthetic LLM records when replay starts at the stream base.
func (c llmBootstrapLogClient) ReadLog(_ context.Context, req *logservepb.ReadLogRequest, _ ...grpc.CallOption) (*logservepb.ReadLogResponse, error) {
	records := c.records[req.GetStreamId()]
	if req.GetFromSeq() > 1 {
		return &logservepb.ReadLogResponse{}, nil
	}
	return &logservepb.ReadLogResponse{Records: records}, nil
}

// TestPredictedLatencySchedulerUsesMaterializedStatsWithoutListingLLMStreams ensures
// predicted scheduling does not scan log streams on the hot assignment path.
func TestPredictedLatencySchedulerUsesMaterializedStatsWithoutListingLLMStreams(t *testing.T) {
	meta := metadata.NewMemoryStore()
	logClient := &llmStatsProbeLogClient{}
	service := NewServiceWithResultStore(meta, logClient, nil, 0)

	meta.UpsertWorker(metadata.Worker{
		WorkerID:      "worker-1",
		CachedModels:  map[string]bool{metadata.ModelKey("model-A", "v1"): true},
		Capacity:      1,
		LastHeartbeat: time.Now().UnixMilli(),
	})
	meta.UpsertWorker(metadata.Worker{
		WorkerID:      "worker-2",
		CachedModels:  map[string]bool{},
		Capacity:      1,
		LastHeartbeat: time.Now().UnixMilli(),
	})
	service.syncSchedulerWorkers()

	service.materializeLLMCompleted(llmEventPayload{
		ModelName:      "model-A",
		ModelVersion:   "v1",
		WorkerID:       "worker-1",
		CacheHit:       true,
		ModelLoadMs:    5,
		TotalLatencyMs: 200,
		TimestampMs:    time.Now().UnixMilli(),
	})
	service.materializeLLMCompleted(llmEventPayload{
		ModelName:         "model-A",
		ModelVersion:      "v1",
		WorkerID:          "worker-2",
		CacheHit:          false,
		ModelLoadMs:       1,
		CheckpointFetchMs: 1,
		TotalLatencyMs:    20,
		TimestampMs:       time.Now().UnixMilli(),
	})

	service.configMu.Lock()
	service.schedulingPolicy = logservepb.SchedulingPolicy_SCHEDULING_POLICY_PREDICTED_LATENCY
	service.configMu.Unlock()
	task, _, err := service.enqueueTask(context.Background(), &logservepb.TaskSpec{
		TaskId:          "task-predicted",
		TaskName:        "llm:model-A",
		FunctionName:    "__logserve_llm__",
		LlmModelName:    "model-A",
		LlmModelVersion: "v1",
		ArgsJson:        []byte(`{"args":["hello"],"kwargs":{}}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	cachedPoll, err := service.PollTask(context.Background(), &logservepb.PollTaskRequest{WorkerId: "worker-1"})
	if err != nil {
		t.Fatal(err)
	}
	if cachedPoll.GetHasTask() {
		t.Fatalf("predicted scheduler used cached worker despite slower materialized stats; got task %s", cachedPoll.GetTask().GetTaskId())
	}
	if calls := logClient.listStreamsCalls.Load(); calls != 0 {
		t.Fatalf("predicted scheduler listed llm streams %d times, want materialized O(workers) lookup", calls)
	}
	fastPoll, err := service.PollTask(context.Background(), &logservepb.PollTaskRequest{WorkerId: "worker-2"})
	if err != nil {
		t.Fatal(err)
	}
	if !fastPoll.GetHasTask() || fastPoll.GetTask().GetTaskId() != task.TaskID {
		t.Fatalf("predicted scheduler did not select worker-2; poll=%v task=%s", fastPoll, task.TaskID)
	}
	if calls := logClient.listStreamsCalls.Load(); calls != 0 {
		t.Fatalf("predicted scheduler listed llm streams %d times after fast poll, want 0", calls)
	}
}

// TestBootstrapLLMStatsRebuildsMaterializedStatsFromCompletionEvents verifies LLM
// completion replay rebuilds latency/cache metrics.
func TestBootstrapLLMStatsRebuildsMaterializedStatsFromCompletionEvents(t *testing.T) {
	payload := mustMarshalLLMStatsPayload(t, map[string]any{
		"task_id":             "task-bootstrap",
		"model_name":          "model-A",
		"model_version":       "v1",
		"worker_id":           "worker-2",
		"cache_hit":           false,
		"model_load_ms":       7,
		"checkpoint_fetch_ms": 3,
		"total_latency_ms":    42,
		"timestamp_ms":        int64(1234),
	})
	service := NewServiceWithResultStore(metadata.NewMemoryStore(), llmBootstrapLogClient{
		records: map[string][]*logservepb.LogRecord{
			"llm:task-bootstrap": {
				{Seq: 1, StreamId: "llm:task-bootstrap", EventType: "LLMCompleted", Payload: payload, TimestampMs: 1234},
			},
		},
	}, nil, 0)

	if err := service.bootstrapLLMStats(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats := service.materializedLLMStats("model-A", "v1")["worker-2"]
	if stats.RequestCount != 1 || stats.CacheHitCount != 0 {
		t.Fatalf("request/cache hit counts = %d/%d, want 1/0", stats.RequestCount, stats.CacheHitCount)
	}
	if stats.EWMATotalLatencyMs != 42 || stats.EWMAModelLoadMs != 7 || stats.EWMACheckpointFetchMs != 3 {
		t.Fatalf("ewma stats = total:%d load:%d checkpoint:%d, want 42/7/3",
			stats.EWMATotalLatencyMs, stats.EWMAModelLoadMs, stats.EWMACheckpointFetchMs)
	}
	if stats.LastUpdatedMs != 1234 {
		t.Fatalf("last_updated_ms = %d, want 1234", stats.LastUpdatedMs)
	}
}

// TestBootstrapLLMStatsIsIdempotentWhenCalledMoreThanOnce verifies bootstrap resets
// stats before replay so repeated calls do not double count.
func TestBootstrapLLMStatsIsIdempotentWhenCalledMoreThanOnce(t *testing.T) {
	payload := mustMarshalLLMStatsPayload(t, map[string]any{
		"task_id":          "task-bootstrap-idempotent",
		"model_name":       "model-A",
		"model_version":    "v1",
		"worker_id":        "worker-1",
		"cache_hit":        true,
		"model_load_ms":    5,
		"total_latency_ms": 25,
		"timestamp_ms":     int64(9999),
	})
	service := NewServiceWithResultStore(metadata.NewMemoryStore(), llmBootstrapLogClient{
		records: map[string][]*logservepb.LogRecord{
			"llm:task-bootstrap-idempotent": {
				{Seq: 1, StreamId: "llm:task-bootstrap-idempotent", EventType: "LLMCompleted", Payload: payload, TimestampMs: 9999},
			},
		},
	}, nil, 0)

	if err := service.bootstrapLLMStats(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := service.bootstrapLLMStats(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats := service.materializedLLMStats("model-A", "v1")["worker-1"]
	if stats.RequestCount != 1 || stats.CacheHitCount != 1 {
		t.Fatalf("request/cache counts = %d/%d, want 1/1 after repeated bootstrap", stats.RequestCount, stats.CacheHitCount)
	}
}

// TestCompleteTaskDoesNotDoubleCountLLMStatsForDuplicateCompletion protects the
// terminal-task fast path from folding LLM stats twice.
func TestCompleteTaskDoesNotDoubleCountLLMStatsForDuplicateCompletion(t *testing.T) {
	payload := mustMarshalLLMStatsPayload(t, map[string]any{
		"task_id":          "task-llm-duplicate",
		"model_name":       "model-A",
		"model_version":    "v1",
		"worker_id":        "worker-1",
		"cache_hit":        true,
		"model_load_ms":    5,
		"total_latency_ms": 33,
		"timestamp_ms":     int64(5678),
	})
	meta := metadata.NewMemoryStore()
	_, duplicate := meta.CreateTask(metadata.Task{
		TaskID:          "task-llm-duplicate",
		TaskName:        "llm:model-A",
		Status:          logservepb.TaskStatus_TASK_STATUS_RUNNING,
		WorkerID:        "worker-1",
		LLMModelName:    "model-A",
		LLMModelVersion: "v1",
		TaskLeaseEpoch:  1,
	}, "")
	if duplicate {
		t.Fatal("unexpected duplicate task")
	}
	service := NewServiceWithResultStore(meta, llmBootstrapLogClient{
		records: map[string][]*logservepb.LogRecord{
			"llm:task-llm-duplicate": {
				{Seq: 1, StreamId: "llm:task-llm-duplicate", EventType: "LLMCompleted", Payload: payload, TimestampMs: 5678},
			},
		},
	}, nil, 0)
	req := &logservepb.CompleteTaskRequest{
		TaskId:         "task-llm-duplicate",
		WorkerId:       "worker-1",
		TaskLeaseEpoch: 1,
		Status:         logservepb.TaskStatus_TASK_STATUS_SUCCEEDED,
		ResultJson:     []byte(`"ok"`),
	}
	if _, err := service.CompleteTask(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompleteTask(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	stats := service.materializedLLMStats("model-A", "v1")["worker-1"]
	if stats.RequestCount != 1 {
		t.Fatalf("request_count = %d, want 1 after duplicate CompleteTask", stats.RequestCount)
	}
}

// mustMarshalLLMStatsPayload marshals synthetic LLM event payloads for tests.
func mustMarshalLLMStatsPayload(t *testing.T, value map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
