package integration

// This file verifies that the worker's local task pool executes independent
// Python tasks concurrently instead of serializing all work on one goroutine.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/worker"
)

// TestWorkerLocalTaskPoolExecutesPythonTasksConcurrently submits four sleeping
// tasks and checks their TaskStarted timestamps are close enough to prove local
// task pool parallelism.
func TestWorkerLocalTaskPoolExecutesPythonTasksConcurrently(t *testing.T) {
	ensureExecutorDeps(t)
	env := startWorkflowEnv(t)
	defer env.stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		// Capacity and TaskPoolSize match the submitted task count so all tasks can
		// start without queueing inside the worker when batching works correctly.
		err := worker.Run(ctx, worker.Config{
			WorkerID:      "pool-worker",
			ControlAddr:   env.controlServer.Addr(),
			LogAddr:       env.logServer.Addr(),
			PythonPath:    "python",
			ExecutorPath:  filepath.Join(env.root, "executor", "python", "server.py"),
			PollInterval:  10 * time.Millisecond,
			MaxTasks:      4,
			Capacity:      4,
			TaskPoolSize:  4,
			LLMPoolSize:   1,
			ActorPoolSize: 1,
		})
		if err == context.Canceled {
			err = nil
		}
		done <- err
	}()

	waitDashboardWorker(t, env.controlClient, "pool-worker")

	source := "def sleepy(value):\n    import time\n    time.sleep(0.35)\n    return value\n"
	taskIDs := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		argsJSON, err := json.Marshal(map[string]any{"args": []int{i}, "kwargs": map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		resp, err := env.controlClient.SubmitTask(context.Background(), &logservepb.SubmitTaskRequest{
			TaskName:       "sleepy",
			FunctionName:   "sleepy",
			FunctionSource: source,
			ArgsJson:       argsJSON,
			IdempotencyKey: "pool-task-" + string(rune('a'+i)),
		})
		if err != nil {
			t.Fatal(err)
		}
		taskIDs = append(taskIDs, resp.GetTaskId())
	}

	for _, taskID := range taskIDs {
		waitTask(t, env.controlClient, taskID, logservepb.TaskStatus_TASK_STATUS_SUCCEEDED)
	}

	var first, last int64
	for i, taskID := range taskIDs {
		startedAt := taskStartedTimestamp(t, env.logClient, taskID)
		if i == 0 || startedAt < first {
			first = startedAt
		}
		if startedAt > last {
			last = startedAt
		}
	}
	// The tasks each sleep for 350ms; a wide start spread would indicate the worker
	// serialized execution even though capacity and pool size were both four.
	if spread := last - first; spread > 700 {
		t.Fatalf("TaskStarted timestamp spread = %dms, want <= 700ms; local task pool may be serialized", spread)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("worker stopped with error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not exit after max tasks")
	}
}

// waitDashboardWorker polls the dashboard until a worker registration is visible
// to the control plane.
func waitDashboardWorker(t *testing.T, client logservepb.ControlServiceClient, workerID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := client.GetDashboardSnapshot(context.Background(), &logservepb.GetDashboardSnapshotRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if dashboardWorker(snapshot, workerID) != nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("worker %s did not register", workerID)
}

// taskStartedTimestamp reads a task stream and returns the durable TaskStarted
// timestamp used to compare worker-side concurrency.
func taskStartedTimestamp(t *testing.T, client logservepb.LogServiceClient, taskID string) int64 {
	t.Helper()
	records, err := client.ReadLog(context.Background(), &logservepb.ReadLogRequest{
		StreamId: "task:" + taskID,
		FromSeq:  1,
		Limit:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records.GetRecords() {
		if record.GetEventType() == "TaskStarted" {
			return record.GetTimestampMs()
		}
	}
	t.Fatalf("TaskStarted event not found for task %s", taskID)
	return 0
}
