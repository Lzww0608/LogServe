package integration

// This file covers control-plane worker RPC batching: multi-task polling,
// long-poll wakeups, and batched completion acknowledgements.

import (
	"context"
	"testing"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
)

// TestPollTaskBatchReturnsUpToMaxTasks verifies one PollTask RPC can return all
// currently queued tasks up to the requested MaxTasks limit.
func TestPollTaskBatchReturnsUpToMaxTasks(t *testing.T) {
	env := startWorkflowEnv(t)
	defer env.stop()

	workerID := "batch-poller"
	if _, err := env.controlClient.RegisterWorker(context.Background(), &logservepb.RegisterWorkerRequest{
		WorkerId: workerID,
		Capacity: 4,
	}); err != nil {
		t.Fatal(err)
	}
	waitDashboardWorker(t, env.controlClient, workerID)

	taskIDs := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		resp, err := env.controlClient.SubmitTask(context.Background(), &logservepb.SubmitTaskRequest{
			TaskName:       "noop",
			FunctionName:   "noop",
			FunctionSource: "def noop():\n    return 1\n",
			ArgsJson:       []byte(`{"args":[],"kwargs":{}}`),
			IdempotencyKey: "batch-poll-" + string(rune('a'+i)),
		})
		if err != nil {
			t.Fatal(err)
		}
		taskIDs = append(taskIDs, resp.GetTaskId())
	}

	poll, err := env.controlClient.PollTask(context.Background(), &logservepb.PollTaskRequest{
		WorkerId: workerID,
		MaxTasks: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(poll.GetTasks()); got != 4 {
		t.Fatalf("batched tasks = %d, want 4 in one PollTask RPC", got)
	}
	gotIDs := make(map[string]struct{}, len(poll.GetTasks()))
	for _, spec := range poll.GetTasks() {
		gotIDs[spec.GetTaskId()] = struct{}{}
	}
	for _, want := range taskIDs {
		if _, ok := gotIDs[want]; !ok {
			t.Fatalf("poll tasks missing %s; got %v", want, gotIDs)
		}
	}
}

// TestPollTaskLongPollWakesBeforeTimeout starts a blocking poll and confirms a
// later task submission wakes it before the wait timeout expires.
func TestPollTaskLongPollWakesBeforeTimeout(t *testing.T) {
	env := startWorkflowEnv(t)
	defer env.stop()

	workerID := "long-poller"
	if _, err := env.controlClient.RegisterWorker(context.Background(), &logservepb.RegisterWorkerRequest{
		WorkerId: workerID,
		Capacity: 1,
	}); err != nil {
		t.Fatal(err)
	}

	resultCh := make(chan *logservepb.PollTaskResponse, 1)
	errCh := make(chan error, 1)
	start := time.Now()
	go func() {
		poll, err := env.controlClient.PollTask(context.Background(), &logservepb.PollTaskRequest{
			WorkerId:      workerID,
			MaxTasks:      1,
			WaitTimeoutMs: 1000,
		})
		if err != nil {
			errCh <- err
			return
		}
		resultCh <- poll
	}()

	// Give the goroutine enough time to enter the long-poll path before submitting
	// the task that should wake it.
	time.Sleep(50 * time.Millisecond)
	submit, err := env.controlClient.SubmitTask(context.Background(), &logservepb.SubmitTaskRequest{
		TaskName:       "wake",
		FunctionName:   "wake",
		FunctionSource: "def wake():\n    return 1\n",
		ArgsJson:       []byte(`{"args":[],"kwargs":{}}`),
		IdempotencyKey: "long-poll-wake",
	})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-errCh:
		t.Fatal(err)
	case poll := <-resultCh:
		if elapsed := time.Since(start); elapsed >= 800*time.Millisecond {
			t.Fatalf("long poll returned after %s, want wake well before timeout", elapsed)
		}
		if !poll.GetHasTask() || poll.GetTask().GetTaskId() != submit.GetTaskId() {
			t.Fatalf("poll = %+v, want task %s", poll, submit.GetTaskId())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("long poll did not return after task submit")
	}
}

// TestCompleteTasksBatchAcceptsMultipleResults polls several leases and completes
// them in one CompleteTasks RPC, preserving each task's lease epoch.
func TestCompleteTasksBatchAcceptsMultipleResults(t *testing.T) {
	env := startWorkflowEnv(t)
	defer env.stop()

	workerID := "batch-complete"
	if _, err := env.controlClient.RegisterWorker(context.Background(), &logservepb.RegisterWorkerRequest{
		WorkerId: workerID,
		Capacity: 3,
	}); err != nil {
		t.Fatal(err)
	}

	specs := make([]*logservepb.TaskSpec, 0, 3)
	for i := 0; i < 3; i++ {
		resp, err := env.controlClient.SubmitTask(context.Background(), &logservepb.SubmitTaskRequest{
			TaskName:       "done",
			FunctionName:   "done",
			FunctionSource: "def done():\n    return 1\n",
			ArgsJson:       []byte(`{"args":[],"kwargs":{}}`),
			IdempotencyKey: "batch-complete-" + string(rune('a'+i)),
		})
		if err != nil {
			t.Fatal(err)
		}
		poll, err := env.controlClient.PollTask(context.Background(), &logservepb.PollTaskRequest{
			WorkerId: workerID,
			MaxTasks: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if poll.GetTask().GetTaskId() != resp.GetTaskId() {
			t.Fatalf("polled task = %s, want %s", poll.GetTask().GetTaskId(), resp.GetTaskId())
		}
		specs = append(specs, poll.GetTask())
	}

	// Batch completions still carry individual lease epochs; the server must reject
	// stale items without losing the rest of the batch.
	completions := make([]*logservepb.CompleteTaskRequest, 0, len(specs))
	for _, spec := range specs {
		completions = append(completions, &logservepb.CompleteTaskRequest{
			TaskId:         spec.GetTaskId(),
			WorkerId:       workerID,
			Status:         logservepb.TaskStatus_TASK_STATUS_SUCCEEDED,
			ResultJson:     []byte(`1`),
			TaskLeaseEpoch: spec.GetTaskLeaseEpoch(),
		})
	}
	batch, err := env.controlClient.CompleteTasks(context.Background(), &logservepb.CompleteTaskBatchRequest{Tasks: completions})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.GetResults()) != 3 {
		t.Fatalf("batch results = %d, want 3", len(batch.GetResults()))
	}
	for _, result := range batch.GetResults() {
		if !result.GetAccepted() {
			t.Fatalf("completion rejected: %+v", result)
		}
	}
}
