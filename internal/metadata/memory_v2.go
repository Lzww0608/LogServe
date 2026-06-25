package metadata

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

const memoryTaskShardCount = 64

type MemoryStoreV2 struct {
	tasks     taskStoreV2
	workers   workerStoreV2
	workflows workflowStoreV2
	actors    actorStoreV2
	models    modelStoreV2
}

func NewMemoryStore() *MemoryStoreV2 {
	store := &MemoryStoreV2{}
	store.tasks.init()
	store.workers.init()
	store.workflows.init()
	store.actors.init()
	store.models.init()
	return store
}

func (s *MemoryStoreV2) CreateTask(task Task, idempotencyKey string) (Task, bool) {
	return s.tasks.create(task, idempotencyKey)
}

func (s *MemoryStoreV2) GetTask(taskID string) (Task, bool) {
	return s.tasks.get(taskID)
}

func (s *MemoryStoreV2) GetTaskByIdempotencyKey(idempotencyKey string) (Task, bool) {
	return s.tasks.getByIdempotencyKey(idempotencyKey)
}

func (s *MemoryStoreV2) ListTasks() []Task {
	return s.tasks.list()
}

func (s *MemoryStoreV2) ListTasksByStatus(status logservepb.TaskStatus) []Task {
	return s.tasks.listByStatus(status)
}

func (s *MemoryStoreV2) LeaseTask(taskID, workerID string) (Task, error) {
	return s.tasks.lease(taskID, workerID)
}

func (s *MemoryStoreV2) ValidateTaskLease(taskID, workerID string, leaseEpoch uint64) (Task, error) {
	return s.tasks.validateLease(taskID, workerID, leaseEpoch)
}

func (s *MemoryStoreV2) RequeueExpiredRunningTasks(maxAge time.Duration) []Task {
	return s.tasks.requeueExpiredRunning(maxAge)
}

func (s *MemoryStoreV2) RequeueTaskIfLeaseExpired(taskID string, leaseEpoch uint64, maxAge time.Duration) (Task, bool) {
	return s.tasks.requeueTaskIfLeaseExpired(taskID, leaseEpoch, maxAge)
}

func (s *MemoryStoreV2) CompleteTask(taskID, workerID string, leaseEpoch uint64, status logservepb.TaskStatus, resultJSON []byte, taskErr string) (Task, error) {
	return s.tasks.complete(taskID, workerID, leaseEpoch, status, resultJSON, taskErr)
}

func (s *MemoryStoreV2) RegisterModel(model *logservepb.ModelInfo) *logservepb.ModelInfo {
	return s.models.register(model)
}

func (s *MemoryStoreV2) GetModel(name, version string) (*logservepb.ModelInfo, bool) {
	return s.models.get(name, version)
}

func (s *MemoryStoreV2) ListModels() []*logservepb.ModelInfo {
	return s.models.list()
}

func (s *MemoryStoreV2) CreateWorkflow(state workflow.State, idempotencyKey string) (workflow.State, bool) {
	return s.workflows.create(state, idempotencyKey)
}

func (s *MemoryStoreV2) GetWorkflow(workflowID string) (workflow.State, bool) {
	return s.workflows.get(workflowID)
}

func (s *MemoryStoreV2) GetWorkflowByIdempotencyKey(idempotencyKey string) (workflow.State, bool) {
	return s.workflows.getByIdempotencyKey(idempotencyKey)
}

func (s *MemoryStoreV2) ListWorkflows() []workflow.State {
	return s.workflows.list()
}

func (s *MemoryStoreV2) UpdateWorkflow(workflowID string, fn func(*workflow.State) error) (workflow.State, error) {
	return s.workflows.update(workflowID, fn)
}

func (s *MemoryStoreV2) UpsertWorkflow(state workflow.State) {
	s.workflows.upsert(state)
}

func (s *MemoryStoreV2) UpsertWorker(worker Worker) {
	s.workers.upsert(worker)
}

func (s *MemoryStoreV2) GetWorker(workerID string) (Worker, bool) {
	return s.workers.get(workerID)
}

func (s *MemoryStoreV2) ActiveWorkers(maxAge time.Duration) []Worker {
	return s.workers.active(maxAge)
}

func (s *MemoryStoreV2) ListWorkers() []Worker {
	return s.workers.list()
}

func (s *MemoryStoreV2) Heartbeat(workerID string, cachedModels map[string]bool) (Worker, bool) {
	return s.workers.heartbeat(workerID, cachedModels)
}

func (s *MemoryStoreV2) IncrementWorkerLoad(workerID string) {
	s.workers.incrementLoad(workerID)
}

