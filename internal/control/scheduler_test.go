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

// TestSchedulerAssignsGeneralFIFO verifies general tasks are assigned FIFO.
func TestSchedulerAssignsGeneralFIFO(t *testing.T) {
	scheduler := newScheduler()
	scheduler.Enqueue(SchedMeta{TaskID: "task-1", CreatedAtMs: 1})
	scheduler.Enqueue(SchedMeta{TaskID: "task-2", CreatedAtMs: 2})

	first := mustAssignScheduler(t, scheduler, workerSnapshot{WorkerID: "worker-1"})
	second := mustAssignScheduler(t, scheduler, workerSnapshot{WorkerID: "worker-1"})

	if first.TaskID != "task-1" || second.TaskID != "task-2" {
		t.Fatalf("assigned %s then %s, want FIFO task-1 then task-2", first.TaskID, second.TaskID)
	}
}

// TestSchedulerTargetWorkerPrecedesGeneralQueue verifies targeted work wins for its
// worker without blocking other workers from general work.
func TestSchedulerTargetWorkerPrecedesGeneralQueue(t *testing.T) {
	scheduler := newScheduler()
	scheduler.Enqueue(SchedMeta{TaskID: "general", CreatedAtMs: 1})
	scheduler.Enqueue(SchedMeta{TaskID: "target", TargetWorker: "worker-2", CreatedAtMs: 2})

	target := mustAssignScheduler(t, scheduler, workerSnapshot{WorkerID: "worker-2"})
	general := mustAssignScheduler(t, scheduler, workerSnapshot{WorkerID: "worker-1"})

	if target.TaskID != "target" {
		t.Fatalf("worker-2 assigned %s, want target", target.TaskID)
	}
	if general.TaskID != "general" {
		t.Fatalf("worker-1 assigned %s, want general", general.TaskID)
	}
}

// TestSchedulerActorCommandSeqGatesPerActorQueue verifies actor queues wait for the
// next expected command sequence and do not block general tasks.
func TestSchedulerActorCommandSeqGatesPerActorQueue(t *testing.T) {
	scheduler := newScheduler()
	scheduler.Enqueue(SchedMeta{TaskID: "actor-future", ActorID: "actor-1", CommandSeq: 2, CreatedAtMs: 1})
	scheduler.Enqueue(SchedMeta{TaskID: "general", CreatedAtMs: 2})

	snapshot := workerSnapshot{
		WorkerID:     "owner",
		ActorIDs:     []string{"actor-1"},
		ActorNextSeq: map[string]uint64{"actor-1": 1},
	}
	first := mustAssignScheduler(t, scheduler, snapshot)
	if first.TaskID != "general" {
		t.Fatalf("future actor command blocked general task; assigned %s", first.TaskID)
	}
	if _, ok := scheduler.Assign(snapshot, 0, schedulerAssignAll); ok {
		t.Fatal("future actor command dispatched before command_seq 1 was ready")
	}

	snapshot.ActorNextSeq["actor-1"] = 2
	ready := mustAssignScheduler(t, scheduler, snapshot)
	if ready.TaskID != "actor-future" {
		t.Fatalf("ready actor command assigned %s, want actor-future", ready.TaskID)
	}
}

// TestSchedulerPrunesEmptyActorPendingQueue verifies drained actor queues are removed
// and recreated on later enqueue.
func TestSchedulerPrunesEmptyActorPendingQueue(t *testing.T) {
	scheduler := newScheduler()
	scheduler.Enqueue(SchedMeta{TaskID: "actor-ready", ActorID: "actor-1", CommandSeq: 1, CreatedAtMs: 1})
	if scheduler.ActorPendingActors() != 1 {
		t.Fatalf("ActorPendingActors() = %d, want 1", scheduler.ActorPendingActors())
	}
	snapshot := workerSnapshot{
		WorkerID:     "owner",
		ActorIDs:     []string{"actor-1"},
		ActorNextSeq: map[string]uint64{"actor-1": 1},
	}
	if _, ok := scheduler.Assign(snapshot, 0, schedulerAssignAll); !ok {
		t.Fatal("expected actor task assignment")
	}
	if scheduler.ActorPendingActors() != 0 {
		t.Fatalf("ActorPendingActors() after drain = %d, want 0", scheduler.ActorPendingActors())
	}
	scheduler.Enqueue(SchedMeta{TaskID: "actor-next", ActorID: "actor-1", CommandSeq: 2, CreatedAtMs: 2})
	if scheduler.ActorPendingActors() != 1 {
		t.Fatalf("ActorPendingActors() after re-enqueue = %d, want 1", scheduler.ActorPendingActors())
	}
}

