package control

import (
	"container/heap"
	"sort"
	"sync"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/metadata"
)

// This file implements the indexed task scheduler used by scheduler v2. It keeps
// separate queues for targeted work, actors, LLM models, and general tasks.
type TaskKind uint8

const (
	TaskKindGeneral TaskKind = iota
	TaskKindTargetWorker
	TaskKindActor
	TaskKindLLM
)

// modelKey is the normalized name/version key used by LLM queues and placement indexes.
type modelKey struct {
	name    string
	version string
}

// SchedMeta is the scheduler-owned projection of task metadata needed for routing,
// lease tracking, and worker eligibility checks.
type SchedMeta struct {
	TaskID        string
	TaskType      TaskKind
	TargetWorker  string
	ActorID       string
	CommandSeq    uint64
	ModelName     string
	ModelVersion  string
	CreatedAtMs   int64
	LeaseEpoch    uint64
	RunningWorker string
}

// workerSnapshot is the per-poll view of a worker used to decide which queued task
// can be safely assigned.
type workerSnapshot struct {
	WorkerID         string
	ActorIDs         []string
	ActorNextSeq     map[string]uint64
	CachedModels     map[modelKey]struct{}
	SchedulingPolicy logservepb.SchedulingPolicy
}

// schedulerDecision lets the service callback assign, skip, or drop a candidate
// without letting scheduler internals call metadata directly.
type schedulerDecision uint8

const (
	schedulerSkip schedulerDecision = iota
	schedulerAssign
	schedulerDrop
)

// Scheduler owns task queues and worker placement indexes. All fields are protected
// by mu and should only be accessed through locked helper methods.
type Scheduler struct {
	mu sync.Mutex

	readyGeneral  taskDeque
	byTarget      map[string]*taskDeque
	actorPending  map[string]*taskDeque
	llmByModel    map[modelKey]*taskDeque
	taskMeta      map[string]SchedMeta
	queued        map[string]TaskKind
	runningLeases map[string]runningLease
	deadlines     taskDeadlineHeap

	workerViews  map[string]workerView
	placement    map[modelKey]modelPlacement
	llmPlacement modelPlacementStore
}

// taskDeque is a compact FIFO deque optimized for frequent front pops without
// reallocating on every dequeue.
type taskDeque struct {
	items []string
	head  int
}

// workerView is the scheduler-local copy of worker capacity and cached model state.
type workerView struct {
	workerID      string
	cachedModels  map[modelKey]struct{}
	capacity      uint32
	runningTasks  uint32
	lastHeartbeat int64
}

// modelPlacement tracks which workers currently report a cached copy of a model.
type modelPlacement struct {
	cachedWorkers map[string]struct{}
	coldWorkers   map[string]struct{}
}

// runningLease records the lease epoch and deadline tracked for redelivery.
type runningLease struct {
	taskID     string
	deadlineMs int64
	leaseEpoch uint64
}

// newScheduler initializes all scheduler queues, indexes, and running-lease maps.
func newScheduler() *Scheduler {
	return &Scheduler{
		byTarget:      make(map[string]*taskDeque),
		actorPending:  make(map[string]*taskDeque),
		llmByModel:    make(map[modelKey]*taskDeque),
		taskMeta:      make(map[string]SchedMeta),
		queued:        make(map[string]TaskKind),
		runningLeases: make(map[string]runningLease),
		workerViews:   make(map[string]workerView),
		placement:     make(map[modelKey]modelPlacement),
		llmPlacement:  newModelPlacementStore(),
	}
}

// schedulerTaskKind classifies a task into the queue family that should own it.
func schedulerTaskKind(meta SchedMeta) TaskKind {
	if meta.ActorID != "" {
		return TaskKindActor
	}
	if meta.TargetWorker != "" {
		return TaskKindTargetWorker
	}
	if meta.ModelName != "" {
		return TaskKindLLM
	}
	return TaskKindGeneral
}

// Enqueue appends a task to the appropriate queue and updates metadata indexes,
// ignoring duplicate queue entries while refreshing task metadata.
func (s *Scheduler) Enqueue(meta SchedMeta) {
	if s == nil || meta.TaskID == "" {
		return
	}
	meta.TaskType = schedulerTaskKind(meta)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.queued[meta.TaskID]; ok {
		s.taskMeta[meta.TaskID] = meta
		return
	}
	s.taskMeta[meta.TaskID] = meta
	s.queued[meta.TaskID] = meta.TaskType
	if meta.TaskType == TaskKindLLM {
		s.ensureModelPlacementLocked(modelKeyFromParts(meta.ModelName, meta.ModelVersion))
	}
	s.queueForLocked(meta).PushBack(meta.TaskID)
}

