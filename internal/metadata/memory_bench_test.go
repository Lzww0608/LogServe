package metadata

// This file benchmarks the legacy and V2 memory stores on task, worker, and
// workflow hot paths.

import (
	"fmt"
	"runtime"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/workflow"
)

// init enables mutex and block profiling so benchmark runs can expose lock
// contention differences between store implementations.
func init() {
	runtime.SetMutexProfileFraction(1)
	runtime.SetBlockProfileRate(10000)
}

// BenchmarkMemoryStoreConcurrentGetTask measures concurrent read throughput for
// task snapshots.
func BenchmarkMemoryStoreConcurrentGetTask(b *testing.B) {
	for _, factory := range memoryStoreBenchmarkFactories() {
		b.Run(factory.name, func(b *testing.B) {
			store := factory.newStore()
			ids := seedBenchmarkTasks(b, store, 10000)
			// seq distributes reads across preseeded IDs without a shared random source.
			var seq uint64
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					idx := int(atomic.AddUint64(&seq, 1)-1) % len(ids)
					if _, ok := store.GetTask(ids[idx]); !ok {
						b.Fatalf("task %s missing", ids[idx])
					}
				}
			})
		})
	}
}

// BenchmarkMemoryStoreConcurrentLeaseComplete measures the lease and completion
// path under parallel workers.
func BenchmarkMemoryStoreConcurrentLeaseComplete(b *testing.B) {
	workers := benchmarkWorkerIDs(128)
	for _, factory := range memoryStoreBenchmarkFactories() {
		b.Run(factory.name, func(b *testing.B) {
			store := factory.newStore()
			ids := seedBenchmarkTasks(b, store, b.N)
			// seq assigns each parallel worker a unique preseeded task lifecycle.
			var seq uint64
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					idx := int(atomic.AddUint64(&seq, 1) - 1)
					if idx >= len(ids) {
						return
					}
					leased, err := store.LeaseTask(ids[idx], workers[idx%len(workers)])
					if err != nil {
						b.Fatal(err)
					}
					if _, err := store.CompleteTask(leased.TaskID, leased.WorkerID, leased.TaskLeaseEpoch, logservepb.TaskStatus_TASK_STATUS_SUCCEEDED, nil, ""); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}

// BenchmarkMemoryStoreConcurrentHeartbeat measures worker heartbeat throughput
// with changing cached-model sets.
func BenchmarkMemoryStoreConcurrentHeartbeat(b *testing.B) {
	workers := benchmarkWorkerIDs(256)
	cacheSets := []map[string]bool{
		{"model-A:v1": true},
		{"model-B:v1": true, "model-C:v2": true},
		{"model-D:v1": true},
	}
	for _, factory := range memoryStoreBenchmarkFactories() {
		b.Run(factory.name, func(b *testing.B) {
			store := factory.newStore()
			// seq rotates workers and cache sets without adding a shared random source.
			var seq uint64
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					idx := int(atomic.AddUint64(&seq, 1) - 1)
					store.Heartbeat(workers[idx%len(workers)], cacheSets[idx%len(cacheSets)])
				}
			})
		})
	}
}

// BenchmarkMemoryStoreHeartbeatUnderCompleteP99 samples heartbeat latency while
// other goroutines lease and complete tasks.
func BenchmarkMemoryStoreHeartbeatUnderCompleteP99(b *testing.B) {
	const batchOps = 8
	workers := benchmarkWorkerIDs(128)
	cacheSet := map[string]bool{"model-A:v1": true}
	for _, factory := range memoryStoreBenchmarkFactories() {
		b.Run(factory.name, func(b *testing.B) {
			store := factory.newStore()
			// Preseed enough tasks for lease/complete bursts so setup does not occur in
			// the timed section.
			ids := seedBenchmarkTasks(b, store, b.N*batchOps)
			samples := make([]int64, b.N)
			var opSeq uint64
			var taskSeq uint64
			var sampleSeq uint64
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					opIdx := int(atomic.AddUint64(&opSeq, 1) - 1)
					workerID := workers[opIdx%len(workers)]
					// Every fourth operation injects write pressure while the other workers
					// sample heartbeat latency under contention.
					if opIdx%4 == 0 {
						base := int(atomic.AddUint64(&taskSeq, batchOps) - batchOps)
						for j := 0; j < batchOps && base+j < len(ids); j++ {
							leased, err := store.LeaseTask(ids[base+j], workerID)
							if err != nil {
								b.Fatal(err)
							}
							if _, err := store.CompleteTask(leased.TaskID, leased.WorkerID, leased.TaskLeaseEpoch, logservepb.TaskStatus_TASK_STATUS_SUCCEEDED, nil, ""); err != nil {
								b.Fatal(err)
							}
						}
						continue
					}
					sampleIdx := int(atomic.AddUint64(&sampleSeq, 1) - 1)
					started := time.Now()
					for j := 0; j < batchOps; j++ {
						store.Heartbeat(workerID, cacheSet)
					}
					if sampleIdx < len(samples) {
						samples[sampleIdx] = time.Since(started).Nanoseconds()
					}
				}
			})
			b.StopTimer()
			used := int(atomic.LoadUint64(&sampleSeq))
			if used > len(samples) {
				used = len(samples)
			}

			// Report a custom p99 metric over sampled heartbeat batches instead of relying
			// only on aggregate ns/op, which hides contention spikes.
			if used > 0 {
				observed := samples[:used]
				sort.Slice(observed, func(i, j int) bool { return observed[i] < observed[j] })
				p99Index := int(float64(used-1) * 0.99)
				b.ReportMetric(float64(observed[p99Index]), "heartbeat-p99-batch-ns")
			}
		})
	}
}

