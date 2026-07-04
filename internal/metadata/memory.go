package metadata

// This file contains the original mutex-backed metadata store. It is kept as a
// simple reference implementation and as a compatibility baseline for the sharded
// V2 store.

import (
	"errors"
	"sync"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/actor"
	"github.com/logserve/logserve/internal/workflow"
)

// Task is the scheduler-visible record for one unit of work. Lease fields fence
// worker completions, while workflow, actor, and LLM fields attach the task to
// higher-level runtime state.
type Task struct {
	TaskID                 string
	TaskName               string
	Status                 logservepb.TaskStatus
	ResultJSON             []byte
	Error                  string
	WorkerID               string
	WorkflowID             string
	StepID                 string
	TargetWorkerID         string
	ActorID                string
	ActorCallID            string
	ActorEpoch             uint64
	ActorCommandSeq        uint64
	TaskLeaseEpoch         uint64
	LLMModelName           string
	LLMModelVersion        string
	IdempotencyKey         string
	IdempotencyFingerprint string
	CreatedAtMs            int64
	UpdatedAtMs            int64
}

// Worker records scheduler capacity and liveness data for one worker process.
// CachedModels and Labels are mutable maps and must be cloned at store boundaries.
type Worker struct {
	WorkerID      string
	Address       string
	Labels        map[string]string
	CachedModels  map[string]bool
	Capacity      uint32
	RunningTasks  uint32
	LastHeartbeat int64
}

// MemoryStore is a single-lock in-memory Store implementation. It favors simple
// correctness and defensive cloning over concurrency throughput.
type MemoryStore struct {
	mu                sync.RWMutex
	tasks             map[string]Task
	taskByIdemKey     map[string]string
	workers           map[string]Worker
	workflows         map[string]workflow.State
	workflowByIdemKey map[string]string
	actors            map[string]actor.State
	actorByIdemKey    map[string]string
	models            map[string]*logservepb.ModelInfo
}

// NewLegacyMemoryStore constructs the original single-lock memory store used as a
// compatibility baseline for tests and benchmarks.
func NewLegacyMemoryStore() *MemoryStore {
	return &MemoryStore{
		tasks:             make(map[string]Task),
		taskByIdemKey:     make(map[string]string),
		workers:           make(map[string]Worker),
		workflows:         make(map[string]workflow.State),
		workflowByIdemKey: make(map[string]string),
		actors:            make(map[string]actor.State),
		actorByIdemKey:    make(map[string]string),
		models:            make(map[string]*logservepb.ModelInfo),
	}
}

// CreateTask inserts a task unless the idempotency key already maps to an
// existing task. The returned bool reports whether the existing task was reused.
func (s *MemoryStore) CreateTask(task Task, idempotencyKey string) (Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if idempotencyKey != "" {
		if taskID, ok := s.taskByIdemKey[idempotencyKey]; ok {
			return s.tasks[taskID], true
		}
	}

	now := time.Now().UnixMilli()
	task.CreatedAtMs = now
	task.UpdatedAtMs = now
	task.IdempotencyKey = idempotencyKey
	s.tasks[task.TaskID] = cloneTask(task)
	if idempotencyKey != "" {
		s.taskByIdemKey[idempotencyKey] = task.TaskID
	}
	return task, false
}

// GetTask returns a defensive copy of the task with the given ID.
func (s *MemoryStore) GetTask(taskID string) (Task, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	task, ok := s.tasks[taskID]
	return cloneTask(task), ok
}

// GetTaskByIdempotencyKey resolves a non-empty idempotency key to its original
// task and returns a defensive copy.
func (s *MemoryStore) GetTaskByIdempotencyKey(idempotencyKey string) (Task, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if idempotencyKey == "" {
		return Task{}, false
	}
	taskID, ok := s.taskByIdemKey[idempotencyKey]
	if !ok {
		return Task{}, false
	}
	task, ok := s.tasks[taskID]
	return cloneTask(task), ok
}

// ListTasks snapshots every task without exposing store-owned byte slices.
func (s *MemoryStore) ListTasks() []Task {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Task, 0, len(s.tasks))
	for _, task := range s.tasks {
		out = append(out, cloneTask(task))
	}
	return out
}

