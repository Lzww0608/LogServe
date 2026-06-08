package logstore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAppendReadAndIdempotency(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	rec, duplicate, err := store.Append(AppendRequest{
		StreamID:       "task:1",
		EventType:      "TaskSubmitted",
		IdempotencyKey: "submit-1",
		Payload:        []byte(`{"x":1}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if duplicate {
		t.Fatal("first append reported duplicate")
	}
	if rec.Seq != 1 {
		t.Fatalf("seq = %d, want 1", rec.Seq)
	}

	again, duplicate, err := store.Append(AppendRequest{
		StreamID:       "task:1",
		EventType:      "TaskSubmitted",
		IdempotencyKey: "submit-1",
		Payload:        []byte(`{"x":2}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !duplicate {
		t.Fatal("second append should be duplicate")
	}
	if again.Seq != rec.Seq {
		t.Fatalf("duplicate seq = %d, want %d", again.Seq, rec.Seq)
	}

	records, err := store.Read("task:1", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
}

func TestRecoveryTruncatesPartialTail(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Append(AppendRequest{
		StreamID:  "task:2",
		EventType: "TaskSubmitted",
		Payload:   []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(dir, "segment-00000001.log")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()

	records, err := recovered.Read("task:2", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
}
