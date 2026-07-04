package metadata

// This file contains interface and compatibility tests that keep metadata Store
// implementations aligned on core behavior.

import (
	"strings"
	"testing"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
)

// TestMemoryStoreImplementsStore verifies the default store satisfies Store and
// preserves task idempotency semantics.
func TestMemoryStoreImplementsStore(t *testing.T) {
	var store Store = NewMemoryStore()

	created, duplicate := store.CreateTask(Task{TaskID: "task-1", TaskName: "demo"}, "idem-1")
	if duplicate {
		t.Fatal("first CreateTask reported duplicate")
	}
	if created.TaskID != "task-1" {
		t.Fatalf("TaskID = %q, want task-1", created.TaskID)
	}

	again, duplicate := store.CreateTask(Task{TaskID: "task-2", TaskName: "demo"}, "idem-1")
	if !duplicate {
		t.Fatal("second CreateTask should be idempotent duplicate")
	}
	if again.TaskID != "task-1" {
		t.Fatalf("duplicate TaskID = %q, want task-1", again.TaskID)
	}
}

// TestPostgresStoreImplementsStore keeps the durable wrapper bound to the Store
// interface at compile time.
func TestPostgresStoreImplementsStore(t *testing.T) {
	var _ Store = (*PostgresStore)(nil)
}

// TestUpsertWorkerPreservesExplicitHeartbeat protects the bootstrap path where
// restored worker timestamps must not be overwritten by insertion time.
func TestUpsertWorkerPreservesExplicitHeartbeat(t *testing.T) {
	store := NewMemoryStore()
	oldHeartbeat := time.Now().Add(-10 * time.Minute).UnixMilli()

	store.UpsertWorker(Worker{
		WorkerID:      "old-worker",
		CachedModels:  map[string]bool{"model-A:v1": true},
		Capacity:      1,
		LastHeartbeat: oldHeartbeat,
	})

	worker, ok := store.GetWorker("old-worker")
	if !ok {
		t.Fatal("worker missing")
	}
	if worker.LastHeartbeat != oldHeartbeat {
		t.Fatalf("last heartbeat = %d, want explicit %d", worker.LastHeartbeat, oldHeartbeat)
	}
	if active := store.ActiveWorkers(time.Second); len(active) != 0 {
		t.Fatalf("active workers = %d, want 0 for stale bootstrapped worker", len(active))
	}
}

// TestCompleteTaskRejectsQueuedExpiredLease verifies a worker cannot complete a
// task after lease recovery moved it back to QUEUED.
func TestCompleteTaskRejectsQueuedExpiredLease(t *testing.T) {
	store := NewMemoryStore()
	created, duplicate := store.CreateTask(Task{
		TaskID:   "task-expired-lease",
		TaskName: "expired",
		Status:   logservepb.TaskStatus_TASK_STATUS_QUEUED,
	}, "")
	if duplicate {
		t.Fatal("unexpected duplicate task")
	}
	leased, err := store.LeaseTask(created.TaskID, "worker-1")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	store.RequeueExpiredRunningTasks(time.Nanosecond)

	_, err = store.CompleteTask(created.TaskID, "worker-1", leased.TaskLeaseEpoch, logservepb.TaskStatus_TASK_STATUS_SUCCEEDED, []byte(`"stale"`), "")
	if err == nil || !strings.Contains(err.Error(), "stale task lease") {
		t.Fatalf("CompleteTask error = %v, want stale task lease", err)
	}
	current, ok := store.GetTask(created.TaskID)
	if !ok {
		t.Fatal("task missing")
	}
	if current.Status != logservepb.TaskStatus_TASK_STATUS_QUEUED {
		t.Fatalf("status = %s, want QUEUED", current.Status)
	}
	if len(current.ResultJSON) != 0 {
		t.Fatalf("result_json = %s, want empty", current.ResultJSON)
	}
}
