package logstore

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestSegmentRollingRecoverAndReadAcrossSegments(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultOptions()
	opts.SegmentSizeBytes = 220
	opts.FsyncPolicy = FsyncAlways

	store, err := OpenWithOptions(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("x"), 128)
	for i := 0; i < 12; i++ {
		if _, _, err := store.Append(AppendRequest{
			StreamID:       "wf:rolling",
			EventType:      "StepCompleted",
			IdempotencyKey: fmt.Sprintf("step-%02d", i),
			Payload:        payload,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	logSegments := globSegments(t, dir, ".log")
	if len(logSegments) < 2 {
		t.Fatalf("log segments = %d, want at least 2", len(logSegments))
	}

	recovered, err := OpenWithOptions(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()

	records, err := recovered.Read("wf:rolling", 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 12 {
		t.Fatalf("records = %d, want 12", len(records))
	}
	for i, rec := range records {
		if rec.Seq != uint64(i+1) {
			t.Fatalf("record[%d].Seq = %d, want %d", i, rec.Seq, i+1)
		}
		if !bytes.Equal(rec.Payload, payload) {
			t.Fatalf("record[%d] payload changed", i)
		}
	}
}

func TestIndexRebuiltFromSegments(t *testing.T) {
	dir := t.TempDir()
	opts := DefaultOptions()
	opts.SegmentSizeBytes = 220
	opts.FsyncPolicy = FsyncAlways

	store, err := OpenWithOptions(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, _, err := store.Append(AppendRequest{
			StreamID:  "task:index-rebuild",
			EventType: "TaskSubmitted",
			Payload:   bytes.Repeat([]byte{byte('a' + i)}, 128),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	logSegments := globSegments(t, dir, ".log")
	for _, path := range globSegments(t, dir, ".index") {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}

	recovered, err := OpenWithOptions(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()

	records, err := recovered.Read("task:index-rebuild", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 6 {
		t.Fatalf("records = %d, want 6", len(records))
	}
	indexSegments := globSegments(t, dir, ".index")
	if len(indexSegments) != len(logSegments) {
		t.Fatalf("index segments = %d, want %d", len(indexSegments), len(logSegments))
	}
}

func TestLogicalTrimFiltersReadsAndReportsCompactableBytes(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("x"), 64)
	for i := 0; i < 5; i++ {
		if _, _, err := store.Append(AppendRequest{
			StreamID:  "actor:trim",
			EventType: "ActorCommandApplied",
			Payload:   payload,
		}); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := store.Trim("actor:trim", 4)
	if err != nil {
		t.Fatal(err)
	}
	if stats.TrimmedBeforeSeq != 4 {
		t.Fatalf("trimmed_before_seq = %d, want 4", stats.TrimmedBeforeSeq)
	}
	if stats.CompactableRecords != 3 {
		t.Fatalf("compactable_records = %d, want 3", stats.CompactableRecords)
	}
	if stats.CompactableBytes == 0 {
		t.Fatal("compactable bytes should be > 0")
	}

	records, err := store.Read("actor:trim", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	if records[0].Seq != 4 || records[1].Seq != 5 {
		t.Fatalf("visible seqs = %d,%d want 4,5", records[0].Seq, records[1].Seq)
	}

	statsList := store.Stats("actor:trim", "")
	if len(statsList) != 1 {
		t.Fatalf("stats streams = %d, want 1", len(statsList))
	}
	if statsList[0].CompactableRecords != 3 || statsList[0].CompactableBytes != stats.CompactableBytes {
		t.Fatalf("stats = %+v, want compactable records/bytes from trim response %+v", statsList[0], stats)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	records, err = recovered.Read("actor:trim", 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].Seq != 4 {
		t.Fatalf("recovered visible records = %+v, want seq >= 4", records)
	}
	if recovered.Stats("actor:trim", "")[0].TrimmedBeforeSeq != 4 {
		t.Fatalf("trim point was not recovered")
	}
}

func TestFsyncPoliciesAppendAndRecover(t *testing.T) {
	for _, policy := range []FsyncPolicy{FsyncBatch, FsyncInterval} {
		t.Run(string(policy), func(t *testing.T) {
			dir := t.TempDir()
			opts := DefaultOptions()
			opts.FsyncPolicy = policy
			opts.FsyncInterval = time.Millisecond

			store, err := OpenWithOptions(dir, opts)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.Append(AppendRequest{
				StreamID:  "task:fsync",
				EventType: "TaskSubmitted",
				Payload:   []byte(`{"ok":true}`),
			}); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			recovered, err := OpenWithOptions(dir, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer recovered.Close()
			records, err := recovered.Read("task:fsync", 1, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 1 {
				t.Fatalf("records = %d, want 1", len(records))
			}
		})
	}
}

func TestOpenRejectsInvalidFsyncPolicy(t *testing.T) {
	opts := DefaultOptions()
	opts.FsyncPolicy = "sometimes"
	if _, err := OpenWithOptions(t.TempDir(), opts); err == nil {
		t.Fatal("OpenWithOptions accepted invalid fsync policy")
	}
}

func globSegments(t *testing.T, dir, ext string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "segment-*"+ext))
	if err != nil {
		t.Fatal(err)
	}
	return paths
}
