package metadata

import (
	"errors"
	"sync"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/actor"
	"github.com/logserve/logserve/internal/workflow"
)

type Task struct {
	TaskID          string
	TaskName        string
	Status          logservepb.TaskStatus
	ResultJSON      []byte
	Error           string
	WorkerID        string
	WorkflowID      string
	StepID          string
	TargetWorkerID  string
	ActorID         string
	ActorCallID     string
	ActorEpoch      uint64
	TaskLeaseEpoch  uint64
	LLMModelName    string
	LLMModelVersion string
	IdempotencyKey  string
	CreatedAtMs     int64
	UpdatedAtMs     int64
}

type Worker struct {
	WorkerID      string
	Address       string
	Labels        map[string]string
	CachedModels  map[string]bool
	Capacity      uint32
	RunningTasks  uint32
	LastHeartbeat int64
}

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

func NewMemoryStore() *MemoryStore {
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

func (s *MemoryStore) GetTask(taskID string) (Task, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	task, ok := s.tasks[taskID]
	return cloneTask(task), ok
}

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

func (s *MemoryStore) ListTasks() []Task {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Task, 0, len(s.tasks))
	for _, task := range s.tasks {
		out = append(out, cloneTask(task))
	}
	return out
}

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
	if task.WorkerID != "" && workerID != "" && task.WorkerID != workerID {
		return Task{}, errors.New("stale task lease rejected")
	}
	if leaseEpoch > 0 && task.TaskLeaseEpoch != leaseEpoch {
		return Task{}, errors.New("stale task lease rejected")
	}
	return cloneTask(task), nil
}

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
	if task.Status == logservepb.TaskStatus_TASK_STATUS_RUNNING {
		if task.WorkerID != "" && workerID != "" && task.WorkerID != workerID {
			return Task{}, errors.New("stale task lease rejected")
		}
		if leaseEpoch > 0 && task.TaskLeaseEpoch != leaseEpoch {
			return Task{}, errors.New("stale task lease rejected")
		}
	}
	task.Status = status
	task.WorkerID = workerID
	task.ResultJSON = append([]byte(nil), resultJSON...)
	task.Error = taskErr
	task.UpdatedAtMs = time.Now().UnixMilli()
	s.tasks[taskID] = task
	return cloneTask(task), nil
}

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

func (s *MemoryStore) GetModel(name, version string) (*logservepb.ModelInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if version == "" {
		version = "v1"
	}
	model, ok := s.models[ModelKey(name, version)]
	return cloneModel(model), ok
}

func (s *MemoryStore) CreateWorkflow(state workflow.State, idempotencyKey string) (workflow.State, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if idempotencyKey != "" {
		if workflowID, ok := s.workflowByIdemKey[idempotencyKey]; ok {
			return cloneWorkflow(s.workflows[workflowID]), true
		}
	}
	s.workflows[state.WorkflowID] = cloneWorkflow(state)
	if idempotencyKey != "" {
		s.workflowByIdemKey[idempotencyKey] = state.WorkflowID
	}
	return cloneWorkflow(state), false
}

func (s *MemoryStore) GetWorkflow(workflowID string) (workflow.State, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.workflows[workflowID]
	return cloneWorkflow(state), ok
}

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

func (s *MemoryStore) ListWorkflows() []workflow.State {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]workflow.State, 0, len(s.workflows))
	for _, state := range s.workflows {
		out = append(out, cloneWorkflow(state))
	}
	return out
}

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

func (s *MemoryStore) UpsertWorkflow(state workflow.State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workflows[state.WorkflowID] = cloneWorkflow(state)
}

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
	worker.LastHeartbeat = time.Now().UnixMilli()
	s.workers[worker.WorkerID] = cloneWorker(worker)
}

func (s *MemoryStore) GetWorker(workerID string) (Worker, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	worker, ok := s.workers[workerID]
	return cloneWorker(worker), ok
}

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

func (s *MemoryStore) ListWorkers() []Worker {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Worker, 0, len(s.workers))
	for _, worker := range s.workers {
		out = append(out, cloneWorker(worker))
	}
	return out
}

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

func (s *MemoryStore) CreateActor(state actor.State, idempotencyKey string) (actor.State, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if idempotencyKey != "" {
		if actorID, ok := s.actorByIdemKey[idempotencyKey]; ok {
			return cloneActor(s.actors[actorID]), true
		}
	}
	s.actors[state.ActorID] = cloneActor(state)
	if idempotencyKey != "" {
		s.actorByIdemKey[idempotencyKey] = state.ActorID
	}
	return cloneActor(state), false
}

func (s *MemoryStore) GetActor(actorID string) (actor.State, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.actors[actorID]
	return cloneActor(state), ok
}

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

func (s *MemoryStore) ListActors() []actor.State {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]actor.State, 0, len(s.actors))
	for _, state := range s.actors {
		out = append(out, cloneActor(state))
	}
	return out
}

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

func (s *MemoryStore) UpsertActor(state actor.State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actors[state.ActorID] = cloneActor(state)
}

func cloneTask(task Task) Task {
	task.ResultJSON = append([]byte(nil), task.ResultJSON...)
	return task
}

func ModelKey(name, version string) string {
	if version == "" {
		version = "v1"
	}
	return name + ":" + version
}

func (s *MemoryStore) ListModels() []*logservepb.ModelInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*logservepb.ModelInfo, 0, len(s.models))
	for _, model := range s.models {
		out = append(out, cloneModel(model))
	}
	return out
}

func cloneWorkflow(state workflow.State) workflow.State {
	state.ResultJSON = append([]byte(nil), state.ResultJSON...)
	state.StepOrder = append([]string(nil), state.StepOrder...)
	state.Definition.Steps = append([]workflow.StepDefinition(nil), state.Definition.Steps...)
	state.Definition.ArgsJSON = append([]byte(nil), state.Definition.ArgsJSON...)
	state.Definition.FunctionSource = string([]byte(state.Definition.FunctionSource))
	state.Steps = cloneWorkflowSteps(state.Steps)
	return state
}

func cloneWorkflowSteps(source map[string]workflow.StepState) map[string]workflow.StepState {
	out := make(map[string]workflow.StepState, len(source))
	for id, step := range source {
		step.ResultJSON = append([]byte(nil), step.ResultJSON...)
		out[id] = step
	}
	return out
}

func cloneActor(state actor.State) actor.State {
	state.ClassSource = string([]byte(state.ClassSource))
	state.InitArgsJSON = append([]byte(nil), state.InitArgsJSON...)
	state.StateJSON = append([]byte(nil), state.StateJSON...)
	return state
}

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

func cloneModelCache(source map[string]bool) map[string]bool {
	out := make(map[string]bool, len(source))
	for k, v := range source {
		out[k] = v
	}
	return out
}

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
