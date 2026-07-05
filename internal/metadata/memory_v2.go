package metadata

// This file contains the sharded in-memory Store implementation used by
// default. It preserves the Store contract while reducing contention on hot task
// and worker paths.

import (
	"container/heap"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/actor"
	"github.com/logserve/logserve/internal/workflow"
)

// memoryTaskShardCount spreads task records across fixed shards to reduce lock
// contention without making the shard layout depend on runtime configuration.
const memoryTaskShardCount = 64

// MemoryStoreV2 composes specialized in-memory stores for each metadata domain.
// Task and worker paths use finer-grained locking because they are scheduler hot
// paths.
type MemoryStoreV2 struct {
	// tasks and workers are split out because scheduling, lease recovery, and
	// heartbeats are the high-concurrency paths.
	tasks   taskStoreV2
	workers workerStoreV2
	// workflows, actors, and models use smaller domain stores with simpler locks.
	workflows workflowStoreV2
	actors    actorStoreV2
	models    modelStoreV2
}

// NewMemoryStore constructs the default sharded in-memory metadata store.
func NewMemoryStore() *MemoryStoreV2 {
	store := &MemoryStoreV2{}
	store.tasks.init()
	store.workers.init()
	store.workflows.init()
	store.actors.init()
	store.models.init()
	return store
}

// CreateTask delegates task creation to the V2 task store and reports whether
// idempotency reused an existing task.
func (s *MemoryStoreV2) CreateTask(task Task, idempotencyKey string) (Task, bool) {
	return s.tasks.create(task, idempotencyKey)
}

// GetTask returns a defensive task snapshot by ID.
func (s *MemoryStoreV2) GetTask(taskID string) (Task, bool) {
	return s.tasks.get(taskID)
}

// GetTaskByIdempotencyKey resolves a task idempotency key through the task index.
func (s *MemoryStoreV2) GetTaskByIdempotencyKey(idempotencyKey string) (Task, bool) {
	return s.tasks.getByIdempotencyKey(idempotencyKey)
}

// ListTasks snapshots all tasks through the secondary task index.
func (s *MemoryStoreV2) ListTasks() []Task {
	return s.tasks.list()
}

// ListTasksByStatus uses status indexes for queued/running tasks and falls back
// to a full indexed scan for other statuses.
func (s *MemoryStoreV2) ListTasksByStatus(status logservepb.TaskStatus) []Task {
	return s.tasks.listByStatus(status)
}

// LeaseTask delegates lease assignment and epoch fencing to the task store.
func (s *MemoryStoreV2) LeaseTask(taskID, workerID string) (Task, error) {
	return s.tasks.lease(taskID, workerID)
}

// ValidateTaskLease verifies the current running lease without mutating state.
func (s *MemoryStoreV2) ValidateTaskLease(taskID, workerID string, leaseEpoch uint64) (Task, error) {
	return s.tasks.validateLease(taskID, workerID, leaseEpoch)
}

// RequeueExpiredRunningTasks requeues running tasks whose lease timestamps are
// older than maxAge.
func (s *MemoryStoreV2) RequeueExpiredRunningTasks(maxAge time.Duration) []Task {
	return s.tasks.requeueExpiredRunning(maxAge)
}

// RequeueTaskIfLeaseExpired performs targeted recovery for a specific lease epoch.
func (s *MemoryStoreV2) RequeueTaskIfLeaseExpired(taskID string, leaseEpoch uint64, maxAge time.Duration) (Task, bool) {
	return s.tasks.requeueTaskIfLeaseExpired(taskID, leaseEpoch, maxAge)
}

// CompleteTask commits a task result only if the caller still owns the current
// lease epoch.
func (s *MemoryStoreV2) CompleteTask(taskID, workerID string, leaseEpoch uint64, status logservepb.TaskStatus, resultJSON []byte, taskErr string) (Task, error) {
	return s.tasks.complete(taskID, workerID, leaseEpoch, status, resultJSON, taskErr)
}

// RegisterModel stores model registry metadata and returns a cloned record.
func (s *MemoryStoreV2) RegisterModel(model *logservepb.ModelInfo) *logservepb.ModelInfo {
	return s.models.register(model)
}

// GetModel returns a cloned model registration for name and version.
func (s *MemoryStoreV2) GetModel(name, version string) (*logservepb.ModelInfo, bool) {
	return s.models.get(name, version)
}

// ListModels snapshots all registered models.
func (s *MemoryStoreV2) ListModels() []*logservepb.ModelInfo {
	return s.models.list()
}

// CreateWorkflow records workflow state or returns an idempotent duplicate.
func (s *MemoryStoreV2) CreateWorkflow(state workflow.State, idempotencyKey string) (workflow.State, bool) {
	return s.workflows.create(state, idempotencyKey)
}

// GetWorkflow returns a cloned workflow state by ID.
func (s *MemoryStoreV2) GetWorkflow(workflowID string) (workflow.State, bool) {
	return s.workflows.get(workflowID)
}

// GetWorkflowByIdempotencyKey resolves a workflow through its idempotency index.
func (s *MemoryStoreV2) GetWorkflowByIdempotencyKey(idempotencyKey string) (workflow.State, bool) {
	return s.workflows.getByIdempotencyKey(idempotencyKey)
}

// ListWorkflows snapshots all workflow states.
func (s *MemoryStoreV2) ListWorkflows() []workflow.State {
	return s.workflows.list()
}

// UpdateWorkflow applies a caller mutation to a private workflow snapshot and
// commits it if fn succeeds.
func (s *MemoryStoreV2) UpdateWorkflow(workflowID string, fn func(*workflow.State) error) (workflow.State, error) {
	return s.workflows.update(workflowID, fn)
}

// UpsertWorkflow installs a workflow snapshot, including its idempotency index.
func (s *MemoryStoreV2) UpsertWorkflow(state workflow.State) {
	s.workflows.upsert(state)
}

