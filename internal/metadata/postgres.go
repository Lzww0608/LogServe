package metadata

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	_ "github.com/lib/pq"
	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/actor"
	"github.com/logserve/logserve/internal/workflow"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type PostgresStore struct {
	memory *MemoryStore
	db     *sql.DB
	mu     sync.Mutex
	last   error
}

func OpenPostgresStore(ctx context.Context, dsn string) (*PostgresStore, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("postgres dsn is required")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	if err := pingPostgres(ctx, db, 30*time.Second); err != nil {
		_ = db.Close()
		return nil, err
	}
	store := NewPostgresStore(db)
	if err := store.ApplyMigrations(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func pingPostgres(ctx context.Context, db *sql.DB, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for {
		if err := db.PingContext(ctx); err == nil {
			return nil
		} else {
			last = err
		}
		if time.Now().After(deadline) {
			return last
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func NewPostgresStore(db *sql.DB) *PostgresStore {
	return &PostgresStore{memory: NewMemoryStore(), db: db}
}

func (s *PostgresStore) ApplyMigrations(ctx context.Context) error {
	data, err := migrationsFS.ReadFile("migrations/001_init.sql")
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, string(data))
	return err
}

func (s *PostgresStore) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *PostgresStore) LastError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *PostgresStore) remember(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = err
}

func (s *PostgresStore) CreateTask(task Task, idempotencyKey string) (Task, bool) {
	created, duplicate := s.memory.CreateTask(task, idempotencyKey)
	if !duplicate {
		s.remember(s.persistTask(context.Background(), created))
	}
	return created, duplicate
}

func (s *PostgresStore) GetTask(taskID string) (Task, bool) {
	return s.memory.GetTask(taskID)
}

func (s *PostgresStore) GetTaskByIdempotencyKey(idempotencyKey string) (Task, bool) {
	return s.memory.GetTaskByIdempotencyKey(idempotencyKey)
}

func (s *PostgresStore) ListTasks() []Task {
	return s.memory.ListTasks()
}

func (s *PostgresStore) LeaseTask(taskID, workerID string) (Task, error) {
	task, err := s.memory.LeaseTask(taskID, workerID)
	if err == nil {
		s.remember(s.persistTask(context.Background(), task))
	}
	return task, err
}

func (s *PostgresStore) ValidateTaskLease(taskID, workerID string, leaseEpoch uint64) (Task, error) {
	return s.memory.ValidateTaskLease(taskID, workerID, leaseEpoch)
}

func (s *PostgresStore) RequeueExpiredRunningTasks(maxAge time.Duration) []Task {
	tasks := s.memory.RequeueExpiredRunningTasks(maxAge)
	for _, task := range tasks {
		s.remember(s.persistTask(context.Background(), task))
	}
	return tasks
}

func (s *PostgresStore) CompleteTask(taskID, workerID string, leaseEpoch uint64, status logservepb.TaskStatus, resultJSON []byte, taskErr string) (Task, error) {
	task, err := s.memory.CompleteTask(taskID, workerID, leaseEpoch, status, resultJSON, taskErr)
	if err == nil {
		s.remember(s.persistTask(context.Background(), task))
	}
	return task, err
}

func (s *PostgresStore) RegisterModel(model *logservepb.ModelInfo) *logservepb.ModelInfo {
	registered := s.memory.RegisterModel(model)
	s.remember(s.persistModel(context.Background(), registered))
	return registered
}

func (s *PostgresStore) GetModel(name, version string) (*logservepb.ModelInfo, bool) {
	return s.memory.GetModel(name, version)
}

func (s *PostgresStore) ListModels() []*logservepb.ModelInfo {
	return s.memory.ListModels()
}

func (s *PostgresStore) CreateWorkflow(state workflow.State, idempotencyKey string) (workflow.State, bool) {
	created, duplicate := s.memory.CreateWorkflow(state, idempotencyKey)
	if !duplicate {
		s.remember(s.persistWorkflow(context.Background(), created))
	}
	return created, duplicate
}

func (s *PostgresStore) GetWorkflow(workflowID string) (workflow.State, bool) {
	return s.memory.GetWorkflow(workflowID)
}

func (s *PostgresStore) GetWorkflowByIdempotencyKey(idempotencyKey string) (workflow.State, bool) {
	return s.memory.GetWorkflowByIdempotencyKey(idempotencyKey)
}

func (s *PostgresStore) ListWorkflows() []workflow.State {
	return s.memory.ListWorkflows()
}

func (s *PostgresStore) UpdateWorkflow(workflowID string, fn func(*workflow.State) error) (workflow.State, error) {
	state, err := s.memory.UpdateWorkflow(workflowID, fn)
	if err == nil {
		s.remember(s.persistWorkflow(context.Background(), state))
	}
	return state, err
}

func (s *PostgresStore) UpsertWorkflow(state workflow.State) {
	s.memory.UpsertWorkflow(state)
	s.remember(s.persistWorkflow(context.Background(), state))
}

func (s *PostgresStore) UpsertWorker(worker Worker) {
	s.memory.UpsertWorker(worker)
	current, _ := s.memory.GetWorker(worker.WorkerID)
	s.remember(s.persistWorker(context.Background(), current))
}

func (s *PostgresStore) GetWorker(workerID string) (Worker, bool) {
	return s.memory.GetWorker(workerID)
}

func (s *PostgresStore) ActiveWorkers(maxAge time.Duration) []Worker {
	return s.memory.ActiveWorkers(maxAge)
}

func (s *PostgresStore) ListWorkers() []Worker {
	return s.memory.ListWorkers()
}

func (s *PostgresStore) Heartbeat(workerID string, cachedModels map[string]bool) (Worker, bool) {
	worker, existed := s.memory.Heartbeat(workerID, cachedModels)
	s.remember(s.persistWorker(context.Background(), worker))
	return worker, existed
}

func (s *PostgresStore) IncrementWorkerLoad(workerID string) {
	s.memory.IncrementWorkerLoad(workerID)
	if worker, ok := s.memory.GetWorker(workerID); ok {
		s.remember(s.persistWorker(context.Background(), worker))
	}
}

func (s *PostgresStore) DecrementWorkerLoad(workerID string) {
	s.memory.DecrementWorkerLoad(workerID)
	if worker, ok := s.memory.GetWorker(workerID); ok {
		s.remember(s.persistWorker(context.Background(), worker))
	}
}

func (s *PostgresStore) CreateActor(state actor.State, idempotencyKey string) (actor.State, bool) {
	created, duplicate := s.memory.CreateActor(state, idempotencyKey)
	if !duplicate {
		s.remember(s.persistActor(context.Background(), created))
	}
	return created, duplicate
}

func (s *PostgresStore) GetActor(actorID string) (actor.State, bool) {
	return s.memory.GetActor(actorID)
}

func (s *PostgresStore) GetActorByIdempotencyKey(idempotencyKey string) (actor.State, bool) {
	return s.memory.GetActorByIdempotencyKey(idempotencyKey)
}

func (s *PostgresStore) ListActors() []actor.State {
	return s.memory.ListActors()
}

func (s *PostgresStore) UpdateActor(actorID string, fn func(*actor.State) error) (actor.State, error) {
	state, err := s.memory.UpdateActor(actorID, fn)
	if err == nil {
		s.remember(s.persistActor(context.Background(), state))
	}
	return state, err
}

func (s *PostgresStore) UpsertActor(state actor.State) {
	s.memory.UpsertActor(state)
	s.remember(s.persistActor(context.Background(), state))
}

func (s *PostgresStore) persistTask(ctx context.Context, task Task) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO task_instances (
  task_id, task_name, status, worker_id, workflow_id, step_id, target_worker_id,
  actor_id, actor_call_id, actor_epoch, task_lease_epoch, llm_model_name,
  llm_model_version, idempotency_key, idempotency_fingerprint, result_json, error, created_at, updated_at
) VALUES (
  $1, $2, $3, $4, $5, $6, $7,
  $8, $9, $10, $11, $12,
  $13, $14, $15, $16::jsonb, $17, $18, $19
) ON CONFLICT (task_id) DO UPDATE SET
  task_name = EXCLUDED.task_name,
  status = EXCLUDED.status,
  worker_id = EXCLUDED.worker_id,
  workflow_id = EXCLUDED.workflow_id,
  step_id = EXCLUDED.step_id,
  target_worker_id = EXCLUDED.target_worker_id,
  actor_id = EXCLUDED.actor_id,
  actor_call_id = EXCLUDED.actor_call_id,
  actor_epoch = EXCLUDED.actor_epoch,
  task_lease_epoch = EXCLUDED.task_lease_epoch,
  llm_model_name = EXCLUDED.llm_model_name,
  llm_model_version = EXCLUDED.llm_model_version,
  idempotency_key = EXCLUDED.idempotency_key,
  idempotency_fingerprint = EXCLUDED.idempotency_fingerprint,
  result_json = EXCLUDED.result_json,
  error = EXCLUDED.error,
  updated_at = EXCLUDED.updated_at`,
		task.TaskID,
		task.TaskName,
		task.Status.String(),
		nullString(task.WorkerID),
		nullString(task.WorkflowID),
		nullString(task.StepID),
		nullString(task.TargetWorkerID),
		nullString(task.ActorID),
		nullString(task.ActorCallID),
		task.ActorEpoch,
		task.TaskLeaseEpoch,
		nullString(task.LLMModelName),
		nullString(task.LLMModelVersion),
		nullString(task.IdempotencyKey),
		nullString(task.IdempotencyFingerprint),
		jsonValue(task.ResultJSON),
		nullString(task.Error),
		msTime(task.CreatedAtMs),
		msTime(task.UpdatedAtMs),
	)
	if err != nil {
		return err
	}
	if task.LLMModelName != "" {
		_, err = s.db.ExecContext(ctx, `
INSERT INTO llm_requests (task_id, model_name, model_version, worker_id)
VALUES ($1, $2, $3, $4)
ON CONFLICT (task_id) DO UPDATE SET
  model_name = EXCLUDED.model_name,
  model_version = EXCLUDED.model_version,
  worker_id = EXCLUDED.worker_id`,
			task.TaskID,
			task.LLMModelName,
			firstNonEmpty(task.LLMModelVersion, "v1"),
			nullString(task.WorkerID),
		)
	}
	return err
}

func (s *PostgresStore) persistModel(ctx context.Context, model *logservepb.ModelInfo) error {
	if model == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO model_registry (name, version, size_bytes, path, adapter, updated_at)
VALUES ($1, $2, $3, $4, $5, now())
ON CONFLICT (name, version) DO UPDATE SET
  size_bytes = EXCLUDED.size_bytes,
  path = EXCLUDED.path,
  adapter = EXCLUDED.adapter,
  updated_at = now()`,
		model.GetName(),
		firstNonEmpty(model.GetVersion(), "v1"),
		model.GetSizeBytes(),
		nullString(model.GetPath()),
		firstNonEmpty(model.GetAdapter(), "mock"),
	)
	return err
}

