package control

import (
	"fmt"
	"testing"
	"time"

	"github.com/logserve/logserve/internal/metadata"
)

// llmPlacementScheduler builds worker placement state for locality and prediction benchmarks.
func llmPlacementScheduler(workers int, cachedFraction float64) *Scheduler {
	scheduler := newScheduler()
	for i := 0; i < workers; i++ {
		cached := map[string]bool{}
		if float64(i)/float64(workers) < cachedFraction {
			cached[metadata.ModelKey("model-A", "v1")] = true
		}
		scheduler.UpsertWorker(metadata.Worker{
			WorkerID:      fmt.Sprintf("worker-%d", i),
			CachedModels:  cached,
			Capacity:      4,
			RunningTasks:  uint32(i % 3),
			LastHeartbeat: time.Now().UnixMilli(),
		})
		if cached[metadata.ModelKey("model-A", "v1")] {
			scheduler.UpdateLLMStats("model-A", "v1", fmt.Sprintf("worker-%d", i), llmWorkerStats{
				RequestCount:       10,
				EWMATotalLatencyMs: int64(20 + i%5),
				EWMAModelLoadMs:    5,
			})
		}
	}
	scheduler.Enqueue(SchedMeta{TaskID: "seed-llm", ModelName: "model-A", ModelVersion: "v1", CreatedAtMs: 1})
	return scheduler
}

// benchmarkPreferredLocalityWorker measures indexed locality lookup cost.
func benchmarkPreferredLocalityWorker(b *testing.B, workers int) {
	scheduler := llmPlacementScheduler(workers, divCachedFraction(workers))
	key := modelKeyFromParts("model-A", "v1")
	nowMs := time.Now().UnixMilli()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if worker := scheduler.PreferredLocalityWorker(key, nowMs-50, nowMs, localityQueueWait.Milliseconds()); worker == "" {
			b.Fatal("no preferred worker")
		}
	}
}

// benchmarkPreferredLocalityWorkerNaive measures the scan-based comparison implementation.
func benchmarkPreferredLocalityWorkerNaive(b *testing.B, workers int) {
	scheduler := llmPlacementScheduler(workers, divCachedFraction(workers))
	key := modelKeyFromParts("model-A", "v1")
	nowMs := time.Now().UnixMilli()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if worker := scheduler.preferredLocalityWorkerNaive(key, nowMs-50, nowMs, localityQueueWait.Milliseconds()); worker == "" {
			b.Fatal("no preferred worker")
		}
	}
}

// benchmarkPreferredPredictedWorker measures predicted-latency placement lookup cost.
func benchmarkPreferredPredictedWorker(b *testing.B, workers int) {
	scheduler := llmPlacementScheduler(workers, divCachedFraction(workers))
	key := modelKeyFromParts("model-A", "v1")
	modelKeyStr := metadata.ModelKey("model-A", "v1")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if worker := scheduler.PreferredPredictedWorker(key, modelKeyStr, 0, localityQueueWait.Milliseconds()); worker == "" {
			b.Fatal("no predicted worker")
		}
	}
}

// divCachedFraction keeps benchmark cache density high for small worker counts and
// lower for larger counts.
func divCachedFraction(workers int) float64 {
	if workers <= 10 {
		return 0.5
	}
	return 0.2
}

// preferredLocalityWorkerNaive is the benchmark baseline that scans every worker
// instead of using the placement index.
func (s *Scheduler) preferredLocalityWorkerNaive(key modelKey, createdAtMs, nowMs int64, waitMs int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	queueDelayMs := int64(0)
	if createdAtMs > 0 && nowMs > createdAtMs {
		queueDelayMs = nowMs - createdAtMs
	}
	hasCachedAvailable := false
	for workerID := range s.placement[key].cachedWorkers {
		worker, ok := s.workerViews[workerID]
		if ok && worker.hasCapacity() {
			hasCachedAvailable = true
			break
		}
	}
	bestWorker := ""
	bestScore := int64(-1 << 60)
	for _, workerID := range sortedWorkerIDs(s.workerViews) {
		worker := s.workerViews[workerID]
		if !worker.hasCapacity() {
			continue
		}
		available := int(worker.capacity - worker.runningTasks)
		score := int64(available*100 - int(worker.runningTasks)*10)
		_, cacheHit := worker.cachedModels[key]
		if cacheHit {
			score += 1000
		}
		if !cacheHit && hasCachedAvailable && queueDelayMs < waitMs {
			score -= 1000
		}
		if bestWorker == "" || score > bestScore || (score == bestScore && worker.workerID < bestWorker) {
			bestWorker = worker.workerID
			bestScore = score
		}
	}
	return bestWorker
}

// BenchmarkPreferredLocalityWorkerPlacementIndex measures cached placement lookup at multiple sizes.
func BenchmarkPreferredLocalityWorkerPlacementIndex(b *testing.B) {
	for _, workers := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("workers=%d", workers), func(b *testing.B) {
			benchmarkPreferredLocalityWorker(b, workers)
		})
	}
}

// BenchmarkPreferredLocalityWorkerNaiveScan measures the scan baseline at multiple sizes.
func BenchmarkPreferredLocalityWorkerNaiveScan(b *testing.B) {
	for _, workers := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("workers=%d", workers), func(b *testing.B) {
			benchmarkPreferredLocalityWorkerNaive(b, workers)
		})
	}
}

// BenchmarkPreferredPredictedWorkerPlacementIndex measures predicted placement lookup at multiple sizes.
func BenchmarkPreferredPredictedWorkerPlacementIndex(b *testing.B) {
	for _, workers := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("workers=%d", workers), func(b *testing.B) {
			benchmarkPreferredPredictedWorker(b, workers)
		})
	}
}

// TestPreferredPredictedWorkerStableTieBreak verifies equal predictions choose the
// lexicographically smallest worker consistently.
func TestPreferredPredictedWorkerStableTieBreak(t *testing.T) {
	scheduler := newScheduler()
	scheduler.UpsertWorker(metadata.Worker{WorkerID: "worker-b", Capacity: 2, LastHeartbeat: 1})
	scheduler.UpsertWorker(metadata.Worker{WorkerID: "worker-a", Capacity: 2, LastHeartbeat: 1})
	scheduler.UpdateLLMStats("model-A", "v1", "worker-a", llmWorkerStats{RequestCount: 1, EWMATotalLatencyMs: 40})
	scheduler.UpdateLLMStats("model-A", "v1", "worker-b", llmWorkerStats{RequestCount: 1, EWMATotalLatencyMs: 40})
	key := modelKeyFromParts("model-A", "v1")
	first := scheduler.PreferredPredictedWorker(key, metadata.ModelKey("model-A", "v1"), 0, localityQueueWait.Milliseconds())
	second := scheduler.PreferredPredictedWorker(key, metadata.ModelKey("model-A", "v1"), 0, localityQueueWait.Milliseconds())
	if first != "worker-a" || second != "worker-a" {
		t.Fatalf("predicted worker = %s then %s, want stable worker-a", first, second)
	}
}