// UpsertWorker registers worker metadata while preserving V2 hot counters as
// appropriate.
func (s *MemoryStoreV2) UpsertWorker(worker Worker) {
	s.workers.upsert(worker)
}

// GetWorker returns a cloned worker snapshot.
func (s *MemoryStoreV2) GetWorker(workerID string) (Worker, bool) {
	return s.workers.get(workerID)
}

// ActiveWorkers returns scheduler-visible workers whose heartbeat is fresh enough.
func (s *MemoryStoreV2) ActiveWorkers(maxAge time.Duration) []Worker {
	return s.workers.active(maxAge)
}

// ListWorkers snapshots every worker record.
func (s *MemoryStoreV2) ListWorkers() []Worker {
	return s.workers.list()
}

// Heartbeat updates worker liveness and optional cached model state.
func (s *MemoryStoreV2) Heartbeat(workerID string, cachedModels map[string]bool) (Worker, bool) {
	return s.workers.heartbeat(workerID, cachedModels)
}

// IncrementWorkerLoad increments the worker hot-path running counter.
func (s *MemoryStoreV2) IncrementWorkerLoad(workerID string) {
	s.workers.incrementLoad(workerID)
}

// DecrementWorkerLoad decrements the worker hot-path running counter without
// underflowing.
func (s *MemoryStoreV2) DecrementWorkerLoad(workerID string) {
	s.workers.decrementLoad(workerID)
}

// CreateActor stores actor state or returns an idempotent duplicate.
func (s *MemoryStoreV2) CreateActor(state actor.State, idempotencyKey string) (actor.State, bool) {
	return s.actors.create(state, idempotencyKey)
}

// GetActor returns a cloned actor snapshot by ID.
func (s *MemoryStoreV2) GetActor(actorID string) (actor.State, bool) {
	return s.actors.get(actorID)
}

// GetActorByIdempotencyKey resolves an actor through its idempotency index.
func (s *MemoryStoreV2) GetActorByIdempotencyKey(idempotencyKey string) (actor.State, bool) {
	return s.actors.getByIdempotencyKey(idempotencyKey)
}

// ListActors snapshots every actor state.
func (s *MemoryStoreV2) ListActors() []actor.State {
	return s.actors.list()
}

// UpdateActor applies a caller mutation to a cloned actor state and commits it
// if fn succeeds.
func (s *MemoryStoreV2) UpdateActor(actorID string, fn func(*actor.State) error) (actor.State, error) {
	return s.actors.update(actorID, fn)
}

// UpsertActor installs an actor snapshot and its idempotency index.
func (s *MemoryStoreV2) UpsertActor(state actor.State) {
	s.actors.upsert(state)
}

// taskStoreV2 stores tasks in shards and maintains secondary indexes for list
// and lease-recovery paths.
type taskStoreV2 struct {
	// shards own task records; each task mutation locks only the shard selected by task ID.
	shards [memoryTaskShardCount]taskShardV2

	// idemMu protects the idempotency index independently from task shards so a
	// create can reserve an idempotency key without serializing unrelated reads.
	idemMu sync.RWMutex
	byIdem map[string]string

	// indexMu protects secondary indexes that are updated from immutable before/after snapshots.
	indexMu sync.RWMutex
	// allTasks is the authoritative list index for deterministic full snapshots.
	allTasks map[string]struct{}
	// byStatus indexes scheduler-hot statuses; terminal statuses fall back to allTasks.
	byStatus map[logservepb.TaskStatus]map[string]struct{}
	// byWorker tracks running assignments for diagnostics and future placement logic.
	byWorker map[string]map[string]struct{}

	// deadlineMu protects the running lease heap separately from list indexes.
	deadlineMu sync.Mutex
	running    taskDeadlineQueueV2
}

// taskShardV2 owns the actual task pointers for one hash shard.
type taskShardV2 struct {
	// mu protects the task pointers in this shard; callers clone before returning values.
	mu sync.RWMutex
	// tasks stores pointers so in-shard updates can mutate one record without copying the map.
	tasks map[string]*Task
}

// init prepares all task shards and secondary indexes.
func (s *taskStoreV2) init() {
	for i := range s.shards {
		s.shards[i].tasks = make(map[string]*Task)
	}
	s.byIdem = make(map[string]string)
	s.allTasks = make(map[string]struct{})
	s.byStatus = make(map[logservepb.TaskStatus]map[string]struct{})
	s.byWorker = make(map[string]map[string]struct{})
	s.running.init()
}

// create inserts a task and serializes non-empty idempotency keys through a
// separate lock so unrelated task IDs can still use shard-level concurrency.
func (s *taskStoreV2) create(task Task, idempotencyKey string) (Task, bool) {
	if idempotencyKey != "" {
		s.idemMu.Lock()
		defer s.idemMu.Unlock()
		if taskID, ok := s.byIdem[idempotencyKey]; ok {
			if existing, ok := s.get(taskID); ok {
				return existing, true
			}

			// Drop stale idempotency entries defensively if the indexed task disappeared.
			// This keeps retries from being permanently pinned to a missing record.
			delete(s.byIdem, idempotencyKey)
		}
		created := s.createWithoutIdemLock(task, idempotencyKey)
		s.byIdem[idempotencyKey] = created.TaskID
		return created, false
	}
	return s.createWithoutIdemLock(task, ""), false
}

// createWithoutIdemLock writes the task shard and then refreshes secondary
// indexes from cloned before/after snapshots.
func (s *taskStoreV2) createWithoutIdemLock(task Task, idempotencyKey string) Task {
	now := time.Now().UnixMilli()
	task.CreatedAtMs = now
	task.UpdatedAtMs = now
	task.IdempotencyKey = idempotencyKey
	stored := cloneTask(task)
	shard := s.shard(stored.TaskID)

	var previous *Task
	shard.mu.Lock()
	if existing, ok := shard.tasks[stored.TaskID]; ok {
		copy := cloneTask(*existing)
		previous = &copy
	}
	shard.tasks[stored.TaskID] = &stored
	shard.mu.Unlock()

	// Indexes are updated after releasing the shard lock to keep shard critical
	// sections short; replacement uses immutable snapshots captured under the shard lock.
	s.replaceTaskIndexes(previous, &stored)
	return cloneTask(stored)
}