// LeaseTask moves a non-terminal task into RUNNING state and increments its
// lease epoch so stale workers can be fenced at completion time.
func (s *MemoryStore) LeaseTask(taskID, workerID string) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return Task{}, errors.New("task not found")
	}
	if task.Status == logservepb.TaskStatus_TASK_STATUS_SUCCEEDED || task.Status == logservepb.TaskStatus_TASK_STATUS_FAILED {
		return cloneTask(task), nil
	}
	task.TaskLeaseEpoch++
	task.Status = logservepb.TaskStatus_TASK_STATUS_RUNNING
	task.WorkerID = workerID
	task.UpdatedAtMs = time.Now().UnixMilli()
	s.tasks[taskID] = task
	return cloneTask(task), nil
}

// ValidateTaskLease verifies that a worker still owns the current running lease.
// Terminal tasks are treated as already settled and returned without error.
func (s *MemoryStore) ValidateTaskLease(taskID, workerID string, leaseEpoch uint64) (Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return Task{}, errors.New("task not found")
	}
	if task.Status == logservepb.TaskStatus_TASK_STATUS_SUCCEEDED || task.Status == logservepb.TaskStatus_TASK_STATUS_FAILED {
		return cloneTask(task), nil
	}
	if task.Status != logservepb.TaskStatus_TASK_STATUS_RUNNING {
		return Task{}, errors.New("stale task lease rejected")
	}
	if task.WorkerID == "" || workerID == "" || task.WorkerID != workerID {
		return Task{}, errors.New("stale task lease rejected")
	}
	if task.TaskLeaseEpoch == 0 || leaseEpoch == 0 || task.TaskLeaseEpoch != leaseEpoch {
		return Task{}, errors.New("stale task lease rejected")
	}
	return cloneTask(task), nil
}

// RequeueExpiredRunningTasks scans all running tasks and returns those whose
// lease age exceeded maxAge after moving them back to QUEUED.
func (s *MemoryStore) RequeueExpiredRunningTasks(maxAge time.Duration) []Task {
	s.mu.Lock()
	defer s.mu.Unlock()

	if maxAge <= 0 {
		return nil
	}
	now := time.Now().UnixMilli()
	cutoffMs := maxAge.Milliseconds()
	requeued := make([]Task, 0)
	for id, task := range s.tasks {
		if task.Status != logservepb.TaskStatus_TASK_STATUS_RUNNING {
			continue
		}
		if now-task.UpdatedAtMs < cutoffMs {
			continue
		}
		requeuedTask := task
		task.Status = logservepb.TaskStatus_TASK_STATUS_QUEUED
		task.WorkerID = ""
		task.UpdatedAtMs = now
		s.tasks[id] = task
		requeuedTask.Status = logservepb.TaskStatus_TASK_STATUS_QUEUED
		requeuedTask.UpdatedAtMs = now
		requeued = append(requeued, cloneTask(requeuedTask))
	}
	return requeued
}

// RequeueTaskIfLeaseExpired performs targeted lease recovery only when the caller
// still references the current lease epoch for that running task.
func (s *MemoryStore) RequeueTaskIfLeaseExpired(taskID string, leaseEpoch uint64, maxAge time.Duration) (Task, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if maxAge <= 0 || taskID == "" || leaseEpoch == 0 {
		return Task{}, false
	}
	task, ok := s.tasks[taskID]
	if !ok {
		return Task{}, false
	}
	if task.Status != logservepb.TaskStatus_TASK_STATUS_RUNNING || task.TaskLeaseEpoch != leaseEpoch {
		return cloneTask(task), false
	}
	now := time.Now().UnixMilli()
	if now-task.UpdatedAtMs < maxAge.Milliseconds() {
		return cloneTask(task), false
	}
	requeuedTask := task
	task.Status = logservepb.TaskStatus_TASK_STATUS_QUEUED
	task.WorkerID = ""
	task.UpdatedAtMs = now
	s.tasks[taskID] = task
	requeuedTask.Status = logservepb.TaskStatus_TASK_STATUS_QUEUED
	requeuedTask.UpdatedAtMs = now
	return cloneTask(requeuedTask), true
}