func (s *MemoryStoreV2) DecrementWorkerLoad(workerID string) {
	s.workers.decrementLoad(workerID)
}

func (s *MemoryStoreV2) CreateActor(state actor.State, idempotencyKey string) (actor.State, bool) {
	return s.actors.create(state, idempotencyKey)
}

func (s *MemoryStoreV2) GetActor(actorID string) (actor.State, bool) {
	return s.actors.get(actorID)
}

func (s *MemoryStoreV2) GetActorByIdempotencyKey(idempotencyKey string) (actor.State, bool) {
	return s.actors.getByIdempotencyKey(idempotencyKey)
}

func (s *MemoryStoreV2) ListActors() []actor.State {
	return s.actors.list()
}

func (s *MemoryStoreV2) UpdateActor(actorID string, fn func(*actor.State) error) (actor.State, error) {
	return s.actors.update(actorID, fn)
}

func (s *MemoryStoreV2) UpsertActor(state actor.State) {
	s.actors.upsert(state)
}

type taskStoreV2 struct {
	shards [memoryTaskShardCount]taskShardV2

	idemMu sync.RWMutex
	byIdem map[string]string

	indexMu  sync.RWMutex
	allTasks map[string]struct{}
	byStatus map[logservepb.TaskStatus]map[string]struct{}
	byWorker map[string]map[string]struct{}

	deadlineMu sync.Mutex
	running    taskDeadlineQueueV2
}

type taskShardV2 struct {
	mu    sync.RWMutex
	tasks map[string]*Task
}

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

func (s *taskStoreV2) create(task Task, idempotencyKey string) (Task, bool) {
	if idempotencyKey != "" {
		s.idemMu.Lock()
		defer s.idemMu.Unlock()
		if taskID, ok := s.byIdem[idempotencyKey]; ok {
			if existing, ok := s.get(taskID); ok {
				return existing, true
			}
			delete(s.byIdem, idempotencyKey)
		}
		created := s.createWithoutIdemLock(task, idempotencyKey)
		s.byIdem[idempotencyKey] = created.TaskID
		return created, false
	}
	return s.createWithoutIdemLock(task, ""), false
}

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

	s.replaceTaskIndexes(previous, &stored)
	return cloneTask(stored)
}

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
		task, ok := s.requeueTaskAt(deadline.taskID, deadline.leaseEpoch, maxAge, time.Now().UnixMilli())
		if ok {
			requeued = append(requeued, task)
		}
	}
	return requeued
}

func (s *taskStoreV2) requeueTaskIfLeaseExpired(taskID string, leaseEpoch uint64, maxAge time.Duration) (Task, bool) {
	return s.requeueTaskAt(taskID, leaseEpoch, maxAge, time.Now().UnixMilli())
}

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
	task.ResultJSON = append([]byte(nil), resultJSON...)
	task.Error = taskErr
	task.UpdatedAtMs = time.Now().UnixMilli()
	current := cloneTask(*task)
	shard.mu.Unlock()

	s.replaceTaskIndexes(&previous, &current)
	return current, nil
}

func (s *taskStoreV2) shard(taskID string) *taskShardV2 {
	return &s.shards[taskShardIndex(taskID)]
}

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

func (s *taskStoreV2) replaceTaskIndexes(previous, current *Task) {
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

	if previous != nil && previous.Status == logservepb.TaskStatus_TASK_STATUS_RUNNING {
		if current == nil || current.Status != logservepb.TaskStatus_TASK_STATUS_RUNNING || current.TaskLeaseEpoch != previous.TaskLeaseEpoch {
			s.untrackRunning(previous.TaskID)
		}
	}
	if current != nil && current.Status == logservepb.TaskStatus_TASK_STATUS_RUNNING {
		s.trackRunning(*current)
	}
}

func (s *taskStoreV2) addStatusIndexLocked(status logservepb.TaskStatus, taskID string) {
	ids := s.byStatus[status]
	if ids == nil {
		ids = make(map[string]struct{})
		s.byStatus[status] = ids
	}
	ids[taskID] = struct{}{}
}

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

func (s *taskStoreV2) addWorkerIndexLocked(workerID, taskID string) {
	ids := s.byWorker[workerID]
	if ids == nil {
		ids = make(map[string]struct{})
		s.byWorker[workerID] = ids
	}
	ids[taskID] = struct{}{}
}

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

func (s *taskStoreV2) trackRunning(task Task) {
	if task.Status != logservepb.TaskStatus_TASK_STATUS_RUNNING || task.TaskLeaseEpoch == 0 {
		return
	}
	s.deadlineMu.Lock()
	s.running.upsert(taskDeadlineV2{taskID: task.TaskID, updatedAtMs: task.UpdatedAtMs, leaseEpoch: task.TaskLeaseEpoch})
	s.deadlineMu.Unlock()
}