func (s *PostgresStore) persistWorkflow(ctx context.Context, state workflow.State) error {
	definitionJSON, err := json.Marshal(state.Definition)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO workflow_instances (
  workflow_id, workflow_name, status, input_json, definition_json, output_json,
  output_ref, error, idempotency_key, idempotency_fingerprint, created_at, updated_at, completed_at
) VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, $6::jsonb, $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT (workflow_id) DO UPDATE SET
  workflow_name = EXCLUDED.workflow_name,
  status = EXCLUDED.status,
  input_json = EXCLUDED.input_json,
  definition_json = EXCLUDED.definition_json,
  output_json = EXCLUDED.output_json,
  output_ref = EXCLUDED.output_ref,
  error = EXCLUDED.error,
  idempotency_key = EXCLUDED.idempotency_key,
  idempotency_fingerprint = EXCLUDED.idempotency_fingerprint,
  updated_at = EXCLUDED.updated_at,
  completed_at = EXCLUDED.completed_at`,
		state.WorkflowID,
		nullString(state.WorkflowName),
		state.Status.String(),
		jsonValue(state.Definition.ArgsJSON),
		string(definitionJSON),
		jsonValue(state.ResultJSON),
		nullString(state.ResultRef),
		nullString(state.Error),
		nullString(state.IdempotencyKey),
		nullString(state.IdempotencyFingerprint),
		msTime(state.CreatedAtMs),
		msTime(state.UpdatedAtMs),
		nullTime(state.CompletedAtMs),
	); err != nil {
		return err
	}

	stepIDs := append([]string(nil), state.StepOrder...)
	if len(stepIDs) == 0 {
		for stepID := range state.Steps {
			stepIDs = append(stepIDs, stepID)
		}
		sort.Strings(stepIDs)
	}
	for _, stepID := range stepIDs {
		step := state.Steps[stepID]
		if _, err := tx.ExecContext(ctx, `
INSERT INTO workflow_steps (
  workflow_id, step_id, task_name, status, attempts, task_id, result_json,
  result_ref, error, started_at, completed_at, latency_ms
) VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9, $10, $11, $12)
ON CONFLICT (workflow_id, step_id) DO UPDATE SET
  task_name = EXCLUDED.task_name,
  status = EXCLUDED.status,
  attempts = EXCLUDED.attempts,
  task_id = EXCLUDED.task_id,
  result_json = EXCLUDED.result_json,
  result_ref = EXCLUDED.result_ref,
  error = EXCLUDED.error,
  started_at = EXCLUDED.started_at,
  completed_at = EXCLUDED.completed_at,
  latency_ms = EXCLUDED.latency_ms`,
			state.WorkflowID,
			step.StepID,
			step.TaskName,
			step.Status.String(),
			step.Attempts,
			nullString(step.TaskID),
			jsonValue(step.ResultJSON),
			nullString(step.ResultRef),
			nullString(step.Error),
			nullTime(step.StartedAtMs),
			nullTime(step.CompletedAtMs),
			step.LatencyMs,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *PostgresStore) persistWorker(ctx context.Context, worker Worker) error {
	labelsJSON, err := json.Marshal(worker.Labels)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO workers (worker_id, address, labels, capacity, running_tasks, last_heartbeat_at)
VALUES ($1, $2, $3::jsonb, $4, $5, $6)
ON CONFLICT (worker_id) DO UPDATE SET
  address = EXCLUDED.address,
  labels = EXCLUDED.labels,
  capacity = EXCLUDED.capacity,
  running_tasks = EXCLUDED.running_tasks,
  last_heartbeat_at = EXCLUDED.last_heartbeat_at`,
		worker.WorkerID,
		nullString(worker.Address),
		string(labelsJSON),
		worker.Capacity,
		worker.RunningTasks,
		msTime(worker.LastHeartbeat),
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM worker_model_cache WHERE worker_id = $1`, worker.WorkerID); err != nil {
		return err
	}
	keys := make([]string, 0, len(worker.CachedModels))
	for key, cached := range worker.CachedModels {
		if cached {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		name, version := splitModelKey(key)
		if _, err := tx.ExecContext(ctx, `