// CompleteTask persists a terminal task result after checking the worker and
// lease epoch. Stale completions are rejected so requeued work cannot be overwritten.
func (s *MemoryStore) CompleteTask(taskID, workerID string, leaseEpoch uint64, status logservepb.TaskStatus, resultJSON []byte, taskErr string) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	task, ok := s.tasks[taskID]
	if !ok {
		return Task{}, errors.New("task not found")
	}
	if task.Status == logservepb.TaskStatus_TASK_STATUS_SUCCEEDED || task.Status == logservepb.TaskStatus_TASK_STATUS_FAILED {
		return cloneTask(task), nil
	}
	if task.Status != logservepb.TaskStatus_TASK_STATUS_RUNNING {
		return Task{}, errors.New("stale task lease rejected")
	}
	if task.WorkerID == "" || workerID == "" || task.WorkerID != workerID {
		return Task{}, errors.New("stale task lease rejected")
	}
	if task.TaskLeaseEpoch == 0 || leaseEpoch == 0 || task.TaskLeaseEpoch != leaseEpoch {
		return Task{}, errors.New("stale task lease rejected")
	}
	task.Status = status
	task.WorkerID = workerID
	task.ResultJSON = append([]byte(nil), resultJSON...)
	task.Error = taskErr
	task.UpdatedAtMs = time.Now().UnixMilli()
	s.tasks[taskID] = task
	return cloneTask(task), nil
}

// RegisterModel stores model metadata, filling the same default version and
// adapter values used by lookup paths.
func (s *MemoryStore) RegisterModel(model *logservepb.ModelInfo) *logservepb.ModelInfo {
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

// GetModel returns a cloned model registration, defaulting an empty version to v1.
func (s *MemoryStore) GetModel(name, version string) (*logservepb.ModelInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if version == "" {
		version = "v1"
	}
	model, ok := s.models[ModelKey(name, version)]
	return cloneModel(model), ok
}

// CreateWorkflow records initial workflow state or returns the prior state for a
// repeated idempotency key.
func (s *MemoryStore) CreateWorkflow(state workflow.State, idempotencyKey string) (workflow.State, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if idempotencyKey != "" {
		if workflowID, ok := s.workflowByIdemKey[idempotencyKey]; ok {
			return cloneWorkflow(s.workflows[workflowID]), true
		}
	}
	state.IdempotencyKey = idempotencyKey
	s.workflows[state.WorkflowID] = cloneWorkflow(state)
	if idempotencyKey != "" {
		s.workflowByIdemKey[idempotencyKey] = state.WorkflowID
	}
	return cloneWorkflow(state), false
}

// GetWorkflow returns a deep copy of workflow runtime state.
func (s *MemoryStore) GetWorkflow(workflowID string) (workflow.State, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.workflows[workflowID]
	return cloneWorkflow(state), ok
}

// GetWorkflowByIdempotencyKey resolves a workflow idempotency key to the original
// workflow state.
func (s *MemoryStore) GetWorkflowByIdempotencyKey(idempotencyKey string) (workflow.State, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if idempotencyKey == "" {
		return workflow.State{}, false
	}
	workflowID, ok := s.workflowByIdemKey[idempotencyKey]
	if !ok {
		return workflow.State{}, false
	}
	state, ok := s.workflows[workflowID]
	return cloneWorkflow(state), ok
}

// ListWorkflows snapshots all workflow states with cloned step payloads.
func (s *MemoryStore) ListWorkflows() []workflow.State {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]workflow.State, 0, len(s.workflows))
	for _, state := range s.workflows {
		out = append(out, cloneWorkflow(state))
	}
	return out
}

// UpdateWorkflow runs fn under the store lock and commits the mutated workflow
// state only if fn succeeds.
func (s *MemoryStore) UpdateWorkflow(workflowID string, fn func(*workflow.State) error) (workflow.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, ok := s.workflows[workflowID]
	if !ok {
		return workflow.State{}, errors.New("workflow not found")
	}
	if err := fn(&state); err != nil {
		return workflow.State{}, err
	}
	state.UpdatedAtMs = time.Now().UnixMilli()
	s.workflows[workflowID] = cloneWorkflow(state)
	return cloneWorkflow(state), nil
}

// UpsertWorkflow installs a workflow snapshot and refreshes the idempotency index
// when the state carries a key.
func (s *MemoryStore) UpsertWorkflow(state workflow.State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workflows[state.WorkflowID] = cloneWorkflow(state)
	if state.IdempotencyKey != "" {
		s.workflowByIdemKey[state.IdempotencyKey] = state.WorkflowID
	}
}

