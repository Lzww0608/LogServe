// Package metadata defines the control-plane state boundary for tasks, workers,
// workflows, actors, and model registry entries. Implementations keep an
// in-memory scheduling view and may additionally mirror that view to durable
// storage such as Postgres.
package metadata

// This file defines the metadata Store contract shared by the control plane,
// workers, web API, and durable metadata backends.

import (
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/actor"
	"github.com/logserve/logserve/internal/workflow"
)

// Store is the in-process metadata boundary for task scheduling, worker
// discovery, workflow state, actor state, and model registration. Implementations
// should return defensive copies for mutable fields so callers cannot corrupt
// store-owned state. Methods expose a synchronous state contract even when a
// durable implementation later mirrors the accepted snapshot asynchronously.
type Store interface {
	// CreateTask inserts a scheduler task and returns duplicate=true when the
	// idempotency key already points at an existing task.
	CreateTask(task Task, idempotencyKey string) (Task, bool)
	// GetTask returns the current task snapshot for taskID.
	GetTask(taskID string) (Task, bool)
	// GetTaskByIdempotencyKey resolves a task idempotency key to its original
	// task snapshot.
	GetTaskByIdempotencyKey(idempotencyKey string) (Task, bool)
	// ListTasks returns a point-in-time snapshot of all tasks.
	ListTasks() []Task
	// LeaseTask marks a task RUNNING for workerID and advances its lease epoch.
	LeaseTask(taskID, workerID string) (Task, error)
	// ValidateTaskLease checks that workerID still owns leaseEpoch for taskID.
	ValidateTaskLease(taskID, workerID string, leaseEpoch uint64) (Task, error)
	// RequeueExpiredRunningTasks moves expired RUNNING leases back to QUEUED.
	RequeueExpiredRunningTasks(maxAge time.Duration) []Task
	// RequeueTaskIfLeaseExpired performs the same recovery for one expected lease.
	RequeueTaskIfLeaseExpired(taskID string, leaseEpoch uint64, maxAge time.Duration) (Task, bool)
	// CompleteTask stores a terminal result after validating the worker lease.
	CompleteTask(taskID, workerID string, leaseEpoch uint64, status logservepb.TaskStatus, resultJSON []byte, taskErr string) (Task, error)

	// RegisterModel stores model registry metadata and returns the normalized copy.
	RegisterModel(model *logservepb.ModelInfo) *logservepb.ModelInfo
	// GetModel returns model metadata by name/version, including default version handling.
	GetModel(name, version string) (*logservepb.ModelInfo, bool)
	// ListModels returns a snapshot of every registered model.
	ListModels() []*logservepb.ModelInfo

	// CreateWorkflow stores workflow state and honors workflow idempotency.
	CreateWorkflow(state workflow.State, idempotencyKey string) (workflow.State, bool)
	// GetWorkflow returns a workflow state snapshot by ID.
	GetWorkflow(workflowID string) (workflow.State, bool)
	// GetWorkflowByIdempotencyKey resolves a workflow idempotency key.
	GetWorkflowByIdempotencyKey(idempotencyKey string) (workflow.State, bool)
	// ListWorkflows returns all workflow state snapshots.
	ListWorkflows() []workflow.State
	// UpdateWorkflow mutates one workflow through a callback and commits only when
	// the callback returns nil.
	UpdateWorkflow(workflowID string, fn func(*workflow.State) error) (workflow.State, error)
	// UpsertWorkflow installs a replayed or externally materialized workflow state.
	UpsertWorkflow(state workflow.State)

	// UpsertWorker registers worker metadata without resetting scheduler-owned load.
	UpsertWorker(worker Worker)
	// GetWorker returns a worker snapshot by ID.
	GetWorker(workerID string) (Worker, bool)
	// ActiveWorkers returns heartbeat-fresh workers for scheduling.
	ActiveWorkers(maxAge time.Duration) []Worker
	// ListWorkers returns all worker snapshots.
	ListWorkers() []Worker
	// Heartbeat refreshes worker liveness and optionally cached model state.
	Heartbeat(workerID string, cachedModels map[string]bool) (Worker, bool)
	// IncrementWorkerLoad increases the scheduler-visible running task count.
	IncrementWorkerLoad(workerID string)
	// DecrementWorkerLoad decreases the running task count without underflow.
	DecrementWorkerLoad(workerID string)

	// CreateActor stores actor state and honors actor idempotency.
	CreateActor(state actor.State, idempotencyKey string) (actor.State, bool)
	// GetActor returns an actor state snapshot by ID.
	GetActor(actorID string) (actor.State, bool)
	// GetActorByIdempotencyKey resolves an actor idempotency key.
	GetActorByIdempotencyKey(idempotencyKey string) (actor.State, bool)
	// ListActors returns all actor state snapshots.
	ListActors() []actor.State
	// UpdateActor mutates one actor through a callback and commits only when the
	// callback returns nil.
	UpdateActor(actorID string, fn func(*actor.State) error) (actor.State, error)
	// UpsertActor installs a replayed or externally materialized actor state.
	UpsertActor(state actor.State)
}

// MemoryStore implements Store through the legacy single-lock implementation.
var _ Store = (*MemoryStore)(nil)

// MemoryStoreV2 implements Store through the default sharded implementation.
var _ Store = (*MemoryStoreV2)(nil)