// get returns a cloned task from its shard.
func (s *taskStoreV2) get(taskID string) (Task, bool) {
	shard := s.shard(taskID)
	shard.mu.RLock()
	task, ok := shard.tasks[taskID]
	if !ok {
		shard.mu.RUnlock()
		return Task{}, false
	}
	out := cloneTask(*task)
	shard.mu.RUnlock()
	return out, true
}

// getByIdempotencyKey resolves a non-empty key and then reuses the normal shard
// lookup path.
func (s *taskStoreV2) getByIdempotencyKey(idempotencyKey string) (Task, bool) {
	if idempotencyKey == "" {
		return Task{}, false
	}
	s.idemMu.RLock()
	taskID, ok := s.byIdem[idempotencyKey]
	s.idemMu.RUnlock()
	if !ok {
		return Task{}, false
	}
	return s.get(taskID)
}

// list snapshots task IDs from the index and then fetches each task from its
// shard.
func (s *taskStoreV2) list() []Task {
	ids := s.allIndexedTaskIDs()
	out := make([]Task, 0, len(ids))
	for _, taskID := range ids {
		if task, ok := s.get(taskID); ok {
			out = append(out, task)
		}
	}
	return out
}

// listByStatus reads candidate IDs from the status index and rechecks each shard
// record because index snapshots can race with concurrent status transitions.
func (s *taskStoreV2) listByStatus(status logservepb.TaskStatus) []Task {
	ids := s.indexedTaskIDsByStatus(status)
	out := make([]Task, 0, len(ids))
	for _, taskID := range ids {
		if task, ok := s.get(taskID); ok && task.Status == status {
			out = append(out, task)
		}
	}
	return out
}

// lease moves a task to RUNNING, assigns the worker, and increments the lease
// epoch for stale-completion fencing.
func (s *taskStoreV2) lease(taskID, workerID string) (Task, error) {
	shard := s.shard(taskID)
	shard.mu.Lock()
	task, ok := shard.tasks[taskID]
	if !ok {
		shard.mu.Unlock()
		return Task{}, errors.New("task not found")
	}
	if isTerminalTaskStatus(task.Status) {
		out := cloneTask(*task)
		shard.mu.Unlock()
		return out, nil
	}
	previous := cloneTask(*task)
	task.TaskLeaseEpoch++
	task.Status = logservepb.TaskStatus_TASK_STATUS_RUNNING
	task.WorkerID = workerID
	task.UpdatedAtMs = time.Now().UnixMilli()
	current := cloneTask(*task)
	shard.mu.Unlock()

	s.replaceTaskIndexes(&previous, &current)
	return current, nil
}

// validateLease checks that the worker and lease epoch still match the current
// running task.
func (s *taskStoreV2) validateLease(taskID, workerID string, leaseEpoch uint64) (Task, error) {
	shard := s.shard(taskID)
	shard.mu.RLock()
	task, ok := shard.tasks[taskID]
	if !ok {
		shard.mu.RUnlock()
		return Task{}, errors.New("task not found")
	}
	if isTerminalTaskStatus(task.Status) {
		out := cloneTask(*task)
		shard.mu.RUnlock()
		return out, nil
	}
	if task.Status != logservepb.TaskStatus_TASK_STATUS_RUNNING {
		shard.mu.RUnlock()
		return Task{}, errors.New("stale task lease rejected")
	}
	if task.WorkerID == "" || workerID == "" || task.WorkerID != workerID {
		shard.mu.RUnlock()
		return Task{}, errors.New("stale task lease rejected")
	}
	if task.TaskLeaseEpoch == 0 || leaseEpoch == 0 || task.TaskLeaseEpoch != leaseEpoch {
		shard.mu.RUnlock()
		return Task{}, errors.New("stale task lease rejected")
	}
	out := cloneTask(*task)
	shard.mu.RUnlock()
	return out, nil
}

// requeueExpiredRunning uses the deadline heap to avoid scanning all tasks for
// lease expiry.
func (s *taskStoreV2) requeueExpiredRunning(maxAge time.Duration) []Task {
	if maxAge <= 0 {
		return nil
	}
	cutoff := time.Now().UnixMilli() - maxAge.Milliseconds()
	requeued := make([]Task, 0)
	for {
		deadline, ok := s.popRunningBefore(cutoff)
		if !ok {
			break
		}

		// The heap only supplies candidates; requeueTaskAt revalidates status, epoch,
		// and age under the task shard lock before changing state.
		task, ok := s.requeueTaskAt(deadline.taskID, deadline.leaseEpoch, maxAge, time.Now().UnixMilli())
		if ok {
			requeued = append(requeued, task)
		}
	}
	return requeued
}

// requeueTaskIfLeaseExpired reuses the same epoch-checked path as heap-driven
// recovery for a single task.
func (s *taskStoreV2) requeueTaskIfLeaseExpired(taskID string, leaseEpoch uint64, maxAge time.Duration) (Task, bool) {
	return s.requeueTaskAt(taskID, leaseEpoch, maxAge, time.Now().UnixMilli())
}