// UpsertWorker registers or refreshes worker metadata while preserving the
// current running task count for existing workers.
func (s *MemoryStore) UpsertWorker(worker Worker) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if worker.Labels == nil {
		worker.Labels = map[string]string{}
	}
	if worker.CachedModels == nil {
		worker.CachedModels = map[string]bool{}
	}
	if worker.Capacity == 0 {
		worker.Capacity = 1
	}
	if existing, ok := s.workers[worker.WorkerID]; ok {
		worker.RunningTasks = existing.RunningTasks
	}
	if worker.LastHeartbeat == 0 {
		worker.LastHeartbeat = time.Now().UnixMilli()
	}
	s.workers[worker.WorkerID] = cloneWorker(worker)
}

// GetWorker returns a cloned worker record so Labels and CachedModels remain
// owned by the store.
func (s *MemoryStore) GetWorker(workerID string) (Worker, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	worker, ok := s.workers[workerID]
	return cloneWorker(worker), ok
}

// ActiveWorkers returns workers whose heartbeat is fresh enough for scheduling;
// non-positive maxAge disables the freshness filter.
func (s *MemoryStore) ActiveWorkers(maxAge time.Duration) []Worker {
	s.mu.RLock()
	defer s.mu.RUnlock()

	now := time.Now().UnixMilli()
	maxAgeMs := maxAge.Milliseconds()
	out := make([]Worker, 0, len(s.workers))
	for _, worker := range s.workers {
		if maxAge <= 0 || now-worker.LastHeartbeat <= maxAgeMs {
			out = append(out, cloneWorker(worker))
		}
	}
	return out
}

// ListWorkers snapshots all worker records.
func (s *MemoryStore) ListWorkers() []Worker {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Worker, 0, len(s.workers))
	for _, worker := range s.workers {
		out = append(out, cloneWorker(worker))
	}
	return out
}

// Heartbeat updates worker liveness and, when provided, replaces the cached model
// set with a cloned copy.
func (s *MemoryStore) Heartbeat(workerID string, cachedModels map[string]bool) (Worker, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	worker, ok := s.workers[workerID]
	if !ok {
		worker = Worker{WorkerID: workerID, Labels: map[string]string{}, CachedModels: map[string]bool{}, Capacity: 1}
	}
	if cachedModels != nil {
		worker.CachedModels = cloneModelCache(cachedModels)
	}
	if worker.Capacity == 0 {
		worker.Capacity = 1
	}
	worker.LastHeartbeat = time.Now().UnixMilli()
	s.workers[workerID] = worker
	return cloneWorker(worker), ok
}

// IncrementWorkerLoad creates a default worker record if needed and increments
// its running task count for scheduler placement decisions.
func (s *MemoryStore) IncrementWorkerLoad(workerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	worker, ok := s.workers[workerID]
	if !ok {
		worker = Worker{WorkerID: workerID, Labels: map[string]string{}, CachedModels: map[string]bool{}, Capacity: 1}
	}
	if worker.Capacity == 0 {
		worker.Capacity = 1
	}
	worker.RunningTasks++
	worker.LastHeartbeat = time.Now().UnixMilli()
	s.workers[workerID] = worker
}

// DecrementWorkerLoad lowers a worker running count without letting it underflow.
func (s *MemoryStore) DecrementWorkerLoad(workerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	worker, ok := s.workers[workerID]
	if !ok {
		return
	}
	if worker.RunningTasks > 0 {
		worker.RunningTasks--
	}
	s.workers[workerID] = worker
}

// CreateActor records actor state or returns the existing state for a repeated
// idempotency key.
func (s *MemoryStore) CreateActor(state actor.State, idempotencyKey string) (actor.State, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if idempotencyKey != "" {
		if actorID, ok := s.actorByIdemKey[idempotencyKey]; ok {
			return cloneActor(s.actors[actorID]), true
		}
	}
	state.IdempotencyKey = idempotencyKey
	s.actors[state.ActorID] = cloneActor(state)
	if idempotencyKey != "" {
		s.actorByIdemKey[idempotencyKey] = state.ActorID
	}
	return cloneActor(state), false
}

// GetActor returns a cloned actor state snapshot.
func (s *MemoryStore) GetActor(actorID string) (actor.State, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.actors[actorID]
	return cloneActor(state), ok
}