func (s *taskStoreV2) untrackRunning(taskID string) {
	s.deadlineMu.Lock()
	s.running.remove(taskID)
	s.deadlineMu.Unlock()
}

func (s *taskStoreV2) popRunningBefore(cutoffMs int64) (taskDeadlineV2, bool) {
	s.deadlineMu.Lock()
	defer s.deadlineMu.Unlock()
	if s.running.Len() == 0 || s.running.entries[0].updatedAtMs > cutoffMs {
		return taskDeadlineV2{}, false
	}
	return heap.Pop(&s.running).(taskDeadlineV2), true
}

type taskDeadlineV2 struct {
	taskID      string
	updatedAtMs int64
	leaseEpoch  uint64
}

type taskDeadlineQueueV2 struct {
	entries   []taskDeadlineV2
	positions map[string]int
}

func (h *taskDeadlineQueueV2) init() {
	h.positions = make(map[string]int)
}

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

func (h *taskDeadlineQueueV2) remove(taskID string) {
	if idx, ok := h.positions[taskID]; ok {
		heap.Remove(h, idx)
	}
}

func (h *taskDeadlineQueueV2) Len() int { return len(h.entries) }

func (h *taskDeadlineQueueV2) Less(i, j int) bool {
	if h.entries[i].updatedAtMs == h.entries[j].updatedAtMs {
		if h.entries[i].taskID == h.entries[j].taskID {
			return h.entries[i].leaseEpoch < h.entries[j].leaseEpoch
		}
		return h.entries[i].taskID < h.entries[j].taskID
	}
	return h.entries[i].updatedAtMs < h.entries[j].updatedAtMs
}

func (h *taskDeadlineQueueV2) Swap(i, j int) {
	h.entries[i], h.entries[j] = h.entries[j], h.entries[i]
	h.positions[h.entries[i].taskID] = i
	h.positions[h.entries[j].taskID] = j
}

func (h *taskDeadlineQueueV2) Push(x any) {
	entry := x.(taskDeadlineV2)
	h.positions[entry.taskID] = len(h.entries)
	h.entries = append(h.entries, entry)
}

func (h *taskDeadlineQueueV2) Pop() any {
	old := h.entries
	last := old[len(old)-1]
	delete(h.positions, last.taskID)
	h.entries = old[:len(old)-1]
	return last
}

func taskShardIndex(taskID string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(taskID); i++ {
		h ^= uint32(taskID[i])
		h *= 16777619
	}
	return h % memoryTaskShardCount
}

func isTerminalTaskStatus(status logservepb.TaskStatus) bool {
	return status == logservepb.TaskStatus_TASK_STATUS_SUCCEEDED || status == logservepb.TaskStatus_TASK_STATUS_FAILED
}

func indexedTaskStatus(status logservepb.TaskStatus) bool {
	return status == logservepb.TaskStatus_TASK_STATUS_QUEUED || status == logservepb.TaskStatus_TASK_STATUS_RUNNING
}

type workerStoreV2 struct {
	mu      sync.RWMutex
	workers map[string]*workerRecordV2
}

type workerRecordV2 struct {
	mu       sync.RWMutex
	workerID string
	address  string
	labels   map[string]string

	capacity      atomic.Uint32
	runningTasks  atomic.Uint32
	lastHeartbeat atomic.Int64
	cachedModels  atomic.Value
}

func (s *workerStoreV2) init() {
	s.workers = make(map[string]*workerRecordV2)
}

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
	if !existed {
		record.runningTasks.Store(worker.RunningTasks)
	}
	record.lastHeartbeat.Store(worker.LastHeartbeat)
	record.cachedModels.Store(cloneModelCache(worker.CachedModels))
}

func (s *workerStoreV2) get(workerID string) (Worker, bool) {
	record, ok := s.record(workerID)
	if !ok {
		return Worker{}, false
	}
	return record.snapshot(), true
}

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

func (s *workerStoreV2) list() []Worker {
	s.mu.RLock()
	out := make([]Worker, 0, len(s.workers))
	for _, record := range s.workers {
		out = append(out, record.snapshot())
	}
	s.mu.RUnlock()
	return out
}

func (s *workerStoreV2) heartbeat(workerID string, cachedModels map[string]bool) (Worker, bool) {
	record, existed := s.getOrCreate(workerID)
	if cachedModels != nil {
		record.cachedModels.Store(cloneModelCache(cachedModels))
	}
	if record.capacity.Load() == 0 {
		record.capacity.Store(1)
	}
	record.lastHeartbeat.Store(time.Now().UnixMilli())
	return record.snapshot(), existed
}

