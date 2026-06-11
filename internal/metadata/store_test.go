package metadata

import (
	"testing"
	"time"
)

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

func TestPostgresStoreImplementsStore(t *testing.T) {
	var _ Store = (*PostgresStore)(nil)
}

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