// BenchmarkMemoryStoreActiveWorkers measures scheduler scans over many registered
// workers.
func BenchmarkMemoryStoreActiveWorkers(b *testing.B) {
	for _, factory := range memoryStoreBenchmarkFactories() {
		b.Run(factory.name, func(b *testing.B) {
			store := factory.newStore()
			for _, workerID := range benchmarkWorkerIDs(1000) {
				store.UpsertWorker(Worker{
					WorkerID:      workerID,
					CachedModels:  map[string]bool{"model-A:v1": true},
					Capacity:      4,
					LastHeartbeat: time.Now().UnixMilli(),
				})
			}
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					_ = store.ActiveWorkers(time.Minute)
				}
			})
		})
	}
}

// BenchmarkMemoryStoreUpdateWorkflow measures concurrent workflow state mutation
// and cloning costs.
func BenchmarkMemoryStoreUpdateWorkflow(b *testing.B) {
	for _, factory := range memoryStoreBenchmarkFactories() {
		b.Run(factory.name, func(b *testing.B) {
			store := factory.newStore()
			workflowIDs := make([]string, 64)
			for i := range workflowIDs {
				workflowIDs[i] = fmt.Sprintf("workflow-%d", i)
				store.UpsertWorkflow(benchmarkWorkflowState(workflowIDs[i], 16))
			}
			// seq spreads mutations across workflow IDs without a shared random source.
			var seq uint64
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					idx := int(atomic.AddUint64(&seq, 1)-1) % len(workflowIDs)
					if _, err := store.UpdateWorkflow(workflowIDs[idx], func(current *workflow.State) error {
						current.UpdateStep("step-0", func(step *workflow.StepState) {
							step.Attempts++
						})
						return nil
					}); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}

// memoryStoreBenchmarkFactory lets every benchmark compare legacy and V2 stores
// with identical setup.
type memoryStoreBenchmarkFactory struct {
	name     string
	newStore func() Store
}

// memoryStoreBenchmarkFactories returns the store implementations under benchmark.
func memoryStoreBenchmarkFactories() []memoryStoreBenchmarkFactory {
	return []memoryStoreBenchmarkFactory{
		{name: "legacy", newStore: func() Store { return NewLegacyMemoryStore() }},
		{name: "v2", newStore: func() Store { return NewMemoryStore() }},
	}
}

// seedBenchmarkTasks creates queued tasks before benchmark timers start.
func seedBenchmarkTasks(b *testing.B, store Store, n int) []string {
	b.Helper()
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		ids[i] = fmt.Sprintf("task-%d", i)
		if _, duplicate := store.CreateTask(Task{
			TaskID:   ids[i],
			TaskName: "benchmark",
			Status:   logservepb.TaskStatus_TASK_STATUS_QUEUED,
		}, ""); duplicate {
			b.Fatalf("unexpected duplicate for %s", ids[i])
		}
	}
	return ids
}

// benchmarkWorkerIDs returns deterministic worker IDs for tests and benchmarks.
func benchmarkWorkerIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("worker-%d", i)
	}
	return ids
}

// benchmarkWorkflowState builds a deterministic workflow state with the requested
// number of steps.
func benchmarkWorkflowState(workflowID string, steps int) workflow.State {
	// Use a linear dependency-free graph so benchmarks measure store cloning and
	// mutation costs instead of workflow validation complexity.
	defs := make([]workflow.StepDefinition, steps)
	for i := range defs {
		defs[i] = workflow.StepDefinition{StepID: fmt.Sprintf("step-%d", i), TaskName: "benchmark-step"}
	}
	state := workflow.NewState(workflowID, workflow.Definition{
		WorkflowName: "benchmark-workflow",
		Steps:        defs,
		ResultStepID: defs[len(defs)-1].StepID,
		MaxAttempts:  3,
		TimeoutMs:    30000,
	}, time.Now().UnixMilli())
	return state
}