// TestSchedulerLLMLocalityUsesModelIndex verifies cached workers get LLM work before
// cold workers while locality wait is active.
func TestSchedulerLLMLocalityUsesModelIndex(t *testing.T) {
	scheduler := newScheduler()
	scheduler.UpsertWorker(metadata.Worker{
		WorkerID:      "cached-worker",
		CachedModels:  map[string]bool{metadata.ModelKey("model-A", "v1"): true},
		Capacity:      1,
		LastHeartbeat: 1,
	})
	scheduler.UpsertWorker(metadata.Worker{
		WorkerID:      "cold-worker",
		CachedModels:  map[string]bool{},
		Capacity:      1,
		LastHeartbeat: 1,
	})
	scheduler.Enqueue(SchedMeta{TaskID: "llm", ModelName: "model-A", ModelVersion: "v1", CreatedAtMs: 1000})
	scheduler.Enqueue(SchedMeta{TaskID: "general", CreatedAtMs: 1001})

	coldSnapshot := workerSnapshot{WorkerID: "cold-worker"}
	cold := mustAssignScheduler(t, scheduler, coldSnapshot)
	if cold.TaskID != "general" {
		t.Fatalf("cold worker assigned %s before locality wait elapsed, want general", cold.TaskID)
	}

	cachedSnapshot := workerSnapshot{
		WorkerID:     "cached-worker",
		CachedModels: map[modelKey]struct{}{modelKeyFromParts("model-A", "v1"): {}},
	}
	cached := mustAssignScheduler(t, scheduler, cachedSnapshot)
	if cached.TaskID != "llm" {
		t.Fatalf("cached worker assigned %s, want llm", cached.TaskID)
	}
}

// TestSchedulerColdWorkerCanTakeLLMAfterLocalityWait verifies cold workers can take
// LLM tasks once cached workers are unavailable or the wait has elapsed.
func TestSchedulerColdWorkerCanTakeLLMAfterLocalityWait(t *testing.T) {
	scheduler := newScheduler()
	scheduler.UpsertWorker(metadata.Worker{
		WorkerID:      "cached-worker",
		CachedModels:  map[string]bool{metadata.ModelKey("model-A", "v1"): true},
		Capacity:      1,
		RunningTasks:  1,
		LastHeartbeat: 1,
	})
	scheduler.UpsertWorker(metadata.Worker{WorkerID: "cold-worker", Capacity: 1, LastHeartbeat: 1})
	scheduler.Enqueue(SchedMeta{TaskID: "llm", ModelName: "model-A", ModelVersion: "v1", CreatedAtMs: 1000})

	cold := mustAssignScheduler(t, scheduler, workerSnapshot{WorkerID: "cold-worker"}, 1000+localityQueueWait.Milliseconds()+1)
	if cold.TaskID != "llm" {
		t.Fatalf("cold worker assigned %s after locality wait/full cache worker, want llm", cold.TaskID)
	}
}

// TestSchedulerDeadlineHeapReturnsOnlyExpiredLeases verifies stale heap entries are
// ignored and only currently tracked expired leases are returned.
func TestSchedulerDeadlineHeapReturnsOnlyExpiredLeases(t *testing.T) {
	scheduler := newScheduler()
	scheduler.TrackRunning("slow", 200, 1)
	scheduler.TrackRunning("fast", 100, 1)
	scheduler.TrackRunning("new-fast", 90, 1)
	scheduler.TrackRunning("new-fast", 300, 2)

	expired := scheduler.PopExpiredRunning(150)
	if len(expired) != 1 || expired[0].taskID != "fast" {
		t.Fatalf("expired leases = %+v, want only fast", expired)
	}
	expired = scheduler.PopExpiredRunning(250)
	if len(expired) != 1 || expired[0].taskID != "slow" {
		t.Fatalf("expired leases = %+v, want only slow", expired)
	}
	expired = scheduler.PopExpiredRunning(350)
	if len(expired) != 1 || expired[0].taskID != "new-fast" || expired[0].leaseEpoch != 2 {
		t.Fatalf("expired leases = %+v, want new-fast epoch 2", expired)
	}
}

