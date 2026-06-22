package control

import (
	"container/heap"
	"sort"
	"sync"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/metadata"
)

type TaskKind uint8

const (
	TaskKindGeneral TaskKind = iota
	TaskKindTargetWorker
	TaskKindActor
	TaskKindLLM
)

type modelKey struct {
	name    string
	version string
}

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

type workerSnapshot struct {
	WorkerID         string
	ActorIDs         []string
	ActorNextSeq     map[string]uint64
	CachedModels     map[modelKey]struct{}
	SchedulingPolicy logservepb.SchedulingPolicy
}

type schedulerDecision uint8

const (
	schedulerSkip schedulerDecision = iota
	schedulerAssign
	schedulerDrop
)

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

	workerViews map[string]workerView
	placement   map[modelKey]modelPlacement
}

type taskDeque struct {
	items []string
	head  int
}

type workerView struct {
	workerID      string
	cachedModels  map[modelKey]struct{}
	capacity      uint32
	runningTasks  uint32
	lastHeartbeat int64
}

type modelPlacement struct {
	cachedWorkers map[string]struct{}
	coldWorkers   map[string]struct{}
}

type runningLease struct {
	taskID     string
	deadlineMs int64
	leaseEpoch uint64
}

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
	}
}

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
	s.queueForLocked(meta).PushBack(meta.TaskID)
}

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

func (s *Scheduler) QueueDepth() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queued)
}

func (s *Scheduler) Assign(snapshot workerSnapshot, nowMs int64, check func(SchedMeta) schedulerDecision) (SchedMeta, bool) {
	if s == nil || snapshot.WorkerID == "" {
		return SchedMeta{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

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
		current, ok := s.runningLeases[top.taskID]
		if !ok || current.deadlineMs != top.deadlineMs || current.leaseEpoch != top.leaseEpoch {
			continue
		}
		delete(s.runningLeases, top.taskID)
		expired = append(expired, top)
	}
	return expired
}

func (s *Scheduler) UpsertWorker(worker metadata.Worker) {
	if s == nil || worker.WorkerID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous, ok := s.workerViews[worker.WorkerID]; ok {
		s.removeWorkerPlacementLocked(previous)
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
}

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
	hasCachedAvailable := false
	for workerID := range s.placement[key].cachedWorkers {
		worker, ok := s.workerViews[workerID]
		if ok && worker.hasCapacity() {
			hasCachedAvailable = true
			break
		}
	}
	bestWorker := ""
	bestScore := -1 << 30
	for _, workerID := range sortedWorkerIDs(s.workerViews) {
		worker := s.workerViews[workerID]
		if !worker.hasCapacity() {
			continue
		}
		available := int(worker.capacity - worker.runningTasks)
		score := available*100 - int(worker.runningTasks)*10
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
		case schedulerSkip:
			return SchedMeta{}, false
		default:
			q.PopFront()
			delete(s.queued, taskID)
			return meta, true
		}
	}
	return SchedMeta{}, false
}

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

func (s *Scheduler) queueSortMetaLocked(q *taskDeque, kind TaskKind) SchedMeta {
	meta, ok := s.frontMetaLocked(q, kind)
	if !ok {
		return SchedMeta{CreatedAtMs: 1<<63 - 1}
	}
	return meta
}

func (s *Scheduler) cachedWorkerHasCapacityLocked(key modelKey) bool {
	for workerID := range s.placement[key].cachedWorkers {
		worker, ok := s.workerViews[workerID]
		if ok && worker.hasCapacity() {
			return true
		}
	}
	return false
}

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

func (q *taskDeque) Len() int {
	if q == nil {
		return 0
	}
	return len(q.items) - q.head
}

func (q *taskDeque) PushBack(taskID string) {
	q.items = append(q.items, taskID)
}

func (q *taskDeque) PushFront(taskID string) {
	if q.head > 0 {
		q.head--
		q.items[q.head] = taskID
		return
	}
	q.items = append([]string{taskID}, q.items...)
}

func (q *taskDeque) Front() (string, bool) {
	if q.Len() == 0 {
		return "", false
	}
	return q.items[q.head], true
}

func (q *taskDeque) PopFront() (string, bool) {
	if q.Len() == 0 {
		return "", false
	}
	taskID := q.items[q.head]
	q.items[q.head] = ""
	q.head++
	if q.head > 64 && q.head*2 >= len(q.items) {
		q.items = append([]string(nil), q.items[q.head:]...)
		q.head = 0
	}
	return taskID, true
}

func (worker workerView) hasCapacity() bool {
	capacity := worker.capacity
	if capacity == 0 {
		capacity = 1
	}
	return worker.runningTasks < capacity
}

func modelKeyFromParts(name, version string) modelKey {
	if version == "" {
		version = "v1"
	}
	return modelKey{name: name, version: version}
}

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

func sortedWorkerIDs(source map[string]workerView) []string {
	out := make([]string, 0, len(source))
	for workerID := range source {
		out = append(out, workerID)
	}
	sort.Strings(out)
	return out
}

type taskDeadlineHeap []runningLease

func (h taskDeadlineHeap) Len() int { return len(h) }

func (h taskDeadlineHeap) Less(i, j int) bool {
	if h[i].deadlineMs == h[j].deadlineMs {
		if h[i].taskID == h[j].taskID {
			return h[i].leaseEpoch < h[j].leaseEpoch
		}
		return h[i].taskID < h[j].taskID
	}
	return h[i].deadlineMs < h[j].deadlineMs
}

func (h taskDeadlineHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *taskDeadlineHeap) Push(x any) {
	*h = append(*h, x.(runningLease))
}

func (h *taskDeadlineHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	old[n-1] = runningLease{}
	*h = old[:n-1]
	return item
}
