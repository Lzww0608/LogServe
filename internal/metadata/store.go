package metadata

import (
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/actor"
	"github.com/logserve/logserve/internal/workflow"
)

type Store interface {
	CreateTask(task Task, idempotencyKey string) (Task, bool)
	GetTask(taskID string) (Task, bool)
	GetTaskByIdempotencyKey(idempotencyKey string) (Task, bool)
	ListTasks() []Task
	LeaseTask(taskID, workerID string) (Task, error)
	ValidateTaskLease(taskID, workerID string, leaseEpoch uint64) (Task, error)
	RequeueExpiredRunningTasks(maxAge time.Duration) []Task
	CompleteTask(taskID, workerID string, leaseEpoch uint64, status logservepb.TaskStatus, resultJSON []byte, taskErr string) (Task, error)

	RegisterModel(model *logservepb.ModelInfo) *logservepb.ModelInfo
	GetModel(name, version string) (*logservepb.ModelInfo, bool)
	ListModels() []*logservepb.ModelInfo

	CreateWorkflow(state workflow.State, idempotencyKey string) (workflow.State, bool)
	GetWorkflow(workflowID string) (workflow.State, bool)
	GetWorkflowByIdempotencyKey(idempotencyKey string) (workflow.State, bool)
	ListWorkflows() []workflow.State
	UpdateWorkflow(workflowID string, fn func(*workflow.State) error) (workflow.State, error)
	UpsertWorkflow(state workflow.State)

	UpsertWorker(worker Worker)
	GetWorker(workerID string) (Worker, bool)
	ActiveWorkers(maxAge time.Duration) []Worker
	ListWorkers() []Worker
	Heartbeat(workerID string, cachedModels map[string]bool) (Worker, bool)
	IncrementWorkerLoad(workerID string)
	DecrementWorkerLoad(workerID string)

	CreateActor(state actor.State, idempotencyKey string) (actor.State, bool)
	GetActor(actorID string) (actor.State, bool)
	GetActorByIdempotencyKey(idempotencyKey string) (actor.State, bool)
	ListActors() []actor.State
	UpdateActor(actorID string, fn func(*actor.State) error) (actor.State, error)
	UpsertActor(state actor.State)
}

var _ Store = (*MemoryStore)(nil)