// BenchmarkSchedulerAssignMixedBacklog measures assignment over mixed actor, LLM,
// targeted, and general queues.
func BenchmarkSchedulerAssignMixedBacklog(b *testing.B) {
	for _, depth := range []int{1000, 10000, 100000} {
		for _, workers := range []int{1, 10, 100, 1000} {
			b.Run(fmt.Sprintf("depth=%d/workers=%d", depth, workers), func(b *testing.B) {
				scheduler := mixedBacklogScheduler(depth, workers)
				snapshot := workerSnapshot{WorkerID: "general-worker"}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					meta, ok := scheduler.Assign(snapshot, int64(i), schedulerAssignAll)
					if !ok {
						b.Fatal("no task assigned")
					}
					scheduler.Enqueue(meta)
				}
			})
		}
	}
}

// mixedBacklogScheduler builds a synthetic mixed scheduler backlog for benchmarks.
func mixedBacklogScheduler(depth, workers int) *Scheduler {
	scheduler := newScheduler()
	for i := 0; i < workers; i++ {
		scheduler.UpsertWorker(metadata.Worker{
			WorkerID:      fmt.Sprintf("llm-worker-%d", i),
			CachedModels:  map[string]bool{metadata.ModelKey(fmt.Sprintf("model-%d", i), "v1"): true},
			Capacity:      1,
			LastHeartbeat: 1,
		})
	}
	for i := 0; i < depth; i++ {
		switch i % 4 {
		case 0:
			scheduler.Enqueue(SchedMeta{
				TaskID:      fmt.Sprintf("actor-%d", i),
				ActorID:     fmt.Sprintf("actor-%d", i),
				CommandSeq:  2,
				CreatedAtMs: int64(i),
			})
		case 1:
			scheduler.Enqueue(SchedMeta{
				TaskID:       fmt.Sprintf("llm-%d", i),
				ModelName:    fmt.Sprintf("model-%d", i%workers),
				ModelVersion: "v1",
				CreatedAtMs:  int64(i),
			})
		case 2:
			scheduler.Enqueue(SchedMeta{
				TaskID:       fmt.Sprintf("target-%d", i),
				TargetWorker: fmt.Sprintf("worker-%d", i%workers),
				CreatedAtMs:  int64(i),
			})
		default:
			scheduler.Enqueue(SchedMeta{
				TaskID:      fmt.Sprintf("general-%d", i),
				CreatedAtMs: int64(i),
			})
		}
	}
	return scheduler
}

// mustAssignScheduler assigns one task or fails the test.
func mustAssignScheduler(t *testing.T, scheduler *Scheduler, snapshot workerSnapshot, now ...int64) SchedMeta {
	t.Helper()
	nowMs := int64(0)
	if len(now) > 0 {
		nowMs = now[0]
	}
	meta, ok := scheduler.Assign(snapshot, nowMs, schedulerAssignAll)
	if !ok {
		t.Fatal("scheduler did not assign a task")
	}
	return meta
}

// schedulerAssignAll accepts every scheduler candidate.
func schedulerAssignAll(SchedMeta) schedulerDecision {
	return schedulerAssign
}

// TestSchedulerPreferredLocalityWorkerUsesPlacement verifies the placement index
// chooses cached workers and falls back when cached capacity is full.
func TestSchedulerPreferredLocalityWorkerUsesPlacement(t *testing.T) {
	scheduler := newScheduler()
	scheduler.UpsertWorker(metadata.Worker{
		WorkerID:      "cold",
		CachedModels:  map[string]bool{},
		Capacity:      8,
		RunningTasks:  0,
		LastHeartbeat: 1,
	})
	scheduler.UpsertWorker(metadata.Worker{
		WorkerID:      "cached",
		CachedModels:  map[string]bool{metadata.ModelKey("model-A", "v1"): true},
		Capacity:      1,
		RunningTasks:  0,
		LastHeartbeat: 1,
	})

	preferred := scheduler.PreferredLocalityWorker(modelKeyFromParts("model-A", "v1"), 1000, 1001, localityQueueWait.Milliseconds())
	if preferred != "cached" {
		t.Fatalf("preferred worker = %s, want cached", preferred)
	}

	scheduler.UpsertWorker(metadata.Worker{
		WorkerID:      "cached",
		CachedModels:  map[string]bool{metadata.ModelKey("model-A", "v1"): true},
		Capacity:      1,
		RunningTasks:  1,
		LastHeartbeat: 1,
	})
	preferred = scheduler.PreferredLocalityWorker(modelKeyFromParts("model-A", "v1"), 1000, 1001, localityQueueWait.Milliseconds())
	if preferred != "cold" {
		t.Fatalf("preferred worker = %s, want cold when cached worker is full", preferred)
	}
}

