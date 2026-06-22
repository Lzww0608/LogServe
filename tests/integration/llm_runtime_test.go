package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/worker"
)

func TestLLMLocalityAwareSchedulerPrefersCachedWorker(t *testing.T) {
	resourceEnv := startWorkflowEnv(t)
	defer resourceEnv.stop()
	registerLLMWorkers(t, resourceEnv.controlClient)
	registerTestModels(t, resourceEnv.controlClient)
	setPolicy(t, resourceEnv.controlClient, logservepb.SchedulingPolicy_SCHEDULING_POLICY_RESOURCE_ONLY)

	resourceTask := submitLLMForTest(t, resourceEnv.controlClient, "model-A", "hello")
	resourcePoll, err := resourceEnv.controlClient.PollTask(context.Background(), &logservepb.PollTaskRequest{WorkerId: "worker-2"})
	if err != nil {
		t.Fatal(err)
	}
	if !resourcePoll.GetHasTask() || resourcePoll.GetTask().GetTaskId() != resourceTask.GetTaskId() {
		t.Fatalf("resource-only should assign first idle poller; poll=%v task=%s", resourcePoll, resourceTask.GetTaskId())
	}

	localityEnv := startWorkflowEnv(t)
	defer localityEnv.stop()
	registerLLMWorkers(t, localityEnv.controlClient)
	registerTestModels(t, localityEnv.controlClient)
	setPolicy(t, localityEnv.controlClient, logservepb.SchedulingPolicy_SCHEDULING_POLICY_LOCALITY_AWARE)

	localityTask := submitLLMForTest(t, localityEnv.controlClient, "model-A", "hello")
	coldPoll, err := localityEnv.controlClient.PollTask(context.Background(), &logservepb.PollTaskRequest{WorkerId: "worker-2"})
	if err != nil {
		t.Fatal(err)
	}
	if coldPoll.GetHasTask() {
		t.Fatalf("locality-aware should wait for cached worker; worker-2 got %s", coldPoll.GetTask().GetTaskId())
	}
	cachedPoll, err := localityEnv.controlClient.PollTask(context.Background(), &logservepb.PollTaskRequest{WorkerId: "worker-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !cachedPoll.GetHasTask() || cachedPoll.GetTask().GetTaskId() != localityTask.GetTaskId() {
		t.Fatalf("locality-aware should assign cached worker; poll=%v task=%s", cachedPoll, localityTask.GetTaskId())
	}
	if cachedPoll.GetTask().GetLlmModelName() != "model-A" {
		t.Fatalf("llm model on task = %q", cachedPoll.GetTask().GetLlmModelName())
	}
}

func TestLLMPredictedLatencySchedulerUsesObservedHistory(t *testing.T) {
	env := startWorkflowEnv(t)
	defer env.stop()
	registerLLMWorkers(t, env.controlClient)
	registerTestModels(t, env.controlClient)
	setPolicy(t, env.controlClient, logservepb.SchedulingPolicy_SCHEDULING_POLICY_RESOURCE_ONLY)
	recordLLMObservation(t, env, "worker-1", "model-A", "v1", true, 200, 5, 0)
	recordLLMObservation(t, env, "worker-2", "model-A", "v1", false, 20, 1, 1)
	setPolicy(t, env.controlClient, logservepb.SchedulingPolicy_SCHEDULING_POLICY_PREDICTED_LATENCY)

	predictedTask := submitLLMForTest(t, env.controlClient, "model-A", "hello")
	cachedPoll, err := env.controlClient.PollTask(context.Background(), &logservepb.PollTaskRequest{WorkerId: "worker-1"})
	if err != nil {
		t.Fatal(err)
	}
	if cachedPoll.GetHasTask() {
		t.Fatalf("predicted-latency should wait for historically faster worker-2; worker-1 got %s", cachedPoll.GetTask().GetTaskId())
	}
	fastPoll, err := env.controlClient.PollTask(context.Background(), &logservepb.PollTaskRequest{WorkerId: "worker-2"})
	if err != nil {
		t.Fatal(err)
	}
	if !fastPoll.GetHasTask() || fastPoll.GetTask().GetTaskId() != predictedTask.GetTaskId() {
		t.Fatalf("predicted-latency should assign worker-2; poll=%v task=%s", fastPoll, predictedTask.GetTaskId())
	}
}

func TestLLMSubmitRequiresRegisteredModel(t *testing.T) {
	env := startWorkflowEnv(t)
	defer env.stop()

	_, err := env.controlClient.SubmitLLM(context.Background(), &logservepb.SubmitLLMRequest{
		ModelName:    "missing-model",
		ModelVersion: "v1",
		Prompt:       "hello",
		Adapter:      "mock",
	})
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("SubmitLLM error = %v, want model registry rejection", err)
	}
}