// requeueTaskAt atomically checks lease freshness and moves a still-expired
// running task back to QUEUED.
func (s *taskStoreV2) requeueTaskAt(taskID string, leaseEpoch uint64, maxAge time.Duration, nowMs int64) (Task, bool) {
	if maxAge <= 0 || taskID == "" || leaseEpoch == 0 {
		return Task{}, false
	}
	shard := s.shard(taskID)
	shard.mu.Lock()
	task, ok := shard.tasks[taskID]
	if !ok {
		shard.mu.Unlock()
		return Task{}, false
	}
	if task.Status != logservepb.TaskStatus_TASK_STATUS_RUNNING || task.TaskLeaseEpoch != leaseEpoch {
		// Heap and targeted recovery both use this path; the epoch check discards
		// stale heap entries created by a previous lease for the same task ID.
		out := cloneTask(*task)
		shard.mu.Unlock()
		return out, false
	}
	if nowMs-task.UpdatedAtMs < maxAge.Milliseconds() {
		out := cloneTask(*task)
		shard.mu.Unlock()
		return out, false
	}
	previous := cloneTask(*task)
	requeuedTask := cloneTask(*task)
	task.Status = logservepb.TaskStatus_TASK_STATUS_QUEUED
	task.WorkerID = ""
	task.UpdatedAtMs = nowMs
	stored := cloneTask(*task)
	requeuedTask.Status = logservepb.TaskStatus_TASK_STATUS_QUEUED
	requeuedTask.UpdatedAtMs = nowMs
	shard.mu.Unlock()

	s.replaceTaskIndexes(&previous, &stored)
	return requeuedTask, true
}

// complete stores the final task result after verifying the caller owns the
// current running lease.
func (s *taskStoreV2) complete(taskID, workerID string, leaseEpoch uint64, status logservepb.TaskStatus, resultJSON []byte, taskErr string) (Task, error) {
	shard := s.shard(taskID)
	shard.mu.Lock()
	task, ok := shard.tasks[taskID]
	if !ok {
		shard.mu.Unlock()
		return Task{}, errors.New("task not found")
	}
	if isTerminalTaskStatus(task.Status) {
		out := cloneTask(*task)
		shard.mu.Unlock()
		return out, nil
	}
	if task.Status != logservepb.TaskStatus_TASK_STATUS_RUNNING {
		shard.mu.Unlock()
		return Task{}, errors.New("stale task lease rejected")
	}
	if task.WorkerID == "" || workerID == "" || task.WorkerID != workerID {
		shard.mu.Unlock()
		return Task{}, errors.New("stale task lease rejected")
	}
	if task.TaskLeaseEpoch == 0 || leaseEpoch == 0 || task.TaskLeaseEpoch != leaseEpoch {
		shard.mu.Unlock()
		return Task{}, errors.New("stale task lease rejected")
	}
	previous := cloneTask(*task)
	task.Status = status
	task.WorkerID = workerID
	// Copy result bytes while holding the shard lock so the stored terminal result
	// cannot alias caller-owned buffers.
	task.ResultJSON = append([]byte(nil), resultJSON...)
	task.Error = taskErr
	task.UpdatedAtMs = time.Now().UnixMilli()
	current := cloneTask(*task)
	shard.mu.Unlock()

	s.replaceTaskIndexes(&previous, &current)
	return current, nil
}

// shard maps a task ID to its fixed hash shard.
func (s *taskStoreV2) shard(taskID string) *taskShardV2 {
	return &s.shards[taskShardIndex(taskID)]
}

// allIndexedTaskIDs returns a deterministic snapshot of indexed task IDs.
func (s *taskStoreV2) allIndexedTaskIDs() []string {
	s.indexMu.RLock()
	out := make([]string, 0, len(s.allTasks))
	for taskID := range s.allTasks {
		out = append(out, taskID)
	}
	s.indexMu.RUnlock()
	sort.Strings(out)
	return out
}

// indexedTaskIDsByStatus returns queued/running IDs from dedicated indexes and
// other statuses from the full task index.
func (s *taskStoreV2) indexedTaskIDsByStatus(status logservepb.TaskStatus) []string {
	s.indexMu.RLock()
	var ids map[string]struct{}
	if indexedTaskStatus(status) {
		ids = s.byStatus[status]
	} else {
		ids = s.allTasks
	}
	out := make([]string, 0, len(ids))
	for taskID := range ids {
		out = append(out, taskID)
	}
	s.indexMu.RUnlock()
	sort.Strings(out)
	return out
}

// replaceTaskIndexes reconciles secondary indexes and the running-deadline heap
// after a task mutation.
func (s *taskStoreV2) replaceTaskIndexes(previous, current *Task) {
	// previous/current are clones captured under the shard lock; using snapshots
	// here avoids holding shard locks while touching global secondary indexes.
	s.indexMu.Lock()
	if previous != nil {
		if indexedTaskStatus(previous.Status) {
			s.removeStatusIndexLocked(previous.Status, previous.TaskID)
		}
		if previous.Status == logservepb.TaskStatus_TASK_STATUS_RUNNING && previous.WorkerID != "" {
			s.removeWorkerIndexLocked(previous.WorkerID, previous.TaskID)
		}
	}
	if current != nil {
		s.allTasks[current.TaskID] = struct{}{}
		if indexedTaskStatus(current.Status) {
			s.addStatusIndexLocked(current.Status, current.TaskID)
		}
		if current.Status == logservepb.TaskStatus_TASK_STATUS_RUNNING && current.WorkerID != "" {
			s.addWorkerIndexLocked(current.WorkerID, current.TaskID)
		}
	}
	s.indexMu.Unlock()

	// Deadline heap maintenance is separated from indexMu so list operations do not
	// block on heap operations for running leases.
	if previous != nil && previous.Status == logservepb.TaskStatus_TASK_STATUS_RUNNING {
		if current == nil || current.Status != logservepb.TaskStatus_TASK_STATUS_RUNNING || current.TaskLeaseEpoch != previous.TaskLeaseEpoch {
			s.untrackRunning(previous.TaskID)
		}
	}
	if current != nil && current.Status == logservepb.TaskStatus_TASK_STATUS_RUNNING {
		s.trackRunning(*current)
	}
}