INSERT INTO worker_model_cache (worker_id, model_name, model_version, updated_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (worker_id, model_name, model_version) DO UPDATE SET updated_at = now()`,
			worker.WorkerID,
			name,
			version,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *PostgresStore) persistActor(ctx context.Context, state actor.State) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO actor_instances (
  actor_id, class_name, class_source, status, owner_worker_id, epoch,
  command_count, snapshot_ref, snapshot_command_count, state_json,
  init_args_json, snapshot_every, idempotency_key, idempotency_fingerprint, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb, $11::jsonb, $12, $13, $14, $15, $16)
ON CONFLICT (actor_id) DO UPDATE SET
  class_name = EXCLUDED.class_name,
  class_source = EXCLUDED.class_source,
  status = EXCLUDED.status,
  owner_worker_id = EXCLUDED.owner_worker_id,
  epoch = EXCLUDED.epoch,
  command_count = EXCLUDED.command_count,
  snapshot_ref = EXCLUDED.snapshot_ref,
  snapshot_command_count = EXCLUDED.snapshot_command_count,
  state_json = EXCLUDED.state_json,
  init_args_json = EXCLUDED.init_args_json,
  snapshot_every = EXCLUDED.snapshot_every,
  idempotency_key = EXCLUDED.idempotency_key,
  idempotency_fingerprint = EXCLUDED.idempotency_fingerprint,
  updated_at = EXCLUDED.updated_at`,
		state.ActorID,
		state.ClassName,
		nullString(state.ClassSource),
		state.Status.String(),
		nullString(state.OwnerWorkerID),
		state.Epoch,
		state.CommandCount,
		nullString(state.SnapshotRef),
		state.SnapshotCommandCount,
		jsonValue(state.StateJSON),
		jsonValue(state.InitArgsJSON),
		state.SnapshotEvery,
		nullString(state.IdempotencyKey),
		nullString(state.IdempotencyFingerprint),
		msTime(state.CreatedAtMs),
		msTime(state.UpdatedAtMs),
	)
	return err
}

func jsonValue(data []byte) any {
	if len(data) == 0 {
		return nil
	}
	return string(data)
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullTime(ms int64) any {
	if ms <= 0 {
		return nil
	}
	return time.UnixMilli(ms)
}

func msTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Now()
	}
	return time.UnixMilli(ms)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func splitModelKey(key string) (string, string) {
	name, version, ok := strings.Cut(key, ":")
	if !ok {
		return key, "v1"
	}
	return name, version
}