// TestSchedulerAssignSkipsRejectedLLMAndFallsBackToGeneral verifies a skipped LLM
// candidate does not hide assignable general work.
func TestSchedulerAssignSkipsRejectedLLMAndFallsBackToGeneral(t *testing.T) {
	scheduler := newScheduler()
	scheduler.Enqueue(SchedMeta{TaskID: "llm", ModelName: "model-A", ModelVersion: "v1", CreatedAtMs: 1})
	scheduler.Enqueue(SchedMeta{TaskID: "general", CreatedAtMs: 2})

	meta := mustAssignScheduler(t, scheduler, workerSnapshot{WorkerID: "worker-1"}, 1000)
	if meta.TaskID != "llm" {
		t.Fatalf("first assignment = %s, want llm", meta.TaskID)
	}
	scheduler.ReturnFront(meta)

	meta, ok := scheduler.Assign(workerSnapshot{WorkerID: "worker-1"}, 1000, func(meta SchedMeta) schedulerDecision {
		if meta.TaskID == "llm" {
			return schedulerSkip
		}
		return schedulerAssign
	})
	if !ok || meta.TaskID != "general" {
		t.Fatalf("assignment = %+v ok=%v, want general fallback", meta, ok)
	}
}

var _ = logservepb.SchedulingPolicy_SCHEDULING_POLICY_LOCALITY_AWARE