// addStatusIndexLocked inserts a task ID into a status set while indexMu is held.
func (s *taskStoreV2) addStatusIndexLocked(status logservepb.TaskStatus, taskID string) {
	ids := s.byStatus[status]
	if ids == nil {
		ids = make(map[string]struct{})
		s.byStatus[status] = ids
	}
	ids[taskID] = struct{}{}
}

// removeStatusIndexLocked deletes a task ID from a status set and removes empty
// sets to keep index scans compact.
func (s *taskStoreV2) removeStatusIndexLocked(status logservepb.TaskStatus, taskID string) {
	ids := s.byStatus[status]
	if ids == nil {
		return
	}
	delete(ids, taskID)
	if len(ids) == 0 {
		delete(s.byStatus, status)
	}
}

// addWorkerIndexLocked tracks which running tasks are currently assigned to a
// worker while indexMu is held.
func (s *taskStoreV2) addWorkerIndexLocked(workerID, taskID string) {
	ids := s.byWorker[workerID]
	if ids == nil {
		ids = make(map[string]struct{})
		s.byWorker[workerID] = ids
	}
	ids[taskID] = struct{}{}
}

// removeWorkerIndexLocked removes a running-task worker index entry and drops
// empty worker sets.
func (s *taskStoreV2) removeWorkerIndexLocked(workerID, taskID string) {
	ids := s.byWorker[workerID]
	if ids == nil {
		return
	}
	delete(ids, taskID)
	if len(ids) == 0 {
		delete(s.byWorker, workerID)
	}
}

// trackRunning records the task lease timestamp in a min-heap for expiry scans.
func (s *taskStoreV2) trackRunning(task Task) {
	if task.Status != logservepb.TaskStatus_TASK_STATUS_RUNNING || task.TaskLeaseEpoch == 0 {
		return
	}
	s.deadlineMu.Lock()
	s.running.upsert(taskDeadlineV2{taskID: task.TaskID, updatedAtMs: task.UpdatedAtMs, leaseEpoch: task.TaskLeaseEpoch})
	s.deadlineMu.Unlock()
}

// untrackRunning removes a task from the lease-expiry heap when it leaves the
// current running lease.
func (s *taskStoreV2) untrackRunning(taskID string) {
	s.deadlineMu.Lock()
	s.running.remove(taskID)
	s.deadlineMu.Unlock()
}

// popRunningBefore returns the oldest running lease candidate only when it is at
// or before the supplied cutoff.
func (s *taskStoreV2) popRunningBefore(cutoffMs int64) (taskDeadlineV2, bool) {
	s.deadlineMu.Lock()
	defer s.deadlineMu.Unlock()
	if s.running.Len() == 0 || s.running.entries[0].updatedAtMs > cutoffMs {
		return taskDeadlineV2{}, false
	}
	return heap.Pop(&s.running).(taskDeadlineV2), true
}

// taskDeadlineV2 is a heap entry keyed by the task lease timestamp and epoch.
type taskDeadlineV2 struct {
	taskID      string
	updatedAtMs int64
	leaseEpoch  uint64
}

// taskDeadlineQueueV2 is an indexed min-heap so lease updates and removals are
// O(log n) instead of leaving unbounded stale heap entries.
type taskDeadlineQueueV2 struct {
	// entries is the heap-ordered slice used by container/heap.
	entries []taskDeadlineV2
	// positions lets upsert/remove locate a task's current heap slot in O(1).
	positions map[string]int
}

// init prepares the heap position index.
func (h *taskDeadlineQueueV2) init() {
	h.positions = make(map[string]int)
}

// upsert inserts or updates the deadline for one task lease.
func (h *taskDeadlineQueueV2) upsert(deadline taskDeadlineV2) {
	if h.positions == nil {
		h.init()
	}
	if idx, ok := h.positions[deadline.taskID]; ok {
		h.entries[idx] = deadline
		heap.Fix(h, idx)
		return
	}
	heap.Push(h, deadline)
}

// remove deletes a task deadline if the task is currently tracked.
func (h *taskDeadlineQueueV2) remove(taskID string) {
	if idx, ok := h.positions[taskID]; ok {
		heap.Remove(h, idx)
	}
}

// Len implements heap.Interface for taskDeadlineQueueV2.
func (h *taskDeadlineQueueV2) Len() int { return len(h.entries) }

// Less orders by oldest lease timestamp and then by stable tie-breakers for
// deterministic tests and repeatable scans.
func (h *taskDeadlineQueueV2) Less(i, j int) bool {
	if h.entries[i].updatedAtMs == h.entries[j].updatedAtMs {
		if h.entries[i].taskID == h.entries[j].taskID {
			return h.entries[i].leaseEpoch < h.entries[j].leaseEpoch
		}
		return h.entries[i].taskID < h.entries[j].taskID
	}
	return h.entries[i].updatedAtMs < h.entries[j].updatedAtMs
}

// Swap implements heap.Interface and keeps the position map synchronized.
func (h *taskDeadlineQueueV2) Swap(i, j int) {
	h.entries[i], h.entries[j] = h.entries[j], h.entries[i]
	h.positions[h.entries[i].taskID] = i
	h.positions[h.entries[j].taskID] = j
}

// Push implements heap.Interface and records the inserted entry position.
func (h *taskDeadlineQueueV2) Push(x any) {
	entry := x.(taskDeadlineV2)
	h.positions[entry.taskID] = len(h.entries)
	h.entries = append(h.entries, entry)
}

// Pop implements heap.Interface and removes the popped entry from the position
// map.
func (h *taskDeadlineQueueV2) Pop() any {
	old := h.entries
	last := old[len(old)-1]
	delete(h.positions, last.taskID)
	h.entries = old[:len(old)-1]
	return last
}

// taskShardIndex hashes task IDs with FNV-1a into the fixed task shard range.
func taskShardIndex(taskID string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(taskID); i++ {
		h ^= uint32(taskID[i])
		h *= 16777619
	}
	return h % memoryTaskShardCount
}