// ReturnFront requeues a task at the front after a failed assignment attempt that
// should be retried before later tasks of the same class.
func (s *Scheduler) ReturnFront(meta SchedMeta) {
	if s == nil || meta.TaskID == "" {
		return
	}
	meta.TaskType = schedulerTaskKind(meta)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.queued[meta.TaskID]; ok {
		s.taskMeta[meta.TaskID] = meta
		return
	}
	s.taskMeta[meta.TaskID] = meta
	s.queued[meta.TaskID] = meta.TaskType
	s.queueForLocked(meta).PushFront(meta.TaskID)
}

// Forget removes scheduler metadata for a task that is gone or terminal.
func (s *Scheduler) Forget(taskID string) {
	if s == nil || taskID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.taskMeta, taskID)
	delete(s.queued, taskID)
	delete(s.runningLeases, taskID)
}

// QueueDepth returns the number of queued task IDs known to the scheduler.
func (s *Scheduler) QueueDepth() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queued)
}

// Assign finds the highest-priority task compatible with a worker snapshot and lets
// the caller validate metadata before the task leaves the scheduler queue.
func (s *Scheduler) Assign(snapshot workerSnapshot, nowMs int64, check func(SchedMeta) schedulerDecision) (SchedMeta, bool) {
	if s == nil || snapshot.WorkerID == "" {
		return SchedMeta{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Assignment priority is target-worker tasks, owned actor mailboxes, LLM locality,
	// then the general queue.
	if q := s.byTarget[snapshot.WorkerID]; q != nil {
		if meta, ok := s.nextFromQueueLocked(q, TaskKindTargetWorker, check); ok {
			return meta, true
		}
	}
	for _, actorID := range snapshot.ActorIDs {
		q := s.actorPending[actorID]
		if q == nil {
			continue
		}
		nextSeq := snapshot.ActorNextSeq[actorID]
		meta, ok := s.nextFromQueueLocked(q, TaskKindActor, func(meta SchedMeta) schedulerDecision {
			if meta.CommandSeq != 0 && nextSeq != 0 && meta.CommandSeq != nextSeq {
				return schedulerSkip
			}
			return check(meta)
		})
		if ok {
			return meta, true
		}
	}
	for _, key := range s.llmCandidateModelsLocked(snapshot, nowMs) {
		q := s.llmByModel[key]
		if q == nil {
			continue
		}
		if meta, ok := s.nextFromQueueLocked(q, TaskKindLLM, check); ok {
			return meta, true
		}
	}
	return s.nextFromQueueLocked(&s.readyGeneral, TaskKindGeneral, check)
}

// TrackRunning records a leased task deadline so redelivery can be driven by a heap
// instead of scanning all running tasks.
func (s *Scheduler) TrackRunning(taskID string, deadlineMs int64, leaseEpoch uint64) {
	if s == nil || taskID == "" || deadlineMs <= 0 || leaseEpoch == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lease := runningLease{taskID: taskID, deadlineMs: deadlineMs, leaseEpoch: leaseEpoch}
	s.runningLeases[taskID] = lease
	heap.Push(&s.deadlines, lease)
	if meta, ok := s.taskMeta[taskID]; ok {
		meta.LeaseEpoch = leaseEpoch
		s.taskMeta[taskID] = meta
	}
}

// CompleteRunning removes a task from running-lease tracking after terminal completion.
func (s *Scheduler) CompleteRunning(taskID string) {
	if s == nil || taskID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.runningLeases, taskID)
	delete(s.taskMeta, taskID)
	delete(s.queued, taskID)
}

// PopExpiredRunning returns leases whose tracked deadline has expired and discards
// stale heap entries left by lease updates.
func (s *Scheduler) PopExpiredRunning(nowMs int64) []runningLease {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	expired := make([]runningLease, 0)
	for s.deadlines.Len() > 0 {
		top := s.deadlines[0]
		if top.deadlineMs > nowMs {
			break
		}
		heap.Pop(&s.deadlines)
		// Heap entries are not updated in place; compare against runningLeases to ignore
		// stale deadlines pushed before a lease was refreshed or completed.
		current, ok := s.runningLeases[top.taskID]
		if !ok || current.deadlineMs != top.deadlineMs || current.leaseEpoch != top.leaseEpoch {
			continue
		}
		delete(s.runningLeases, top.taskID)
		expired = append(expired, top)
	}
	return expired
}

// UpsertWorker refreshes scheduler worker state and rebuilds affected model
// placement indexes.
func (s *Scheduler) UpsertWorker(worker metadata.Worker) {
	if s == nil || worker.WorkerID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var previous workerView
	if prev, ok := s.workerViews[worker.WorkerID]; ok {
		previous = prev
		s.removeWorkerPlacementLocked(previous)
		s.removeWorkerPlacementIndexLocked(worker.WorkerID, previous)
	}
	view := workerView{
		workerID:      worker.WorkerID,
		cachedModels:  modelKeySet(worker.CachedModels),
		capacity:      worker.Capacity,
		runningTasks:  worker.RunningTasks,
		lastHeartbeat: worker.LastHeartbeat,
	}
	if view.capacity == 0 {
		view.capacity = 1
	}
	s.workerViews[worker.WorkerID] = view
	s.addWorkerPlacementLocked(view)
	s.refreshWorkerPlacementLocked(worker.WorkerID, view, previous)
}

// PreferredLocalityWorker returns the worker preferred by cached-model locality,
// allowing cold workers once the task has waited long enough.
func (s *Scheduler) PreferredLocalityWorker(key modelKey, createdAtMs, nowMs int64, waitMs int64) string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	queueDelayMs := int64(0)
	if createdAtMs > 0 && nowMs > createdAtMs {
		queueDelayMs = nowMs - createdAtMs
	}
	return s.preferredLocalityFromPlacementLocked(key, queueDelayMs, waitMs)
}