func (s *workerStoreV2) incrementLoad(workerID string) {
	record, _ := s.getOrCreate(workerID)
	if record.capacity.Load() == 0 {
		record.capacity.Store(1)
	}
	record.runningTasks.Add(1)
	record.lastHeartbeat.Store(time.Now().UnixMilli())
}

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

func (s *workerStoreV2) record(workerID string) (*workerRecordV2, bool) {
	s.mu.RLock()
	record, ok := s.workers[workerID]
	s.mu.RUnlock()
	return record, ok
}

func (s *workerStoreV2) records() []*workerRecordV2 {
	s.mu.RLock()
	out := make([]*workerRecordV2, 0, len(s.workers))
	for _, record := range s.workers {
		out = append(out, record)
	}
	s.mu.RUnlock()
	return out
}

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

type workflowStoreV2 struct {
	mu        sync.RWMutex
	states    map[string]*workflowRecordV2
	byIdemKey map[string]string
}

type workflowRecordV2 struct {
	state     workflow.State
	steps     []workflow.StepState
	stepIndex map[string]int
}

func (s *workflowStoreV2) init() {
	s.states = make(map[string]*workflowRecordV2)
	s.byIdemKey = make(map[string]string)
}

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

func (s *workflowStoreV2) list() []workflow.State {
	s.mu.RLock()
	out := make([]workflow.State, 0, len(s.states))
	for _, record := range s.states {
		out = append(out, record.snapshot())
	}
	s.mu.RUnlock()
	return out
}

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

func (s *workflowStoreV2) upsert(state workflow.State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[state.WorkflowID] = newWorkflowRecordV2(state)
	if state.IdempotencyKey != "" {
		s.byIdemKey[state.IdempotencyKey] = state.WorkflowID
	}
}

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

func (r *workflowRecordV2) snapshot() workflow.State {
	return workflow.CloneState(r.state)
}

type actorStoreV2 struct {
	mu        sync.RWMutex
	states    map[string]actor.State
	byIdemKey map[string]string
}

func (s *actorStoreV2) init() {
	s.states = make(map[string]actor.State)
	s.byIdemKey = make(map[string]string)
}

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

func (s *actorStoreV2) get(actorID string) (actor.State, bool) {
	s.mu.RLock()
	state, ok := s.states[actorID]
	out := cloneActor(state)
	s.mu.RUnlock()
	return out, ok
}

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

func (s *actorStoreV2) list() []actor.State {
	s.mu.RLock()
	out := make([]actor.State, 0, len(s.states))
	for _, state := range s.states {
		out = append(out, cloneActor(state))
	}
	s.mu.RUnlock()
	return out
}

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

func (s *actorStoreV2) upsert(state actor.State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[state.ActorID] = cloneActor(state)
	if state.IdempotencyKey != "" {
		s.byIdemKey[state.IdempotencyKey] = state.ActorID
	}
}

type modelStoreV2 struct {
	mu     sync.RWMutex
	models map[string]*logservepb.ModelInfo
}

func (s *modelStoreV2) init() {
	s.models = make(map[string]*logservepb.ModelInfo)
}

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

func (s *modelStoreV2) get(name, version string) (*logservepb.ModelInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if version == "" {
		version = "v1"
	}
	model, ok := s.models[ModelKey(name, version)]
	return cloneModel(model), ok
}

func (s *modelStoreV2) list() []*logservepb.ModelInfo {
	s.mu.RLock()
	out := make([]*logservepb.ModelInfo, 0, len(s.models))
	for _, model := range s.models {
		out = append(out, cloneModel(model))
	}
	s.mu.RUnlock()
	return out
}

func cloneWorkflowStateWithoutSteps(state workflow.State) workflow.State {
	return workflow.CloneState(state)
}

func cloneWorkflowDefinitionV2(def workflow.Definition) workflow.Definition {
	def.ArgsJSON = append([]byte(nil), def.ArgsJSON...)
	def.Steps = append([]workflow.StepDefinition(nil), def.Steps...)
	for i := range def.Steps {
		def.Steps[i].ArgsJSON = append([]byte(nil), def.Steps[i].ArgsJSON...)
		def.Steps[i].DependsOn = append([]string(nil), def.Steps[i].DependsOn...)
	}
	return def
}

func cloneWorkflowStepState(step workflow.StepState) workflow.StepState {
	step.ResultJSON = append([]byte(nil), step.ResultJSON...)
	return step
}

func cloneStringMap(source map[string]string) map[string]string {
	out := make(map[string]string, len(source))
	for k, v := range source {
		out[k] = v
	}
	return out
}