// isTerminalTaskStatus identifies statuses that should not be leased or
// overwritten by stale completions.
func isTerminalTaskStatus(status logservepb.TaskStatus) bool {
	return status == logservepb.TaskStatus_TASK_STATUS_SUCCEEDED || status == logservepb.TaskStatus_TASK_STATUS_FAILED
}

// indexedTaskStatus marks the scheduler hot statuses that get dedicated
// secondary indexes.
func indexedTaskStatus(status logservepb.TaskStatus) bool {
	return status == logservepb.TaskStatus_TASK_STATUS_QUEUED || status == logservepb.TaskStatus_TASK_STATUS_RUNNING
}

// workerStoreV2 stores worker records in a map while keeping frequently updated
// counters inside per-worker records.
type workerStoreV2 struct {
	// mu protects the worker map; each workerRecordV2 owns its own hot fields.
	mu sync.RWMutex
	// workers is stable after getOrCreate returns a record pointer for a worker ID.
	workers map[string]*workerRecordV2
}

// workerRecordV2 separates rarely changed identity fields behind a mutex from
// hot capacity, load, heartbeat, and cache fields updated atomically.
type workerRecordV2 struct {
	// mu protects identity fields and labels, which change far less often than load counters.
	mu       sync.RWMutex
	workerID string
	address  string
	labels   map[string]string

	// capacity, runningTasks, and lastHeartbeat are atomics for heartbeat and placement hot paths.
	capacity      atomic.Uint32
	runningTasks  atomic.Uint32
	lastHeartbeat atomic.Int64
	// cachedModels stores a cloned map; snapshots clone it again before returning to callers.
	cachedModels atomic.Value
}

// init prepares the worker map.
func (s *workerStoreV2) init() {
	s.workers = make(map[string]*workerRecordV2)
}

// upsert registers worker metadata and preserves existing running task counts
// once a worker record already exists.
func (s *workerStoreV2) upsert(worker Worker) {
	if worker.Labels == nil {
		worker.Labels = map[string]string{}
	}
	if worker.CachedModels == nil {
		worker.CachedModels = map[string]bool{}
	}
	if worker.Capacity == 0 {
		worker.Capacity = 1
	}
	if worker.LastHeartbeat == 0 {
		worker.LastHeartbeat = time.Now().UnixMilli()
	}
	record, existed := s.getOrCreate(worker.WorkerID)
	record.mu.Lock()
	record.workerID = worker.WorkerID
	record.address = worker.Address
	record.labels = cloneStringMap(worker.Labels)
	record.mu.Unlock()
	record.capacity.Store(worker.Capacity)

	// Only the first explicit upsert seeds RunningTasks; later metadata refreshes
	// must not erase scheduler-maintained load counters.
	if !existed {
		record.runningTasks.Store(worker.RunningTasks)
	}
	record.lastHeartbeat.Store(worker.LastHeartbeat)
	record.cachedModels.Store(cloneModelCache(worker.CachedModels))
}

// get returns a cloned worker snapshot by ID.
func (s *workerStoreV2) get(workerID string) (Worker, bool) {
	record, ok := s.record(workerID)
	if !ok {
		return Worker{}, false
	}
	return record.snapshot(), true
}

// active snapshots workers whose heartbeat is within maxAge; non-positive maxAge
// disables heartbeat filtering.
func (s *workerStoreV2) active(maxAge time.Duration) []Worker {
	now := time.Now().UnixMilli()
	maxAgeMs := maxAge.Milliseconds()
	s.mu.RLock()
	out := make([]Worker, 0, len(s.workers))
	for _, record := range s.workers {
		worker := record.snapshot()
		if maxAge <= 0 || now-worker.LastHeartbeat <= maxAgeMs {
			out = append(out, worker)
		}
	}
	s.mu.RUnlock()
	return out
}

// list snapshots all worker records.
func (s *workerStoreV2) list() []Worker {
	s.mu.RLock()
	out := make([]Worker, 0, len(s.workers))
	for _, record := range s.workers {
		out = append(out, record.snapshot())
	}
	s.mu.RUnlock()
	return out
}

// heartbeat creates missing workers, updates liveness, and optionally replaces
// the cached-model set.
func (s *workerStoreV2) heartbeat(workerID string, cachedModels map[string]bool) (Worker, bool) {
	record, existed := s.getOrCreate(workerID)
	if cachedModels != nil {
		// nil preserves the existing cache; a non-nil empty map means the worker is
		// explicitly reporting no cached models.
		record.cachedModels.Store(cloneModelCache(cachedModels))
	}
	if record.capacity.Load() == 0 {
		record.capacity.Store(1)
	}
	record.lastHeartbeat.Store(time.Now().UnixMilli())
	return record.snapshot(), existed
}

// incrementLoad creates a default worker if needed and atomically increments
// its running task count.
func (s *workerStoreV2) incrementLoad(workerID string) {
	record, _ := s.getOrCreate(workerID)
	if record.capacity.Load() == 0 {
		record.capacity.Store(1)
	}
	record.runningTasks.Add(1)
	record.lastHeartbeat.Store(time.Now().UnixMilli())
}

// decrementLoad uses a CAS loop so concurrent decrements cannot underflow the
// running task counter.
func (s *workerStoreV2) decrementLoad(workerID string) {
	record, ok := s.record(workerID)
	if !ok {
		return
	}
	for {
		current := record.runningTasks.Load()
		if current == 0 {
			return
		}
		if record.runningTasks.CompareAndSwap(current, current-1) {
			return
		}
	}
}

