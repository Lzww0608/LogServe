package metadata

import "testing"

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