// nextFromQueueLocked pops stale entries, drops invalid tasks, or returns the first
// assignable task from one queue. The caller must hold s.mu.
func (s *Scheduler) nextFromQueueLocked(q *taskDeque, kind TaskKind, check func(SchedMeta) schedulerDecision) (SchedMeta, bool) {
	for q.Len() > 0 {
		taskID, _ := q.Front()
		meta, ok := s.taskMeta[taskID]
		queuedKind, queued := s.queued[taskID]
		if !ok || !queued || queuedKind != kind {
			q.PopFront()
			continue
		}
		decision := schedulerAssign
		if check != nil {
			decision = check(meta)
		}
		switch decision {
		case schedulerDrop:
			q.PopFront()
			delete(s.queued, taskID)
			delete(s.taskMeta, taskID)
			s.maybeCleanupQueueLocked(q, kind, meta)
		case schedulerSkip:
			return SchedMeta{}, false
		default:
			q.PopFront()
			delete(s.queued, taskID)
			s.maybeCleanupQueueLocked(q, kind, meta)
			return meta, true
		}
	}
	return SchedMeta{}, false
}

// maybeCleanupQueueLocked removes empty per-key queues from their owner maps. The
// caller must hold s.mu.
func (s *Scheduler) maybeCleanupQueueLocked(q *taskDeque, kind TaskKind, meta SchedMeta) {
	if q == nil || q.Len() > 0 {
		return
	}
	switch kind {
	case TaskKindActor:
		if meta.ActorID != "" && s.actorPending[meta.ActorID] == q {
			delete(s.actorPending, meta.ActorID)
		}
	case TaskKindTargetWorker:
		if meta.TargetWorker != "" && s.byTarget[meta.TargetWorker] == q {
			delete(s.byTarget, meta.TargetWorker)
		}
	case TaskKindLLM:
		key := modelKeyFromParts(meta.ModelName, meta.ModelVersion)
		if s.llmByModel[key] == q {
			delete(s.llmByModel, key)
		}
	}
}

// ActorPendingActors returns how many actors currently have queued commands.
func (s *Scheduler) ActorPendingActors() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.actorPending)
}