// getOrCreate uses double-checked locking to avoid taking the write lock on
// the common path where a worker already exists.
func (s *workerStoreV2) getOrCreate(workerID string) (*workerRecordV2, bool) {
	s.mu.RLock()
	record, ok := s.workers[workerID]
	s.mu.RUnlock()
	if ok {
		return record, true
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if record, ok = s.workers[workerID]; ok {
		return record, true
	}
	record = &workerRecordV2{
		workerID: workerID,
		labels:   map[string]string{},
	}
	record.capacity.Store(1)
	record.cachedModels.Store(map[string]bool{})
	s.workers[workerID] = record
	return record, false
}

// record returns the internal worker record pointer under the map read lock.
func (s *workerStoreV2) record(workerID string) (*workerRecordV2, bool) {
	s.mu.RLock()
	record, ok := s.workers[workerID]
	s.mu.RUnlock()
	return record, ok
}

// records snapshots internal record pointers for tests and diagnostics.
func (s *workerStoreV2) records() []*workerRecordV2 {
	s.mu.RLock()
	out := make([]*workerRecordV2, 0, len(s.workers))
	for _, record := range s.workers {
		out = append(out, record)
	}
	s.mu.RUnlock()
	return out
}

// snapshot combines mutex-protected identity fields and atomic hot fields into
// one cloned Worker value.
func (r *workerRecordV2) snapshot() Worker {
	r.mu.RLock()
	worker := Worker{
		WorkerID: r.workerID,
		Address:  r.address,
		Labels:   cloneStringMap(r.labels),
	}
	r.mu.RUnlock()
	worker.Capacity = r.capacity.Load()
	if worker.Capacity == 0 {
		worker.Capacity = 1
	}
	worker.RunningTasks = r.runningTasks.Load()
	worker.LastHeartbeat = r.lastHeartbeat.Load()
	if cached, ok := r.cachedModels.Load().(map[string]bool); ok {
		worker.CachedModels = cloneModelCache(cached)
	} else {
		worker.CachedModels = map[string]bool{}
	}
	return worker
}

// workflowStoreV2 stores workflow records and their idempotency index behind one
// lock because workflow updates are less frequent than task scheduling.
type workflowStoreV2 struct {
	mu        sync.RWMutex
	states    map[string]*workflowRecordV2
	byIdemKey map[string]string
}

// workflowRecordV2 keeps step slices and indexes alongside the cloned workflow
// state for fast internal access and invariant tests.
type workflowRecordV2 struct {
	// state is the cloned workflow header and map representation returned to callers.
	state workflow.State
	// steps preserves execution order from State.StepStatesInOrder for internal checks.
	steps []workflow.StepState
	// stepIndex maps step ID to the ordered steps slice without scanning.
	stepIndex map[string]int
}

// init prepares workflow state and idempotency maps.
func (s *workflowStoreV2) init() {
	s.states = make(map[string]*workflowRecordV2)
	s.byIdemKey = make(map[string]string)
}

// create stores initial workflow state or returns the existing idempotent state.
func (s *workflowStoreV2) create(state workflow.State, idempotencyKey string) (workflow.State, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if idempotencyKey != "" {
		if workflowID, ok := s.byIdemKey[idempotencyKey]; ok {
			return s.states[workflowID].snapshot(), true
		}
	}
	state.IdempotencyKey = idempotencyKey
	s.states[state.WorkflowID] = newWorkflowRecordV2(state)
	if idempotencyKey != "" {
		s.byIdemKey[idempotencyKey] = state.WorkflowID
	}
	return s.states[state.WorkflowID].snapshot(), false
}

// get returns a cloned workflow snapshot by ID.
func (s *workflowStoreV2) get(workflowID string) (workflow.State, bool) {
	s.mu.RLock()
	record, ok := s.states[workflowID]
	if !ok {
		s.mu.RUnlock()
		return workflow.State{}, false
	}
	out := record.snapshot()
	s.mu.RUnlock()
	return out, true
}

// getByIdempotencyKey resolves a workflow idempotency key and returns its
// cloned state.
func (s *workflowStoreV2) getByIdempotencyKey(idempotencyKey string) (workflow.State, bool) {
	if idempotencyKey == "" {
		return workflow.State{}, false
	}
	s.mu.RLock()
	workflowID, ok := s.byIdemKey[idempotencyKey]
	if !ok {
		s.mu.RUnlock()
		return workflow.State{}, false
	}
	record, ok := s.states[workflowID]
	if !ok {
		s.mu.RUnlock()
		return workflow.State{}, false
	}
	out := record.snapshot()
	s.mu.RUnlock()
	return out, true
}

// list snapshots all workflow records.
func (s *workflowStoreV2) list() []workflow.State {
	s.mu.RLock()
	out := make([]workflow.State, 0, len(s.states))
	for _, record := range s.states {
		out = append(out, record.snapshot())
	}
	s.mu.RUnlock()
	return out
}

// update clones workflow state, lets fn mutate the clone, and replaces the
// record only after fn succeeds.
func (s *workflowStoreV2) update(workflowID string, fn func(*workflow.State) error) (workflow.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.states[workflowID]
	if !ok {
		return workflow.State{}, errors.New("workflow not found")
	}
	state := record.snapshot()
	if err := fn(&state); err != nil {
		return workflow.State{}, err
	}
	state.UpdatedAtMs = time.Now().UnixMilli()
	record = newWorkflowRecordV2(state)
	s.states[workflowID] = record
	return state, nil
}

// upsert replaces a workflow record and refreshes its idempotency index.
func (s *workflowStoreV2) upsert(state workflow.State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[state.WorkflowID] = newWorkflowRecordV2(state)
	if state.IdempotencyKey != "" {
		s.byIdemKey[state.IdempotencyKey] = state.WorkflowID
	}
}

// newWorkflowRecordV2 captures workflow step order once so internal step indexes
// stay aligned with the cloned workflow state.
func newWorkflowRecordV2(state workflow.State) *workflowRecordV2 {
	steps := state.StepStatesInOrder()
	record := &workflowRecordV2{
		state:     workflow.CloneState(state),
		stepIndex: make(map[string]int, len(steps)),
	}
	for _, step := range steps {
		record.stepIndex[step.StepID] = len(record.steps)
		record.steps = append(record.steps, cloneWorkflowStepState(step))
	}
	return record
}

// snapshot returns a workflow clone without exposing record-owned slices.
func (r *workflowRecordV2) snapshot() workflow.State {
	return workflow.CloneState(r.state)
}

// actorStoreV2 stores actor state and idempotency indexes behind one lock.
type actorStoreV2 struct {
	mu        sync.RWMutex
	states    map[string]actor.State
	byIdemKey map[string]string
}

// init prepares actor state and idempotency maps.
func (s *actorStoreV2) init() {
	s.states = make(map[string]actor.State)
	s.byIdemKey = make(map[string]string)
}

// create stores actor state or returns the existing idempotent actor.
func (s *actorStoreV2) create(state actor.State, idempotencyKey string) (actor.State, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if idempotencyKey != "" {
		if actorID, ok := s.byIdemKey[idempotencyKey]; ok {
			return cloneActor(s.states[actorID]), true
		}
	}
	state.IdempotencyKey = idempotencyKey
	s.states[state.ActorID] = cloneActor(state)
	if idempotencyKey != "" {
		s.byIdemKey[idempotencyKey] = state.ActorID
	}
	return cloneActor(state), false
}

// get returns a cloned actor state by ID.
func (s *actorStoreV2) get(actorID string) (actor.State, bool) {
	s.mu.RLock()
	state, ok := s.states[actorID]
	out := cloneActor(state)
	s.mu.RUnlock()
	return out, ok
}

// getByIdempotencyKey resolves an actor idempotency key to cloned state.
func (s *actorStoreV2) getByIdempotencyKey(idempotencyKey string) (actor.State, bool) {
	if idempotencyKey == "" {
		return actor.State{}, false
	}
	s.mu.RLock()
	actorID, ok := s.byIdemKey[idempotencyKey]
	if !ok {
		s.mu.RUnlock()
		return actor.State{}, false
	}
	state, ok := s.states[actorID]
	out := cloneActor(state)
	s.mu.RUnlock()
	return out, ok
}

// list snapshots all actor states.
func (s *actorStoreV2) list() []actor.State {
	s.mu.RLock()
	out := make([]actor.State, 0, len(s.states))
	for _, state := range s.states {
		out = append(out, cloneActor(state))
	}
	s.mu.RUnlock()
	return out
}

// update clones actor state before invoking fn and commits the clone on success.
func (s *actorStoreV2) update(actorID string, fn func(*actor.State) error) (actor.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.states[actorID]
	if !ok {
		return actor.State{}, errors.New("actor not found")
	}
	state = cloneActor(state)
	if err := fn(&state); err != nil {
		return actor.State{}, err
	}
	state.UpdatedAtMs = time.Now().UnixMilli()
	s.states[actorID] = cloneActor(state)
	return cloneActor(state), nil
}

// upsert replaces an actor snapshot and refreshes its idempotency index.
func (s *actorStoreV2) upsert(state actor.State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[state.ActorID] = cloneActor(state)
	if state.IdempotencyKey != "" {
		s.byIdemKey[state.IdempotencyKey] = state.ActorID
	}
}

// modelStoreV2 stores model registry entries under a small mutex-protected map.
type modelStoreV2 struct {
	mu     sync.RWMutex
	models map[string]*logservepb.ModelInfo
}

// init prepares the model registry map.
func (s *modelStoreV2) init() {
	s.models = make(map[string]*logservepb.ModelInfo)
}

// register stores cloned model metadata and applies default version/adapter
// values.
func (s *modelStoreV2) register(model *logservepb.ModelInfo) *logservepb.ModelInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := cloneModel(model)
	if clone.Version == "" {
		clone.Version = "v1"
	}
	if clone.Adapter == "" {
		clone.Adapter = "mock"
	}
	s.models[ModelKey(clone.Name, clone.Version)] = clone
	return cloneModel(clone)
}