// TestPollTaskIndexedDoesNotLetActorAndLLMBacklogBlockGeneral verifies unrelated
// general work can dispatch despite blocked actor and LLM queues.
func TestPollTaskIndexedDoesNotLetActorAndLLMBacklogBlockGeneral(t *testing.T) {
	t.Setenv("LOGSERVE_SCHEDULER_V2", "1")
	meta := metadata.NewMemoryStore()
	meta.UpsertWorker(metadata.Worker{WorkerID: "general-worker", Capacity: 1, LastHeartbeat: actor.NowMs()})
	meta.UpsertWorker(metadata.Worker{
		WorkerID:      "llm-worker",
		CachedModels:  map[string]bool{metadata.ModelKey("model-A", "v1"): true},
		Capacity:      1,
		LastHeartbeat: actor.NowMs(),
	})
	actorState := actor.NewState("actor-mixed", "Counter", "class Counter: pass", []byte(`{"args":[],"kwargs":{}}`), 10, actor.NowMs())
	actorState.OwnerWorkerID = "actor-owner"
	actorState.Epoch = 1
	if _, duplicate := meta.CreateActor(actorState, ""); duplicate {
		t.Fatal("unexpected duplicate actor")
	}
	service := NewServiceWithResultStore(meta, acceptingLogClient{}, nil, 0)
	service.syncSchedulerWorkers()

	_, _, err := service.enqueueTaskWithMetadata(context.Background(), &logservepb.TaskSpec{
		TaskId:            "actor-future",
		TaskName:          "actor:inc",
		FunctionName:      "inc",
		ArgsJson:          []byte(`{"args":[],"kwargs":{}}`),
		IdempotencyKey:    "actor-future",
		ActorId:           actorState.ActorID,
		ActorCallId:       "actor-future",
		ActorClassName:    actorState.ClassName,
		ActorClassSource:  actorState.ClassSource,
		ActorMethod:       "inc",
		ActorInitArgsJson: actorState.InitArgsJSON,
		ActorEpoch:        actorState.Epoch,
	}, func(task *metadata.Task) {
		task.ActorCommandSeq = 2
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = service.enqueueTask(context.Background(), &logservepb.TaskSpec{
		TaskId:          "llm-local",
		TaskName:        "llm:model-A",
		FunctionName:    "__logserve_llm__",
		ArgsJson:        []byte(`{"args":[],"kwargs":{}}`),
		IdempotencyKey:  "llm-local",
		LlmModelName:    "model-A",
		LlmModelVersion: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	general, _, err := service.enqueueTask(context.Background(), &logservepb.TaskSpec{
		TaskId:         "general-ready",
		TaskName:       "general",
		FunctionName:   "general",
		FunctionSource: "def general(): return 1",
		ArgsJson:       []byte(`{"args":[],"kwargs":{}}`),
		IdempotencyKey: "general-ready",
	})
	if err != nil {
		t.Fatal(err)
	}

	poll, err := service.PollTask(context.Background(), &logservepb.PollTaskRequest{WorkerId: "general-worker"})
	if err != nil {
		t.Fatal(err)
	}
	if !poll.GetHasTask() || poll.GetTask().GetTaskId() != general.TaskID {
		t.Fatalf("poll assigned %v, want general task %s", poll.GetTask(), general.TaskID)
	}
}

// TestRedeliveryIndexedRequeuesOnlyTrackedExpiredLease verifies indexed redelivery
// only touches leases tracked by the scheduler deadline heap.
func TestRedeliveryIndexedRequeuesOnlyTrackedExpiredLease(t *testing.T) {
	t.Setenv("LOGSERVE_SCHEDULER_V2", "1")
	meta := metadata.NewMemoryStore()
	meta.UpsertWorker(metadata.Worker{WorkerID: "worker-1", Capacity: 1, LastHeartbeat: actor.NowMs()})
	service := NewServiceWithResultStore(meta, acceptingLogClient{}, nil, 0)
	service.configMu.Lock()
	service.redeliveryTimeout = time.Nanosecond
	service.configMu.Unlock()

	tracked, _, err := service.enqueueTask(context.Background(), &logservepb.TaskSpec{
		TaskId:         "tracked",
		TaskName:       "tracked",
		FunctionName:   "tracked",
		FunctionSource: "def tracked(): return 1",
		ArgsJson:       []byte(`{"args":[],"kwargs":{}}`),
		IdempotencyKey: "tracked",
	})
	if err != nil {
		t.Fatal(err)
	}
	poll, err := service.PollTask(context.Background(), &logservepb.PollTaskRequest{WorkerId: "worker-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !poll.GetHasTask() || poll.GetTask().GetTaskId() != tracked.TaskID {
		t.Fatalf("poll assigned %v, want tracked", poll.GetTask())
	}
	_, duplicate := meta.CreateTask(metadata.Task{
		TaskID:         "untracked",
		TaskName:       "untracked",
		Status:         logservepb.TaskStatus_TASK_STATUS_RUNNING,
		WorkerID:       "worker-1",
		TaskLeaseEpoch: 1,
	}, "")
	if duplicate {
		t.Fatal("unexpected duplicate untracked task")
	}
	time.Sleep(time.Millisecond)
	if err := service.redeliverExpiredTasks(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, ok := meta.GetTask(tracked.TaskID)
	if !ok || current.Status != logservepb.TaskStatus_TASK_STATUS_QUEUED {
		t.Fatalf("tracked status = %v ok=%v, want QUEUED", current.Status, ok)
	}
	untracked, ok := meta.GetTask("untracked")
	if !ok || untracked.Status != logservepb.TaskStatus_TASK_STATUS_RUNNING {
		t.Fatalf("untracked status = %v ok=%v, want RUNNING", untracked.Status, ok)
	}
}

// TestPollTaskIndexedAssignsLLMToCachedWorker verifies scheduler v2 dispatches LLM
// work to a worker that reports the model as cached.
func TestPollTaskIndexedAssignsLLMToCachedWorker(t *testing.T) {
	t.Setenv("LOGSERVE_SCHEDULER_V2", "1")
	meta := metadata.NewMemoryStore()
	meta.UpsertWorker(metadata.Worker{
		WorkerID:      "llm-worker",
		CachedModels:  map[string]bool{metadata.ModelKey("model-A", "v1"): true},
		Capacity:      1,
		LastHeartbeat: actor.NowMs(),
	})
	service := NewServiceWithResultStore(meta, acceptingLogClient{}, nil, 0)
	service.syncSchedulerWorkers()
	task, _, err := service.enqueueTask(context.Background(), &logservepb.TaskSpec{
		TaskId:          "llm-cached",
		TaskName:        "llm:model-A",
		FunctionName:    "__logserve_llm__",
		ArgsJson:        []byte(`{"args":[],"kwargs":{}}`),
		IdempotencyKey:  "llm-cached",
		LlmModelName:    "model-A",
		LlmModelVersion: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	poll, err := service.PollTask(context.Background(), &logservepb.PollTaskRequest{WorkerId: "llm-worker"})
	if err != nil {
		t.Fatal(err)
	}
	if !poll.GetHasTask() || poll.GetTask().GetTaskId() != task.TaskID {
		t.Fatalf("poll assigned %v, want %s", poll.GetTask(), task.TaskID)
	}
}
