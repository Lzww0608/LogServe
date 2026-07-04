package control

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/actor"
	"github.com/logserve/logserve/internal/metadata"
)

// TestPollTaskBatchLegacyLeasesUpToMaxTasks verifies legacy polling leases up to the
// requested batch size and updates worker load.
func TestPollTaskBatchLegacyLeasesUpToMaxTasks(t *testing.T) {
	t.Setenv("LOGSERVE_SCHEDULER_V2", "0")
	meta := metadata.NewMemoryStore()
	meta.UpsertWorker(metadata.Worker{WorkerID: "worker-1", Capacity: 3, LastHeartbeat: time.Now().UnixMilli()})
	service := NewServiceWithResultStore(meta, acceptingLogClient{}, nil, 0)

	tasks := enqueueBatchTestTasks(t, service, 3)
	poll, err := service.PollTask(context.Background(), &logservepb.PollTaskRequest{WorkerId: "worker-1", MaxTasks: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !poll.GetHasTask() || poll.GetTask().GetTaskId() != tasks[0].TaskID {
		t.Fatalf("first task = %v, want %s", poll.GetTask(), tasks[0].TaskID)
	}
	if got := len(poll.GetTasks()); got != 2 {
		t.Fatalf("batched tasks = %d, want 2", got)
	}
	for i, spec := range poll.GetTasks() {
		if spec.GetTaskId() != tasks[i].TaskID {
			t.Fatalf("task[%d] = %s, want %s", i, spec.GetTaskId(), tasks[i].TaskID)
		}
		leased, ok := meta.GetTask(spec.GetTaskId())
		if !ok || leased.Status != logservepb.TaskStatus_TASK_STATUS_RUNNING || leased.WorkerID != "worker-1" {
			t.Fatalf("leased task %s = %+v ok=%v, want RUNNING on worker-1", spec.GetTaskId(), leased, ok)
		}
	}
	worker, ok := meta.GetWorker("worker-1")
	if !ok || worker.RunningTasks != 2 {
		t.Fatalf("worker running_tasks = %d ok=%v, want 2", worker.RunningTasks, ok)
	}
}

// TestPollTaskBatchIndexedAndCompleteTasks verifies scheduler v2 batch leasing and
// batch completion release worker load.
func TestPollTaskBatchIndexedAndCompleteTasks(t *testing.T) {
	t.Setenv("LOGSERVE_SCHEDULER_V2", "1")
	meta := metadata.NewMemoryStore()
	meta.UpsertWorker(metadata.Worker{WorkerID: "worker-1", Capacity: 3, LastHeartbeat: time.Now().UnixMilli()})
	service := NewServiceWithResultStore(meta, acceptingLogClient{}, nil, 0)

	enqueueBatchTestTasks(t, service, 3)
	poll, err := service.PollTask(context.Background(), &logservepb.PollTaskRequest{WorkerId: "worker-1", MaxTasks: 3})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(poll.GetTasks()); got != 3 {
		t.Fatalf("batched tasks = %d, want 3", got)
	}
	completeReqs := make([]*logservepb.CompleteTaskRequest, 0, len(poll.GetTasks()))
	for _, spec := range poll.GetTasks() {
		completeReqs = append(completeReqs, &logservepb.CompleteTaskRequest{
			TaskId:         spec.GetTaskId(),
			WorkerId:       "worker-1",
			Status:         logservepb.TaskStatus_TASK_STATUS_SUCCEEDED,
			ResultJson:     []byte(`{"ok":true}`),
			TaskLeaseEpoch: spec.GetTaskLeaseEpoch(),
		})
	}
	complete, err := service.CompleteTasks(context.Background(), &logservepb.CompleteTaskBatchRequest{Tasks: completeReqs})
	if err != nil {
		t.Fatal(err)
	}
	if !complete.GetAccepted() || len(complete.GetResults()) != 3 {
		t.Fatalf("batch complete = accepted %v results %d, want accepted 3 results", complete.GetAccepted(), len(complete.GetResults()))
	}
	for _, result := range complete.GetResults() {
		if !result.GetAccepted() || result.GetError() != "" {
			t.Fatalf("completion result = %+v, want accepted without error", result)
		}
		task, ok := meta.GetTask(result.GetTaskId())
		if !ok || task.Status != logservepb.TaskStatus_TASK_STATUS_SUCCEEDED {
			t.Fatalf("task %s status = %v ok=%v, want SUCCEEDED", result.GetTaskId(), task.Status, ok)
		}
	}
	worker, ok := meta.GetWorker("worker-1")
	if !ok || worker.RunningTasks != 0 {
		t.Fatalf("worker running_tasks = %d ok=%v, want 0", worker.RunningTasks, ok)
	}
}

// TestPollTaskBatchDoesNotDispatchFutureActorCommand verifies batch polling still
// honors actor command sequence ordering.
func TestPollTaskBatchDoesNotDispatchFutureActorCommand(t *testing.T) {
	t.Setenv("LOGSERVE_SCHEDULER_V2", "0")
	meta := metadata.NewMemoryStore()
	meta.UpsertWorker(metadata.Worker{WorkerID: "worker-1", Capacity: 2, LastHeartbeat: actor.NowMs()})
	state := actor.NewState("actor-batch-order", "Counter", "class Counter: pass", []byte(`{"args":[],"kwargs":{}}`), 10, actor.NowMs())
	state.OwnerWorkerID = "worker-1"
	state.Epoch = 1
	if _, duplicate := meta.CreateActor(state, ""); duplicate {
		t.Fatal("unexpected duplicate actor")
	}
	service := NewServiceWithResultStore(meta, acceptingLogClient{}, nil, 0)
	for seq := 1; seq <= 2; seq++ {
		seq := seq
		_, _, err := service.enqueueTaskWithMetadata(context.Background(), &logservepb.TaskSpec{
			TaskId:            fmt.Sprintf("actor-call-%d", seq),
			TaskName:          "actor:inc",
			FunctionName:      "inc",
			ArgsJson:          []byte(`{"args":[],"kwargs":{}}`),
			IdempotencyKey:    fmt.Sprintf("actor-call-%d", seq),
			TargetWorkerId:    "worker-1",
			ActorId:           state.ActorID,
			ActorCallId:       fmt.Sprintf("actor-call-%d", seq),
			ActorClassName:    state.ClassName,
			ActorClassSource:  state.ClassSource,
			ActorMethod:       "inc",
			ActorInitArgsJson: state.InitArgsJSON,
			ActorEpoch:        state.Epoch,
		}, func(task *metadata.Task) {
			task.ActorCommandSeq = uint64(seq)
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	poll, err := service.PollTask(context.Background(), &logservepb.PollTaskRequest{WorkerId: "worker-1", MaxTasks: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(poll.GetTasks()); got != 1 {
		t.Fatalf("batched actor tasks = %d, want only the ready first command", got)
	}
	if poll.GetTasks()[0].GetTaskId() != "actor-call-1" {
		t.Fatalf("dispatched actor task = %s, want actor-call-1", poll.GetTasks()[0].GetTaskId())
	}
	future, ok := meta.GetTask("actor-call-2")
	if !ok || future.Status != logservepb.TaskStatus_TASK_STATUS_QUEUED {
		t.Fatalf("future actor command status = %v ok=%v, want QUEUED", future.Status, ok)
	}
}

// TestPollTaskLongPollReturnsWhenTaskArrives verifies long-poll waiters wake when a
// task is enqueued.
func TestPollTaskLongPollReturnsWhenTaskArrives(t *testing.T) {
	t.Setenv("LOGSERVE_SCHEDULER_V2", "0")
	meta := metadata.NewMemoryStore()
	meta.UpsertWorker(metadata.Worker{WorkerID: "worker-1", Capacity: 2, LastHeartbeat: time.Now().UnixMilli()})
	service := NewServiceWithResultStore(meta, acceptingLogClient{}, nil, 0)

	resultCh := make(chan *logservepb.PollTaskResponse, 1)
	errCh := make(chan error, 1)
	start := time.Now()
	go func() {
		poll, err := service.PollTask(context.Background(), &logservepb.PollTaskRequest{WorkerId: "worker-1", MaxTasks: 2, WaitTimeoutMs: 500})
		if err != nil {
			errCh <- err
			return
		}
		resultCh <- poll
	}()
	time.Sleep(50 * time.Millisecond)
	task := enqueueBatchTestTask(t, service, "long-poll-task")

	select {
	case err := <-errCh:
		t.Fatal(err)
	case poll := <-resultCh:
		if elapsed := time.Since(start); elapsed >= 450*time.Millisecond {
			t.Fatalf("long poll returned after %s, want wake before timeout", elapsed)
		}
		if got := len(poll.GetTasks()); got != 1 || poll.GetTasks()[0].GetTaskId() != task.TaskID {
			t.Fatalf("long poll tasks = %v, want %s", poll.GetTasks(), task.TaskID)
		}
	case <-time.After(time.Second):
		t.Fatal("long poll did not return after task enqueue")
	}
}

// TestPollTaskLongPollTimeoutReturnsEmpty verifies idle long-poll requests return an
// empty response after the bounded timeout.
func TestPollTaskLongPollTimeoutReturnsEmpty(t *testing.T) {
	meta := metadata.NewMemoryStore()
	meta.UpsertWorker(metadata.Worker{WorkerID: "worker-1", Capacity: 1, LastHeartbeat: time.Now().UnixMilli()})
	service := NewServiceWithResultStore(meta, acceptingLogClient{}, nil, 0)

	start := time.Now()
	poll, err := service.PollTask(context.Background(), &logservepb.PollTaskRequest{WorkerId: "worker-1", WaitTimeoutMs: 50})
	if err != nil {
		t.Fatal(err)
	}
	if poll.GetHasTask() || len(poll.GetTasks()) != 0 {
		t.Fatalf("poll = %+v, want empty timeout response", poll)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Fatalf("long poll returned after %s, want it to wait near timeout", elapsed)
	}
}

// enqueueBatchTestTasks creates a sequence of queued task records for polling tests.
func enqueueBatchTestTasks(t *testing.T, service *Service, count int) []metadata.Task {
	t.Helper()
	tasks := make([]metadata.Task, 0, count)
	for i := 0; i < count; i++ {
		tasks = append(tasks, enqueueBatchTestTask(t, service, fmt.Sprintf("batch-task-%d", i+1)))
	}
	return tasks
}

// enqueueBatchTestTask submits one deterministic task through the normal enqueue path.
func enqueueBatchTestTask(t *testing.T, service *Service, taskID string) metadata.Task {
	t.Helper()
	task, _, err := service.enqueueTask(context.Background(), &logservepb.TaskSpec{
		TaskId:         taskID,
		TaskName:       taskID,
		FunctionName:   "run",
		FunctionSource: "def run(): return 1",
		ArgsJson:       []byte(`{"args":[],"kwargs":{}}`),
		IdempotencyKey: taskID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}
