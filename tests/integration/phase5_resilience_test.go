package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
)

func TestRunningTaskIsRedeliveredAfterWorkerLeaseExpires(t *testing.T) {
	env := startWorkflowEnv(t)
	defer env.stop()

	configureBackpressure(t, env.controlClient, 10, 100)
	registerBasicWorker(t, env.controlClient, "redelivery-worker-1")
	registerBasicWorker(t, env.controlClient, "redelivery-worker-2")

	submitted := submitPlainTask(t, env.controlClient, "redeliver_me")
	firstPoll, err := env.controlClient.PollTask(context.Background(), &logservepb.PollTaskRequest{WorkerId: "redelivery-worker-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !firstPoll.GetHasTask() || firstPoll.GetTask().GetTaskId() != submitted.GetTaskId() {
		t.Fatalf("first poll = %v, want task %s", firstPoll, submitted.GetTaskId())
	}
	if _, err := env.controlClient.StartTask(context.Background(), &logservepb.StartTaskRequest{
		TaskId:   submitted.GetTaskId(),
		WorkerId: "redelivery-worker-1",
	}); err != nil {
		t.Fatal(err)
	}

	time.Sleep(180 * time.Millisecond)
	secondPoll, err := env.controlClient.PollTask(context.Background(), &logservepb.PollTaskRequest{WorkerId: "redelivery-worker-2"})
	if err != nil {
		t.Fatal(err)
	}
	if !secondPoll.GetHasTask() || secondPoll.GetTask().GetTaskId() != submitted.GetTaskId() {
		t.Fatalf("redelivery poll = %v, want task %s", secondPoll, submitted.GetTaskId())
	}
}

func TestPolledTaskIsRedeliveredWhenWorkerDiesBeforeStart(t *testing.T) {
	env := startWorkflowEnv(t)
	defer env.stop()

	configureBackpressure(t, env.controlClient, 10, 100)
	registerBasicWorker(t, env.controlClient, "poll-worker-1")
	registerBasicWorker(t, env.controlClient, "poll-worker-2")

	submitted := submitPlainTask(t, env.controlClient, "redeliver_before_start")
	firstPoll, err := env.controlClient.PollTask(context.Background(), &logservepb.PollTaskRequest{WorkerId: "poll-worker-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !firstPoll.GetHasTask() || firstPoll.GetTask().GetTaskId() != submitted.GetTaskId() {
		t.Fatalf("first poll = %v, want task %s", firstPoll, submitted.GetTaskId())
	}

	time.Sleep(180 * time.Millisecond)
	secondPoll, err := env.controlClient.PollTask(context.Background(), &logservepb.PollTaskRequest{WorkerId: "poll-worker-2"})
	if err != nil {
		t.Fatal(err)
	}
	if !secondPoll.GetHasTask() || secondPoll.GetTask().GetTaskId() != submitted.GetTaskId() {
		t.Fatalf("redelivery poll = %v, want task %s", secondPoll, submitted.GetTaskId())
	}
}

func TestStaleTaskCompletionRejectedAfterRedelivery(t *testing.T) {
	env := startWorkflowEnv(t)
	defer env.stop()

	configureBackpressure(t, env.controlClient, 10, 100)
	registerBasicWorker(t, env.controlClient, "stale-worker-1")
	registerBasicWorker(t, env.controlClient, "stale-worker-2")

	submitted := submitPlainTask(t, env.controlClient, "reject_stale_completion")
	firstPoll, err := env.controlClient.PollTask(context.Background(), &logservepb.PollTaskRequest{WorkerId: "stale-worker-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !firstPoll.GetHasTask() {
		t.Fatal("first worker did not receive task")
	}
	if _, err := env.controlClient.StartTask(context.Background(), &logservepb.StartTaskRequest{
		TaskId:   submitted.GetTaskId(),
		WorkerId: "stale-worker-1",
	}); err != nil {
		t.Fatal(err)
	}

	time.Sleep(180 * time.Millisecond)
	secondPoll, err := env.controlClient.PollTask(context.Background(), &logservepb.PollTaskRequest{WorkerId: "stale-worker-2"})
	if err != nil {
		t.Fatal(err)
	}
	if !secondPoll.GetHasTask() {
		t.Fatal("second worker did not receive redelivered task")
	}
	if _, err := env.controlClient.StartTask(context.Background(), &logservepb.StartTaskRequest{
		TaskId:   submitted.GetTaskId(),
		WorkerId: "stale-worker-2",
	}); err != nil {
		t.Fatal(err)
	}

	_, err = env.controlClient.CompleteTask(context.Background(), &logservepb.CompleteTaskRequest{
		TaskId:     submitted.GetTaskId(),
		WorkerId:   "stale-worker-1",
		Status:     logservepb.TaskStatus_TASK_STATUS_SUCCEEDED,
		ResultJson: []byte(`"stale"`),
	})
	if err == nil || !strings.Contains(err.Error(), "stale task completion") {
		t.Fatalf("stale completion error = %v, want stale task completion rejection", err)
	}
}

func TestBackpressureRejectsNewTaskWhenQueueBacklogExceedsWatermark(t *testing.T) {
	env := startWorkflowEnv(t)
	defer env.stop()

	configureBackpressure(t, env.controlClient, 1, 1000)
	submitPlainTask(t, env.controlClient, "queued_first")
	_, err := env.controlClient.SubmitTask(context.Background(), &logservepb.SubmitTaskRequest{
		TaskName:       "queued_second",
		FunctionName:   "queued_second",
		FunctionSource: plainTaskSource("queued_second"),
		ArgsJson:       []byte(`{"args":[],"kwargs":{}}`),
	})
	if err == nil {
		t.Fatal("second queued task should be rejected by backpressure")
	}
	if !strings.Contains(err.Error(), "backpressure") {
		t.Fatalf("error = %v, want backpressure", err)
	}
}

func TestBackpressureAllowsIdempotentDuplicateWhenQueueIsFull(t *testing.T) {
	env := startWorkflowEnv(t)
	defer env.stop()

	configureBackpressure(t, env.controlClient, 1, 1000)
	req := &logservepb.SubmitTaskRequest{
		TaskName:       "idempotent_queued",
		FunctionName:   "idempotent_queued",
		FunctionSource: plainTaskSource("idempotent_queued"),
		ArgsJson:       []byte(`{"args":[],"kwargs":{}}`),
		IdempotencyKey: "same-task-key",
	}
	first, err := env.controlClient.SubmitTask(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := env.controlClient.SubmitTask(context.Background(), req)
	if err != nil {
		t.Fatalf("idempotent duplicate should bypass backpressure: %v", err)
	}
	if second.GetTaskId() != first.GetTaskId() {
		t.Fatalf("duplicate task id = %s, want %s", second.GetTaskId(), first.GetTaskId())
	}
}

func TestLogAppendSlowBackpressureRejectsNewTask(t *testing.T) {
	env := startWorkflowEnv(t)
	defer env.stop()

	_, err := env.controlClient.SetBackpressure(context.Background(), &logservepb.SetBackpressureRequest{
		QueueHighWatermark:  10,
		RedeliveryTimeoutMs: 1000,
		LogAppendSlowMs:     1,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = env.controlClient.SubmitTask(context.Background(), &logservepb.SubmitTaskRequest{
		TaskName:       "log_slow_rejected",
		FunctionName:   "log_slow_rejected",
		FunctionSource: plainTaskSource("log_slow_rejected"),
		ArgsJson:       []byte(`{"args":[],"kwargs":{}}`),
	})
	if err == nil || !strings.Contains(err.Error(), "last log append latency") {
		t.Fatalf("submit error = %v, want log append backpressure", err)
	}
}

func TestDashboardSnapshotShowsWorkflowTaskActorAndModelCache(t *testing.T) {
	env := startWorkflowEnv(t)
	defer env.stop()

	registerTestModels(t, env.controlClient)
	registerLLMWorkers(t, env.controlClient)
	actor := createCounterActor(t, env.controlClient, 20)
	workflow := submitWorkflowForTest(t, env.controlClient, simpleRAGDefinition(t))
	task := submitPlainTask(t, env.controlClient, "dashboard_task")

	snapshot, err := env.controlClient.GetDashboardSnapshot(context.Background(), &logservepb.GetDashboardSnapshotRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.GetQueueDepth() < 2 {
		t.Fatalf("queue_depth = %d, want at least 2", snapshot.GetQueueDepth())
	}
	if dashboardWorkflow(snapshot, workflow.GetWorkflowId()) == nil {
		t.Fatalf("workflow %s missing from dashboard", workflow.GetWorkflowId())
	}
	if dashboardTask(snapshot, task.GetTaskId()) == nil {
		t.Fatalf("task %s missing from dashboard", task.GetTaskId())
	}
	if dashboardActor(snapshot, actor.GetActorId()) == nil {
		t.Fatalf("actor %s missing from dashboard", actor.GetActorId())
	}
	worker := dashboardWorker(snapshot, "worker-1")
	if worker == nil {
		t.Fatal("worker-1 missing from dashboard")
	}
	if len(worker.GetCachedModels()) == 0 || worker.GetCachedModels()[0].GetName() != "model-A" {
		t.Fatalf("worker-1 cached models = %v, want model-A", worker.GetCachedModels())
	}
	if len(snapshot.GetModels()) < 2 {
		t.Fatalf("models = %v, want registered models", snapshot.GetModels())
	}
}

func TestControlRestartBootstrapsWorkflowAndModelStateFromLog(t *testing.T) {
	env := startWorkflowEnv(t)
	defer env.stop()

	registerTestModels(t, env.controlClient)
	setPolicy(t, env.controlClient, logservepb.SchedulingPolicy_SCHEDULING_POLICY_RESOURCE_ONLY)
	submitted := submitWorkflowForTest(t, env.controlClient, simpleRAGDefinition(t))

	env.restartControl(t)

	if _, err := env.controlClient.SubmitLLM(context.Background(), &logservepb.SubmitLLMRequest{
		ModelName:    "model-A",
		ModelVersion: "v1",
		Prompt:       "registry survived restart",
		Adapter:      "mock",
	}); err != nil {
		t.Fatalf("model registry did not bootstrap after control restart: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runWorkerForTest(ctx, t, env, "bootstrap-worker", 0)
	status := waitWorkflow(t, env.controlClient, submitted.GetWorkflowId(), logservepb.WorkflowStatus_WORKFLOW_STATUS_COMPLETED)
	if string(status.GetResultJson()) != `"answer:hello:doc:vec:hello"` {
		t.Fatalf("result after control restart = %s", status.GetResultJson())
	}
}

func configureBackpressure(t *testing.T, client logservepb.ControlServiceClient, queueHighWatermark uint32, redeliveryTimeoutMs int64) {
	t.Helper()
	_, err := client.SetBackpressure(context.Background(), &logservepb.SetBackpressureRequest{
		QueueHighWatermark:  queueHighWatermark,
		RedeliveryTimeoutMs: redeliveryTimeoutMs,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func registerBasicWorker(t *testing.T, client logservepb.ControlServiceClient, workerID string) {
	t.Helper()
	if _, err := client.RegisterWorker(context.Background(), &logservepb.RegisterWorkerRequest{WorkerId: workerID, Capacity: 1}); err != nil {
		t.Fatal(err)
	}
}

func submitPlainTask(t *testing.T, client logservepb.ControlServiceClient, name string) *logservepb.SubmitTaskResponse {
	t.Helper()
	resp, err := client.SubmitTask(context.Background(), &logservepb.SubmitTaskRequest{
		TaskName:       name,
		FunctionName:   name,
		FunctionSource: plainTaskSource(name),
		ArgsJson:       []byte(`{"args":[],"kwargs":{}}`),
		IdempotencyKey: name + time.Now().Format("150405.000000000"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func plainTaskSource(name string) string {
	data, _ := json.Marshal(name + "-ok")
	return "def " + name + "():\n    return " + string(data) + "\n"
}

func dashboardWorkflow(snapshot *logservepb.DashboardSnapshot, workflowID string) *logservepb.DashboardWorkflow {
	for _, workflow := range snapshot.GetWorkflows() {
		if workflow.GetWorkflowId() == workflowID {
			return workflow
		}
	}
	return nil
}

func dashboardTask(snapshot *logservepb.DashboardSnapshot, taskID string) *logservepb.DashboardTask {
	for _, task := range snapshot.GetTasks() {
		if task.GetTaskId() == taskID {
			return task
		}
	}
	return nil
}

func dashboardActor(snapshot *logservepb.DashboardSnapshot, actorID string) *logservepb.GetActorStatusResponse {
	for _, actor := range snapshot.GetActors() {
		if actor.GetActorId() == actorID {
			return actor
		}
	}
	return nil
}

func dashboardWorker(snapshot *logservepb.DashboardSnapshot, workerID string) *logservepb.DashboardWorker {
	for _, worker := range snapshot.GetWorkers() {
		if worker.GetWorkerId() == workerID {
			return worker
		}
	}
	return nil
}