// llmCandidateModelsLocked orders LLM model queues for a worker, preferring cached
// models and delaying cold assignment while cached workers still have capacity.
func (s *Scheduler) llmCandidateModelsLocked(snapshot workerSnapshot, nowMs int64) []modelKey {
	candidates := make([]modelKey, 0, len(s.llmByModel))
	seen := make(map[modelKey]struct{}, len(s.llmByModel))
	for key := range snapshot.CachedModels {
		if q := s.llmByModel[key]; q != nil && q.Len() > 0 {
			candidates = append(candidates, key)
			seen[key] = struct{}{}
		}
	}
	for key, q := range s.llmByModel {
		if q == nil || q.Len() == 0 {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		meta, ok := s.frontMetaLocked(q, TaskKindLLM)
		if !ok {
			continue
		}
		if s.cachedWorkerHasCapacityLocked(key) && nowMs-meta.CreatedAtMs < localityQueueWait.Milliseconds() {
			continue
		}
		candidates = append(candidates, key)
	}
	sort.Slice(candidates, func(i, j int) bool {
		left := s.queueSortMetaLocked(s.llmByModel[candidates[i]], TaskKindLLM)
		right := s.queueSortMetaLocked(s.llmByModel[candidates[j]], TaskKindLLM)
		if left.CreatedAtMs == right.CreatedAtMs {
			if candidates[i].name == candidates[j].name {
				return candidates[i].version < candidates[j].version
			}
			return candidates[i].name < candidates[j].name
		}
		return left.CreatedAtMs < right.CreatedAtMs
	})
	return candidates
}

// frontMetaLocked returns the first live task metadata from a queue while pruning
// stale queue entries. The caller must hold s.mu.
func (s *Scheduler) frontMetaLocked(q *taskDeque, kind TaskKind) (SchedMeta, bool) {
	for q.Len() > 0 {
		taskID, _ := q.Front()
		meta, ok := s.taskMeta[taskID]
		queuedKind, queued := s.queued[taskID]
		if !ok || !queued || queuedKind != kind {
			q.PopFront()
			continue
		}
		return meta, true
	}
	return SchedMeta{}, false
}

// queueSortMetaLocked returns front metadata or a max timestamp sentinel for sorting.
func (s *Scheduler) queueSortMetaLocked(q *taskDeque, kind TaskKind) SchedMeta {
	meta, ok := s.frontMetaLocked(q, kind)
	if !ok {
		return SchedMeta{CreatedAtMs: 1<<63 - 1}
	}
	return meta
}

// cachedWorkerHasCapacityLocked reports whether any cached worker can currently run
// the model. The caller must hold s.mu.
func (s *Scheduler) cachedWorkerHasCapacityLocked(key modelKey) bool {
	for workerID := range s.placement[key].cachedWorkers {
		worker, ok := s.workerViews[workerID]
		if ok && worker.hasCapacity() {
			return true
		}
	}
	return false
}

// queueForLocked returns or creates the queue that owns a task. The caller must hold s.mu.
func (s *Scheduler) queueForLocked(meta SchedMeta) *taskDeque {
	switch meta.TaskType {
	case TaskKindTargetWorker:
		q := s.byTarget[meta.TargetWorker]
		if q == nil {
			q = &taskDeque{}
			s.byTarget[meta.TargetWorker] = q
		}
		return q
	case TaskKindActor:
		q := s.actorPending[meta.ActorID]
		if q == nil {
			q = &taskDeque{}
			s.actorPending[meta.ActorID] = q
		}
		return q
	case TaskKindLLM:
		key := modelKeyFromParts(meta.ModelName, meta.ModelVersion)
		q := s.llmByModel[key]
		if q == nil {
			q = &taskDeque{}
			s.llmByModel[key] = q
		}
		return q
	default:
		return &s.readyGeneral
	}
}

// addWorkerPlacementLocked records cached model membership for a worker. The caller
// must hold s.mu.
func (s *Scheduler) addWorkerPlacementLocked(worker workerView) {
	for _, key := range sortedModelKeys(worker.cachedModels) {
		placement := s.placement[key]
		if placement.cachedWorkers == nil {
			placement.cachedWorkers = make(map[string]struct{})
		}
		if placement.coldWorkers == nil {
			placement.coldWorkers = make(map[string]struct{})
		}
		placement.cachedWorkers[worker.workerID] = struct{}{}
		delete(placement.coldWorkers, worker.workerID)
		s.placement[key] = placement
	}
}

// removeWorkerPlacementLocked removes a worker from all cached/cold placement maps.
// The caller must hold s.mu.
func (s *Scheduler) removeWorkerPlacementLocked(worker workerView) {
	for key, placement := range s.placement {
		delete(placement.cachedWorkers, worker.workerID)
		delete(placement.coldWorkers, worker.workerID)
		if len(placement.cachedWorkers) == 0 && len(placement.coldWorkers) == 0 {
			delete(s.placement, key)
			continue
		}
		s.placement[key] = placement
	}
}

// Len returns the number of non-popped items in the deque.
func (q *taskDeque) Len() int {
	if q == nil {
		return 0
	}
	return len(q.items) - q.head
}

// PushBack appends a task to the tail of the deque.
func (q *taskDeque) PushBack(taskID string) {
	q.items = append(q.items, taskID)
}

// PushFront prepends a task, reusing consumed head space when possible.
func (q *taskDeque) PushFront(taskID string) {
	if q.head > 0 {
		q.head--
		q.items[q.head] = taskID
		return
	}
	q.items = append([]string{taskID}, q.items...)
}

// Front returns the next task ID without removing it.
func (q *taskDeque) Front() (string, bool) {
	if q.Len() == 0 {
		return "", false
	}
	return q.items[q.head], true
}

// PopFront removes the next task ID and periodically compacts consumed head space.
func (q *taskDeque) PopFront() (string, bool) {
	if q.Len() == 0 {
		return "", false
	}
	taskID := q.items[q.head]
	q.items[q.head] = ""
	q.head++
	// Compact only after enough headroom has accumulated; this avoids per-pop copying
	// while bounding retained memory for long-lived queues.
	if q.head > 64 && q.head*2 >= len(q.items) {
		q.items = append([]string(nil), q.items[q.head:]...)
		q.head = 0
	}
	return taskID, true
}

// hasCapacity reports worker availability, treating zero capacity as one slot.
func (worker workerView) hasCapacity() bool {
	capacity := worker.capacity
	if capacity == 0 {
		capacity = 1
	}
	return worker.runningTasks < capacity
}

// modelKeyFromParts normalizes an empty version to v1.
func modelKeyFromParts(name, version string) modelKey {
	if version == "" {
		version = "v1"
	}
	return modelKey{name: name, version: version}
}

// modelKeySet converts metadata model keys into normalized scheduler keys.
func modelKeySet(source map[string]bool) map[modelKey]struct{} {
	out := make(map[modelKey]struct{}, len(source))
	for key, cached := range source {
		if !cached {
			continue
		}
		name, version := splitModelKey(key)
		out[modelKeyFromParts(name, version)] = struct{}{}
	}
	return out
}

// sortedModelKeys returns deterministic model-key order for index updates.
func sortedModelKeys(source map[modelKey]struct{}) []modelKey {
	out := make([]modelKey, 0, len(source))
	for key := range source {
		out = append(out, key)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].name == out[j].name {
			return out[i].version < out[j].version
		}
		return out[i].name < out[j].name
	})
	return out
}

