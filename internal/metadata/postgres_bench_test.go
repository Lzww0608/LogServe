package metadata

// This file benchmarks synchronous and asynchronous Postgres metadata write modes
// using the recording SQL driver from postgres_async_test.go.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
)

// BenchmarkPostgresStoreHeartbeatWriteModes compares heartbeat cost in sync and
// async persistence modes and reports timed versus post-flush SQL counts.
func BenchmarkPostgresStoreHeartbeatWriteModes(b *testing.B) {
	for _, tc := range []struct {
		name string
		mode PostgresWriteMode
	}{
		{name: "sync", mode: PostgresWriteModeSync},
		{name: "async", mode: PostgresWriteModeAsync},
	} {
		b.Run(tc.name, func(b *testing.B) {
			db, recorder := openRecordingPostgresDB(b)
			store := NewPostgresStoreWithOptions(db, PostgresOptions{
				Mode:          tc.mode,
				BatchMax:      1024,
				FlushInterval: time.Hour,
				QueueSize:     1024,
			})
			defer store.Close()
			cache := map[string]bool{"model-A:v1": true}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				store.Heartbeat("worker-bench", cache)
			}
			b.StopTimer()
			// Count SQL calls before explicit Flush so async mode can separate foreground
			// latency from eventual durable work.
			timedExecs := recorder.countAll()
			if err := store.Flush(context.Background()); err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(float64(timedExecs)/float64(b.N), "timed-sql-exec/op")
			b.ReportMetric(float64(recorder.countAll())/float64(b.N), "total-sql-exec/op")
		})
	}
}

// BenchmarkPostgresStoreTaskLifecycleWriteModes compares create/lease/complete
// task writes in sync and async persistence modes.
func BenchmarkPostgresStoreTaskLifecycleWriteModes(b *testing.B) {
	for _, tc := range []struct {
		name string
		mode PostgresWriteMode
	}{
		{name: "sync", mode: PostgresWriteModeSync},
		{name: "async", mode: PostgresWriteModeAsync},
	} {
		b.Run(tc.name, func(b *testing.B) {
			db, recorder := openRecordingPostgresDB(b)
			store := NewPostgresStoreWithOptions(db, PostgresOptions{
				Mode:          tc.mode,
				BatchMax:      max(1024, b.N*4),
				FlushInterval: time.Hour,
				QueueSize:     max(1024, b.N*4),
			})
			defer store.Close()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				taskID := fmt.Sprintf("task-lifecycle-%d", i)
				created, duplicate := store.CreateTask(Task{
					TaskID:   taskID,
					TaskName: "benchmark",
					Status:   logservepb.TaskStatus_TASK_STATUS_QUEUED,
				}, "")
				if duplicate {
					b.Fatalf("unexpected duplicate task %s", taskID)
				}
				leased, err := store.LeaseTask(created.TaskID, "worker-bench")
				if err != nil {
					b.Fatal(err)
				}
				if _, err := store.CompleteTask(leased.TaskID, leased.WorkerID, leased.TaskLeaseEpoch, logservepb.TaskStatus_TASK_STATUS_SUCCEEDED, nil, ""); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			// Count SQL calls before explicit Flush so async mode can separate foreground
			// latency from eventual durable work.
			timedExecs := recorder.countAll()
			if err := store.Flush(context.Background()); err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(float64(timedExecs)/float64(b.N), "timed-sql-exec/op")
			b.ReportMetric(float64(recorder.countAll())/float64(b.N), "total-sql-exec/op")
		})
	}
}