func recordLLMObservation(t *testing.T, env *workflowTestEnv, workerID, modelName, modelVersion string, cacheHit bool, totalLatencyMs, modelLoadMs, checkpointFetchMs int64) {
	t.Helper()
	submitted := submitLLMForTest(t, env.controlClient, modelName, "history")
	poll, err := env.controlClient.PollTask(context.Background(), &logservepb.PollTaskRequest{WorkerId: workerID})
	if err != nil {
		t.Fatal(err)
	}
	if !poll.GetHasTask() || poll.GetTask().GetTaskId() != submitted.GetTaskId() {
		t.Fatalf("history observation poll by %s = %v, want task %s", workerID, poll, submitted.GetTaskId())
	}
	taskID := poll.GetTask().GetTaskId()
	payload, err := json.Marshal(map[string]any{
		"task_id":             taskID,
		"model_name":          modelName,
		"model_version":       modelVersion,
		"worker_id":           workerID,
		"cache_hit":           cacheHit,
		"model_load_ms":       modelLoadMs,
		"checkpoint_fetch_ms": checkpointFetchMs,
		"total_latency_ms":    totalLatencyMs,
		"timestamp_ms":        time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.logClient.AppendLog(context.Background(), &logservepb.AppendLogRequest{
		StreamId:       "llm:" + taskID,
		EventType:      "LLMCompleted",
		IdempotencyKey: "seed:" + taskID,
		Payload:        payload,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.controlClient.CompleteTask(context.Background(), &logservepb.CompleteTaskRequest{
		TaskId:         taskID,
		WorkerId:       workerID,
		Status:         logservepb.TaskStatus_TASK_STATUS_SUCCEEDED,
		ResultJson:     []byte(`"ok"`),
		TaskLeaseEpoch: poll.GetTask().GetTaskLeaseEpoch(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLLMReplayRecordsModelLoadAndCompletion(t *testing.T) {
	env := startWorkflowEnv(t)
	defer env.stop()
	registerTestModels(t, env.controlClient)
	setPolicy(t, env.controlClient, logservepb.SchedulingPolicy_SCHEDULING_POLICY_LOCALITY_AWARE)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runWorkerForTestWithModels(ctx, t, env, "llm-worker", nil, 0)

	submitted := submitLLMForTest(t, env.controlClient, "model-A", "what is logserve?")
	status := waitTask(t, env.controlClient, submitted.GetTaskId(), logservepb.TaskStatus_TASK_STATUS_SUCCEEDED)
	if status.GetWorkerId() != "llm-worker" {
		t.Fatalf("worker_id = %s, want llm-worker", status.GetWorkerId())
	}
	if !strings.Contains(string(status.GetResultJson()), "mock:model-A") {
		t.Fatalf("llm result = %s", status.GetResultJson())
	}

	replayed, err := env.controlClient.ReplayLLM(context.Background(), &logservepb.ReplayLLMRequest{TaskId: submitted.GetTaskId()})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.GetTaskId() != submitted.GetTaskId() {
		t.Fatalf("replayed task_id = %s", replayed.GetTaskId())
	}
	if replayed.GetWorkerId() != "llm-worker" {
		t.Fatalf("replayed worker_id = %s", replayed.GetWorkerId())
	}
	if replayed.GetCacheHit() {
		t.Fatal("first uncached model-A request should be a cache miss")
	}
	if replayed.GetModelLoadMs() <= 0 || replayed.GetFirstTokenMs() <= 0 || replayed.GetTotalLatencyMs() <= 0 {
		t.Fatalf("missing latency metrics: load=%d first_token=%d total=%d", replayed.GetModelLoadMs(), replayed.GetFirstTokenMs(), replayed.GetTotalLatencyMs())
	}
	if got := llmEventTypes(replayed.GetEvents()); strings.Join(got, ",") != "ModelLoadStarted,ModelLoaded,LLMCompleted" {
		t.Fatalf("llm events = %v", got)
	}
}

func TestLLMCheckpointCacheFetchThenHit(t *testing.T) {
	env := startWorkflowEnv(t)
	defer env.stop()
	registerTestModels(t, env.controlClient)
	setPolicy(t, env.controlClient, logservepb.SchedulingPolicy_SCHEDULING_POLICY_LOCALITY_AWARE)

	sourceDir := t.TempDir()
	cacheDir := t.TempDir()
	writeLLMCheckpoint(t, sourceDir, "model-A", "v1", []byte(strings.Repeat("checkpoint", 256)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runWorkerForTestWithConfig(ctx, t, env, worker.Config{
		WorkerID:                 "checkpoint-worker",
		MaxTasks:                 2,
		ModelCheckpointSourceDir: sourceDir,
		ModelCacheDir:            cacheDir,
		ModelCacheCapacityBytes:  1 << 20,
	})

	first := submitLLMForTest(t, env.controlClient, "model-A", "first")
	waitTask(t, env.controlClient, first.GetTaskId(), logservepb.TaskStatus_TASK_STATUS_SUCCEEDED)
	second := submitLLMForTest(t, env.controlClient, "model-A", "second")
	waitTask(t, env.controlClient, second.GetTaskId(), logservepb.TaskStatus_TASK_STATUS_SUCCEEDED)

	firstLoad := llmEventPayloadForTest(t, env.logClient, first.GetTaskId(), "ModelLoaded")
	if firstLoad.CacheHit {
		t.Fatal("first checkpoint request should be a cache miss")
	}
	if firstLoad.CheckpointFetchMs <= 0 || firstLoad.ModelLoadMs <= 0 {
		t.Fatalf("first checkpoint metrics missing: fetch=%d load=%d", firstLoad.CheckpointFetchMs, firstLoad.ModelLoadMs)
	}

	secondLoad := llmEventPayloadForTest(t, env.logClient, second.GetTaskId(), "ModelLoaded")
	if !secondLoad.CacheHit {
		t.Fatal("second checkpoint request should be a cache hit")
	}
	if secondLoad.CheckpointFetchMs != 0 {
		t.Fatalf("cache hit checkpoint_fetch_ms = %d, want 0", secondLoad.CheckpointFetchMs)
	}
	if secondLoad.ModelLoadMs <= 0 {
		t.Fatalf("cache hit model_load_ms = %d, want > 0", secondLoad.ModelLoadMs)
	}

	firstReplayed, err := env.controlClient.ReplayLLM(context.Background(), &logservepb.ReplayLLMRequest{TaskId: first.GetTaskId()})
	if err != nil {
		t.Fatal(err)
	}
	if firstReplayed.GetCheckpointFetchMs() <= 0 || firstReplayed.GetCacheUsedBytes() <= 0 || firstReplayed.GetCacheCapacityBytes() <= 0 {
		t.Fatalf("ReplayLLM first checkpoint metrics missing: fetch=%d used=%d capacity=%d",
			firstReplayed.GetCheckpointFetchMs(), firstReplayed.GetCacheUsedBytes(), firstReplayed.GetCacheCapacityBytes())
	}

	replayed, err := env.controlClient.ReplayLLM(context.Background(), &logservepb.ReplayLLMRequest{TaskId: second.GetTaskId()})
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.GetCacheHit() {
		t.Fatal("ReplayLLM should reconstruct second request as cache hit")
	}
	if replayed.GetCheckpointFetchMs() != 0 || replayed.GetCacheUsedBytes() <= 0 || replayed.GetCacheCapacityBytes() <= 0 {
		t.Fatalf("ReplayLLM cache-hit metrics = fetch:%d used:%d capacity:%d",
			replayed.GetCheckpointFetchMs(), replayed.GetCacheUsedBytes(), replayed.GetCacheCapacityBytes())
	}
}

func TestLocalityAwareImprovesSchedulingExperimentMetrics(t *testing.T) {
	resource := runManualSchedulingExperiment(t, logservepb.SchedulingPolicy_SCHEDULING_POLICY_RESOURCE_ONLY)
	locality := runManualSchedulingExperiment(t, logservepb.SchedulingPolicy_SCHEDULING_POLICY_LOCALITY_AWARE)

	if locality.CacheHitRate <= resource.CacheHitRate {
		t.Fatalf("cache hit rate did not improve: resource=%.2f locality=%.2f", resource.CacheHitRate, locality.CacheHitRate)
	}
	if locality.ColdStartMs >= resource.ColdStartMs {
		t.Fatalf("cold start did not improve: resource=%d locality=%d", resource.ColdStartMs, locality.ColdStartMs)
	}
	if locality.P95LatencyMs >= resource.P95LatencyMs || locality.P99LatencyMs >= resource.P99LatencyMs {
		t.Fatalf("tail latency did not improve: resource p95=%d p99=%d locality p95=%d p99=%d",
			resource.P95LatencyMs, resource.P99LatencyMs, locality.P95LatencyMs, locality.P99LatencyMs)
	}
}

func TestRAGWorkflowCanUseMockLLM(t *testing.T) {
	env := startWorkflowEnv(t)
	defer env.stop()
	registerTestModels(t, env.controlClient)
	setPolicy(t, env.controlClient, logservepb.SchedulingPolicy_SCHEDULING_POLICY_LOCALITY_AWARE)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runWorkerForTestWithModels(ctx, t, env, "rag-worker", []string{"model-A:v1"}, 0)

	submitted := submitWorkflowForTest(t, env.controlClient, ragWithLLMDefinition(t))
	status := waitWorkflow(t, env.controlClient, submitted.GetWorkflowId(), logservepb.WorkflowStatus_WORKFLOW_STATUS_COMPLETED)
	if !strings.Contains(string(status.GetResultJson()), "mock:model-A") {
		t.Fatalf("workflow result = %s", status.GetResultJson())
	}
	llmStep := stepByID(t, status, "generate_answer")
	if llmStep.GetStatus() != logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SUCCEEDED {
		t.Fatalf("llm step status = %s", llmStep.GetStatus())
	}
}

type experimentMetrics struct {
	CacheHitRate  float64
	ColdStartMs   int64
	P95LatencyMs  int64
	P99LatencyMs  int64
	AssignedTasks []string
}

func runManualSchedulingExperiment(t *testing.T, policy logservepb.SchedulingPolicy) experimentMetrics {
	t.Helper()
	env := startWorkflowEnv(t)
	defer env.stop()
	registerLLMWorkers(t, env.controlClient)
	registerTestModels(t, env.controlClient)
	setPolicy(t, env.controlClient, policy)

	const requests = 6
	hits := 0
	var coldStartMs int64
	latencies := make([]int64, 0, requests)
	assigned := make([]string, 0, requests)
	pollOrder := []string{"worker-2", "worker-3", "worker-1"}
	for i := 0; i < requests; i++ {
		submitLLMForTest(t, env.controlClient, "model-A", "hello")
		var worker string
		var taskID string
		var taskEpoch uint64
		for _, poller := range pollOrder {
			resp, err := env.controlClient.PollTask(context.Background(), &logservepb.PollTaskRequest{WorkerId: poller})
			if err != nil {
				t.Fatal(err)
			}
			if resp.GetHasTask() {
				worker = poller
				taskID = resp.GetTask().GetTaskId()
				taskEpoch = resp.GetTask().GetTaskLeaseEpoch()
				break
			}
		}
		if worker == "" {
			t.Fatal("no worker selected")
		}
		if _, err := env.controlClient.CompleteTask(context.Background(), &logservepb.CompleteTaskRequest{
			TaskId:         taskID,
			WorkerId:       worker,
			TaskLeaseEpoch: taskEpoch,
			Status:         logservepb.TaskStatus_TASK_STATUS_SUCCEEDED,
			ResultJson:     []byte(`"ok"`),
		}); err != nil {
			t.Fatal(err)
		}
		assigned = append(assigned, worker)
		cacheHit := worker == "worker-1"
		if cacheHit {
			hits++
			latencies = append(latencies, 25)
		} else {
			coldStartMs += 100
			latencies = append(latencies, 125)
		}
	}
	return experimentMetrics{
		CacheHitRate:  float64(hits) / requests,
		ColdStartMs:   coldStartMs,
		P95LatencyMs:  percentile(latencies, 95),
		P99LatencyMs:  percentile(latencies, 99),
		AssignedTasks: assigned,
	}
}

func registerLLMWorkers(t *testing.T, client logservepb.ControlServiceClient) {
	t.Helper()
	for _, worker := range []struct {
		id     string
		models []*logservepb.ModelCacheEntry
	}{
		{id: "worker-1", models: []*logservepb.ModelCacheEntry{{Name: "model-A", Version: "v1"}}},
		{id: "worker-2", models: []*logservepb.ModelCacheEntry{{Name: "model-B", Version: "v1"}}},
		{id: "worker-3"},
	} {
		if _, err := client.RegisterWorker(context.Background(), &logservepb.RegisterWorkerRequest{
			WorkerId:     worker.id,
			CachedModels: worker.models,
			Capacity:     1,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func registerTestModels(t *testing.T, client logservepb.ControlServiceClient) {
	t.Helper()
	for _, model := range []*logservepb.ModelInfo{
		{Name: "model-A", Version: "v1", SizeBytes: 100, Path: "mock://model-A", Adapter: "mock"},
		{Name: "model-B", Version: "v1", SizeBytes: 100, Path: "mock://model-B", Adapter: "mock"},
	} {
		if _, err := client.RegisterModel(context.Background(), &logservepb.RegisterModelRequest{Model: model}); err != nil {
			t.Fatal(err)
		}
	}
}

func setPolicy(t *testing.T, client logservepb.ControlServiceClient, policy logservepb.SchedulingPolicy) {
	t.Helper()
	if _, err := client.SetSchedulingPolicy(context.Background(), &logservepb.SetSchedulingPolicyRequest{Policy: policy}); err != nil {
		t.Fatal(err)
	}
}

func submitLLMForTest(t *testing.T, client logservepb.ControlServiceClient, model, prompt string) *logservepb.SubmitLLMResponse {
	t.Helper()
	resp, err := client.SubmitLLM(context.Background(), &logservepb.SubmitLLMRequest{
		ModelName:    model,
		ModelVersion: "v1",
		Prompt:       prompt,
		MaxTokens:    32,
		Adapter:      "mock",
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func waitTask(t *testing.T, client logservepb.ControlServiceClient, taskID string, want logservepb.TaskStatus) *logservepb.GetTaskStatusResponse {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last *logservepb.GetTaskStatusResponse
	for time.Now().Before(deadline) {
		resp, err := client.GetTaskStatus(context.Background(), &logservepb.GetTaskStatusRequest{TaskId: taskID})
		if err != nil {
			t.Fatal(err)
		}
		last = resp
		if resp.GetStatus() == want {
			return resp
		}
		if resp.GetStatus() == logservepb.TaskStatus_TASK_STATUS_FAILED {
			t.Fatalf("task failed: %s", resp.GetError())
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("task status = %s, want %s; last=%v", last.GetStatus(), want, last)
	return nil
}

func runWorkerForTestWithModels(ctx context.Context, t *testing.T, env *workflowTestEnv, workerID string, cachedModels []string, maxTasks int) {
	t.Helper()
	err := worker.Run(ctx, worker.Config{
		WorkerID:     workerID,
		ControlAddr:  env.controlServer.Addr(),
		LogAddr:      env.logServer.Addr(),
		PythonPath:   "python",
		ExecutorPath: filepath.Join(env.root, "executor", "python", "server.py"),
		PollInterval: 20 * time.Millisecond,
		MaxTasks:     maxTasks,
		CachedModels: cachedModels,
	})
	if err != nil && err != context.Canceled {
		t.Errorf("worker %s stopped: %v", workerID, err)
	}
}

func runWorkerForTestWithConfig(ctx context.Context, t *testing.T, env *workflowTestEnv, cfg worker.Config) {
	t.Helper()
	if cfg.WorkerID == "" {
		cfg.WorkerID = "worker"
	}
	cfg.ControlAddr = env.controlServer.Addr()
	cfg.LogAddr = env.logServer.Addr()
	cfg.PythonPath = "python"
	cfg.ExecutorPath = filepath.Join(env.root, "executor", "python", "server.py")
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 20 * time.Millisecond
	}
	err := worker.Run(ctx, cfg)
	if err != nil && err != context.Canceled {
		t.Errorf("worker %s stopped: %v", cfg.WorkerID, err)
	}
}

func writeLLMCheckpoint(t *testing.T, root, name, version string, data []byte) {
	t.Helper()
	dir := filepath.Join(root, name+"-"+version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func llmEventPayloadForTest(t *testing.T, client logservepb.LogServiceClient, taskID, eventType string) struct {
	CacheHit          bool  `json:"cache_hit"`
	ModelLoadMs       int64 `json:"model_load_ms"`
	CheckpointFetchMs int64 `json:"checkpoint_fetch_ms"`
	EvictionCount     int64 `json:"eviction_count"`
} {
	t.Helper()
	records, err := client.ReadLog(context.Background(), &logservepb.ReadLogRequest{
		StreamId: "llm:" + taskID,
		FromSeq:  1,
		Limit:    100,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range records.GetRecords() {
		if rec.GetEventType() != eventType {
			continue
		}
		var payload struct {
			CacheHit          bool  `json:"cache_hit"`
			ModelLoadMs       int64 `json:"model_load_ms"`
			CheckpointFetchMs int64 `json:"checkpoint_fetch_ms"`
			EvictionCount     int64 `json:"eviction_count"`
		}
		if err := json.Unmarshal(rec.GetPayload(), &payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}
	t.Fatalf("event %s not found for task %s", eventType, taskID)
	return struct {
		CacheHit          bool  `json:"cache_hit"`
		ModelLoadMs       int64 `json:"model_load_ms"`
		CheckpointFetchMs int64 `json:"checkpoint_fetch_ms"`
		EvictionCount     int64 `json:"eviction_count"`
	}{}
}

func llmEventTypes(events []*logservepb.LLMEvent) []string {
	out := make([]string, 0, len(events))
	for _, event := range events {
		out = append(out, event.GetEventType())
	}
	return out
}

func ragWithLLMDefinition(t *testing.T) map[string]any {
	t.Helper()
	source := `
def embed(query):
    return "vec:" + query

def search(vec):
    return ["doc:" + vec]

def build_prompt(query, docs):
    return "answer " + query + " using " + docs[0]
`
	return map[string]any{
		"workflow_name":   "rag_with_llm",
		"function_source": source,
		"max_attempts":    3,
		"timeout_ms":      30000,
		"result_step_id":  "generate_answer",
		"steps": []map[string]any{
			{
				"step_id":         "embed",
				"task_name":       "embed",
				"function_name":   "embed",
				"function_source": source,
				"args_json":       map[string]any{"args": []any{"hello"}, "kwargs": map[string]any{}},
				"depends_on":      []string{},
			},
			{
				"step_id":         "search",
				"task_name":       "search",
				"function_name":   "search",
				"function_source": source,
				"args_json":       map[string]any{"args": []any{map[string]any{"__step_ref__": "embed"}}, "kwargs": map[string]any{}},
				"depends_on":      []string{"embed"},
			},
			{
				"step_id":         "build_prompt",
				"task_name":       "build_prompt",
				"function_name":   "build_prompt",
				"function_source": source,
				"args_json":       map[string]any{"args": []any{"hello", map[string]any{"__step_ref__": "search"}}, "kwargs": map[string]any{}},
				"depends_on":      []string{"search"},
			},
			{
				"step_id":           "generate_answer",
				"task_name":         "llm:model-A",
				"function_name":     "__logserve_llm__",
				"args_json":         map[string]any{"args": []any{map[string]any{"__step_ref__": "build_prompt"}}, "kwargs": map[string]any{}},
				"depends_on":        []string{"build_prompt"},
				"llm_model_name":    "model-A",
				"llm_model_version": "v1",
				"llm_adapter":       "mock",
				"llm_max_tokens":    32,
			},
		},
	}
}

func percentile(values []int64, p int) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]int64(nil), values...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j-1] > sorted[j]; j-- {
			sorted[j-1], sorted[j] = sorted[j], sorted[j-1]
		}
	}
	idx := (len(sorted)*p + 99) / 100
	if idx <= 0 {
		idx = 1
	}
	if idx > len(sorted) {
		idx = len(sorted)
	}
	return sorted[idx-1]
}

var _ = json.RawMessage{}