// sortedWorkerIDs returns deterministic worker ID order.
func sortedWorkerIDs(source map[string]workerView) []string {
	out := make([]string, 0, len(source))
	for workerID := range source {
		out = append(out, workerID)
	}
	sort.Strings(out)
	return out
}

// taskDeadlineHeap orders running leases by deadline for efficient redelivery scans.
type taskDeadlineHeap []runningLease

// Len implements heap.Interface for taskDeadlineHeap.
func (h taskDeadlineHeap) Len() int { return len(h) }

// Less orders earlier deadlines first and uses task ID and lease epoch for stable ties.
func (h taskDeadlineHeap) Less(i, j int) bool {
	if h[i].deadlineMs == h[j].deadlineMs {
		if h[i].taskID == h[j].taskID {
			return h[i].leaseEpoch < h[j].leaseEpoch
		}
		return h[i].taskID < h[j].taskID
	}
	return h[i].deadlineMs < h[j].deadlineMs
}

// Swap implements heap.Interface for taskDeadlineHeap.
func (h taskDeadlineHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

// Push adds a running lease to the deadline heap.
func (h *taskDeadlineHeap) Push(x any) {
	*h = append(*h, x.(runningLease))
}

// Pop removes the last heap element after container/heap moves the minimum there.
func (h *taskDeadlineHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = runningLease{}
	*h = old[:n-1]
	return item
}
