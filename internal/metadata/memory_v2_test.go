package metadata

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/workflow"
)

func TestMemoryStoreV2TaskIndexesFollowStatusTransitions(t *testing.T) {
	store := NewMemoryStore()
	created, duplicate := store.CreateTask(Task{
		TaskID:   "task-indexed",
		TaskName: "indexed",
		Status:   logservepb.TaskStatus_TASK_STATUS_QUEUED,
	}, "")
	if duplicate {
		t.Fatal("unexpected duplicate")
	}
	if queued := store.ListTasksByStatus(logservepb.TaskStatus_TASK_STATUS_QUEUED); len(queued) != 1 || queued[0].TaskID != created.TaskID {
		t.Fatalf("queued index = %#v, want created task", queued)
	}

	leased, err := store.LeaseTask(created.TaskID, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	if queued := store.ListTasksByStatus(logservepb.TaskStatus_TASK_STATUS_QUEUED); len(queued) != 0 {
		t.Fatalf("queued index length = %d, want 0", len(queued))
	}
	if running := store.ListTasksByStatus(logservepb.TaskStatus_TASK_STATUS_RUNNING); len(running) != 1 || running[0].TaskID != created.TaskID {
		t.Fatalf("running index = %#v, want leased task", running)
	}
	store.tasks.indexMu.RLock()
	_, workerIndexed := store.tasks.byWorker["worker-1"][created.TaskID]
	store.tasks.indexMu.RUnlock()
	if !workerIndexed {
		t.Fatal("running task missing from worker index")
	}

	if _, err := store.CompleteTask(leased.TaskID, leased.WorkerID, leased.TaskLeaseEpoch, logservepb.TaskStatus_TASK_STATUS_SUCCEEDED, []byte(`{"ok":true}`), ""); err != nil {
		t.Fatal(err)
	}
	if running := store.ListTasksByStatus(logservepb.TaskStatus_TASK_STATUS_RUNNING); len(running) != 0 {
		t.Fatalf("running index length = %d, want 0", len(running))
	}
	if succeeded := store.ListTasksByStatus(logservepb.TaskStatus_TASK_STATUS_SUCCEEDED); len(succeeded) != 1 || succeeded[0].TaskID != created.TaskID {
		t.Fatalf("succeeded index = %#v, want completed task", succeeded)
	}
	store.tasks.indexMu.RLock()
	_, workerIndexed = store.tasks.byWorker["worker-1"][created.TaskID]
	store.tasks.indexMu.RUnlock()
	if workerIndexed {
		t.Fatal("completed task still present in running worker index")
	}
}

func TestMemoryStoreV2DeadlineHeapRequeuesOnlyExpiredRunningTasks(t *testing.T) {
	store := NewMemoryStore()
	oldTask, _ := store.CreateTask(Task{TaskID: "old-running", TaskName: "old", Status: logservepb.TaskStatus_TASK_STATUS_QUEUED}, "")
	freshTask, _ := store.CreateTask(Task{TaskID: "fresh-running", TaskName: "fresh", Status: logservepb.TaskStatus_TASK_STATUS_QUEUED}, "")
	oldLease, err := store.LeaseTask(oldTask.TaskID, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	freshLease, err := store.LeaseTask(freshTask.TaskID, "worker-2")
	if err != nil {
		t.Fatal(err)
	}
	expireLeaseForTest(t, store, oldLease.TaskID, oldLease.TaskLeaseEpoch, time.Minute)

	requeued := store.RequeueExpiredRunningTasks(time.Second)
	if len(requeued) != 1 || requeued[0].TaskID != oldTask.TaskID || requeued[0].WorkerID != "worker-1" {
		t.Fatalf("requeued = %#v, want old task with previous worker", requeued)
	}
	oldCurrent, _ := store.GetTask(oldLease.TaskID)
	if oldCurrent.Status != logservepb.TaskStatus_TASK_STATUS_QUEUED || oldCurrent.WorkerID != "" {
		t.Fatalf("old task state = %s/%q, want queued/no worker", oldCurrent.Status, oldCurrent.WorkerID)
	}
	freshCurrent, _ := store.GetTask(freshLease.TaskID)
	if freshCurrent.Status != logservepb.TaskStatus_TASK_STATUS_RUNNING || freshCurrent.WorkerID != "worker-2" {
		t.Fatalf("fresh task state = %s/%q, want running/worker-2", freshCurrent.Status, freshCurrent.WorkerID)
	}
}

func TestMemoryStoreV2RejectsStaleCompletionAfterTargetedRequeue(t *testing.T) {
	store := NewMemoryStore()
	created, _ := store.CreateTask(Task{TaskID: "targeted-requeue", TaskName: "targeted", Status: logservepb.TaskStatus_TASK_STATUS_QUEUED}, "")
	leased, err := store.LeaseTask(created.TaskID, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	if _, requeued := store.RequeueTaskIfLeaseExpired(leased.TaskID, leased.TaskLeaseEpoch, time.Nanosecond); !requeued {
		t.Fatal("targeted requeue did not requeue expired lease")
	}
	_, err = store.CompleteTask(leased.TaskID, "worker-1", leased.TaskLeaseEpoch, logservepb.TaskStatus_TASK_STATUS_SUCCEEDED, []byte(`"stale"`), "")
	if err == nil || !strings.Contains(err.Error(), "stale task lease") {
		t.Fatalf("CompleteTask error = %v, want stale task lease", err)
	}
}

func TestMemoryStoreV2SnapshotsAreImmutable(t *testing.T) {
	store := NewMemoryStore()
	store.CreateTask(Task{TaskID: "task-snapshot", TaskName: "snapshot", Status: logservepb.TaskStatus_TASK_STATUS_QUEUED, ResultJSON: []byte(`{"value":1}`)}, "")
	task, ok := store.GetTask("task-snapshot")
	if !ok {
		t.Fatal("task missing")
	}
	task.ResultJSON[0] = '['
	again, _ := store.GetTask("task-snapshot")
	if string(again.ResultJSON) != `{"value":1}` {
		t.Fatalf("task result snapshot mutated store: %s", again.ResultJSON)
	}

	store.UpsertWorker(Worker{WorkerID: "worker-snapshot", Labels: map[string]string{"zone": "a"}, CachedModels: map[string]bool{"model:v1": true}, Capacity: 1})
	worker, _ := store.GetWorker("worker-snapshot")
	worker.Labels["zone"] = "b"
	worker.CachedModels["model:v1"] = false
	workerAgain, _ := store.GetWorker("worker-snapshot")
	if workerAgain.Labels["zone"] != "a" || !workerAgain.CachedModels["model:v1"] {
		t.Fatalf("worker snapshot mutated store: %#v", workerAgain)
	}

	state := benchmarkWorkflowState("workflow-snapshot", 2)
	step := state.Steps["step-0"]
	step.ResultJSON = []byte(`{"step":1}`)
	state.Steps["step-0"] = step
	store.UpsertWorkflow(state)
	workflowState, _ := store.GetWorkflow("workflow-snapshot")
	workflowState.Steps["step-0"] = workflow.StepState{StepID: "step-0", ResultJSON: []byte(`bad`)}
	workflowAgain, _ := store.GetWorkflow("workflow-snapshot")
	if string(workflowAgain.Steps["step-0"].ResultJSON) != `{"step":1}` {
		t.Fatalf("workflow step snapshot mutated store: %#v", workflowAgain.Steps["step-0"])
	}
}

func TestMemoryStoreV2WorkflowRecordUsesSliceIndex(t *testing.T) {
	store := NewMemoryStore()
	state := benchmarkWorkflowState("workflow-index", 3)
	store.UpsertWorkflow(state)

	store.workflows.mu.RLock()
	record := store.workflows.states["workflow-index"]
	store.workflows.mu.RUnlock()
	if record == nil {
		t.Fatal("workflow record missing")
	}
	if record.state.Steps != nil {
		t.Fatal("workflow record stores public step map internally")
	}
	if len(record.steps) != 3 || len(record.stepIndex) != 3 {
		t.Fatalf("steps/index sizes = %d/%d, want 3/3", len(record.steps), len(record.stepIndex))
	}
	if idx, ok := record.stepIndex["step-1"]; !ok || record.steps[idx].StepID != "step-1" {
		t.Fatalf("step index for step-1 = %d/%v", idx, ok)
	}
}

func TestMemoryStoreV2ConcurrentHeartbeatLeaseComplete(t *testing.T) {
	store := NewMemoryStore()
	for i := 0; i < 256; i++ {
		store.CreateTask(Task{TaskID: benchmarkTaskID(i), TaskName: "concurrent", Status: logservepb.TaskStatus_TASK_STATUS_QUEUED}, "")
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		workerID := benchmarkWorkerIDs(8)[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				store.Heartbeat(workerID, map[string]bool{"model:v1": true})
			}
		}()
	}
	for i := 0; i < 256; i++ {
		taskID := benchmarkTaskID(i)
		workerID := benchmarkWorkerIDs(8)[i%8]
		wg.Add(1)
		go func() {
			defer wg.Done()
			leased, err := store.LeaseTask(taskID, workerID)
			if err != nil {
				t.Errorf("lease %s: %v", taskID, err)
				return
			}
			if _, err := store.CompleteTask(leased.TaskID, leased.WorkerID, leased.TaskLeaseEpoch, logservepb.TaskStatus_TASK_STATUS_SUCCEEDED, nil, ""); err != nil {
				t.Errorf("complete %s: %v", taskID, err)
			}
		}()
	}
	wg.Wait()

	if succeeded := store.ListTasksByStatus(logservepb.TaskStatus_TASK_STATUS_SUCCEEDED); len(succeeded) != 256 {
		t.Fatalf("succeeded tasks = %d, want 256", len(succeeded))
	}
}

func expireLeaseForTest(t *testing.T, store *MemoryStoreV2, taskID string, leaseEpoch uint64, age time.Duration) {
	t.Helper()
	shard := store.tasks.shard(taskID)
	shard.mu.Lock()
	task, ok := shard.tasks[taskID]
	if !ok {
		shard.mu.Unlock()
		t.Fatalf("task %s missing", taskID)
	}
	if task.TaskLeaseEpoch != leaseEpoch {
		shard.mu.Unlock()
		t.Fatalf("task lease epoch = %d, want %d", task.TaskLeaseEpoch, leaseEpoch)
	}
	task.UpdatedAtMs = time.Now().Add(-age).UnixMilli()
	expired := cloneTask(*task)
	shard.mu.Unlock()
	store.tasks.trackRunning(expired)
}
func benchmarkTaskID(i int) string {
	return "concurrent-task-" + string(rune('a'+(i%26))) + "-" + benchmarkWorkerIDs(256)[i]
}