// get returns a cloned model registration, defaulting an empty version to v1.
func (s *modelStoreV2) get(name, version string) (*logservepb.ModelInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if version == "" {
		version = "v1"
	}
	model, ok := s.models[ModelKey(name, version)]
	return cloneModel(model), ok
}

// list snapshots all model registrations.
func (s *modelStoreV2) list() []*logservepb.ModelInfo {
	s.mu.RLock()
	out := make([]*logservepb.ModelInfo, 0, len(s.models))
	for _, model := range s.models {
		out = append(out, cloneModel(model))
	}
	s.mu.RUnlock()
	return out
}

// cloneWorkflowStateWithoutSteps currently delegates to workflow.CloneState; the
// name documents the historical intent of separating workflow header and step data.
func cloneWorkflowStateWithoutSteps(state workflow.State) workflow.State {
	return workflow.CloneState(state)
}

// cloneWorkflowDefinitionV2 deep-copies workflow definition slices and JSON args.
func cloneWorkflowDefinitionV2(def workflow.Definition) workflow.Definition {
	def.ArgsJSON = append([]byte(nil), def.ArgsJSON...)
	def.Steps = append([]workflow.StepDefinition(nil), def.Steps...)
	for i := range def.Steps {
		def.Steps[i].ArgsJSON = append([]byte(nil), def.Steps[i].ArgsJSON...)
		def.Steps[i].DependsOn = append([]string(nil), def.Steps[i].DependsOn...)
	}
	return def
}

// cloneWorkflowStepState copies mutable step result bytes.
func cloneWorkflowStepState(step workflow.StepState) workflow.StepState {
	step.ResultJSON = append([]byte(nil), step.ResultJSON...)
	return step
}

// cloneStringMap copies string metadata maps before they cross store boundaries.
func cloneStringMap(source map[string]string) map[string]string {
	out := make(map[string]string, len(source))
	for k, v := range source {
		out[k] = v
	}
	return out
}