// GetActorByIdempotencyKey resolves an actor idempotency key to the original
// actor state.
func (s *MemoryStore) GetActorByIdempotencyKey(idempotencyKey string) (actor.State, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if idempotencyKey == "" {
		return actor.State{}, false
	}
	actorID, ok := s.actorByIdemKey[idempotencyKey]
	if !ok {
		return actor.State{}, false
	}
	state, ok := s.actors[actorID]
	return cloneActor(state), ok
}

// ListActors snapshots all actor states.
func (s *MemoryStore) ListActors() []actor.State {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]actor.State, 0, len(s.actors))
	for _, state := range s.actors {
		out = append(out, cloneActor(state))
	}
	return out
}

// UpdateActor applies fn to a private copy of actor state and commits it only
// when fn succeeds.
func (s *MemoryStore) UpdateActor(actorID string, fn func(*actor.State) error) (actor.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, ok := s.actors[actorID]
	if !ok {
		return actor.State{}, errors.New("actor not found")
	}
	if err := fn(&state); err != nil {
		return actor.State{}, err
	}
	state.UpdatedAtMs = time.Now().UnixMilli()
	s.actors[actorID] = cloneActor(state)
	return cloneActor(state), nil
}

// UpsertActor installs an actor snapshot and refreshes its idempotency index when
// present.
func (s *MemoryStore) UpsertActor(state actor.State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actors[state.ActorID] = cloneActor(state)
	if state.IdempotencyKey != "" {
		s.actorByIdemKey[state.IdempotencyKey] = state.ActorID
	}
}

// cloneTask copies mutable task result bytes before crossing the store boundary.
func cloneTask(task Task) Task {
	task.ResultJSON = append([]byte(nil), task.ResultJSON...)
	return task
}

// ModelKey builds the canonical name:version lookup key, defaulting an empty
// version to v1 for compatibility with callers that omit it.
func ModelKey(name, version string) string {
	if version == "" {
		version = "v1"
	}
	return name + ":" + version
}

// ListModels returns cloned model registrations.
func (s *MemoryStore) ListModels() []*logservepb.ModelInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*logservepb.ModelInfo, 0, len(s.models))
	for _, model := range s.models {
		out = append(out, cloneModel(model))
	}
	return out
}

// cloneWorkflow delegates to the workflow package so nested step state is cloned
// consistently with replay code.
func cloneWorkflow(state workflow.State) workflow.State {
	return workflow.CloneState(state)
}

// cloneWorkflowSteps deep-copies per-step result payloads for legacy callers.
func cloneWorkflowSteps(source map[string]workflow.StepState) map[string]workflow.StepState {
	out := make(map[string]workflow.StepState, len(source))
	for id, step := range source {
		step.ResultJSON = append([]byte(nil), step.ResultJSON...)
		out[id] = step
	}
	return out
}

// cloneActor copies actor source and JSON payloads so caller mutation cannot
// affect persisted actor state.
func cloneActor(state actor.State) actor.State {
	state.ClassSource = string([]byte(state.ClassSource))
	state.InitArgsJSON = append([]byte(nil), state.InitArgsJSON...)
	state.StateJSON = append([]byte(nil), state.StateJSON...)
	return state
}

// cloneWorker deep-copies worker maps and normalizes zero capacity to one.
func cloneWorker(worker Worker) Worker {
	labels := make(map[string]string, len(worker.Labels))
	for k, v := range worker.Labels {
		labels[k] = v
	}
	worker.Labels = labels
	worker.CachedModels = cloneModelCache(worker.CachedModels)
	if worker.Capacity == 0 {
		worker.Capacity = 1
	}
	return worker
}

// cloneModelCache copies the worker model-cache set represented as a map.
func cloneModelCache(source map[string]bool) map[string]bool {
	out := make(map[string]bool, len(source))
	for k, v := range source {
		out[k] = v
	}
	return out
}

// cloneModel returns a detached protobuf model record.
func cloneModel(model *logservepb.ModelInfo) *logservepb.ModelInfo {
	if model == nil {
		return nil
	}
	return &logservepb.ModelInfo{
		Name:      model.GetName(),
		Version:   model.GetVersion(),
		SizeBytes: model.GetSizeBytes(),
		Path:      model.GetPath(),
		Adapter:   model.GetAdapter(),
	}
}
