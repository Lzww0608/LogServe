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
	"sync/atomic"
	"time"

	_ "github.com/lib/pq"
	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/actor"
	"github.com/logserve/logserve/internal/workflow"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type PostgresStore struct {
	memory       Store
	db           *sql.DB
	mode         PostgresWriteMode
	materializer *Materializer
	deltaSeq     atomic.Int64
	mu           sync.Mutex
	last         error
}

type PostgresWriteMode string

const (
	PostgresWriteModeSync  PostgresWriteMode = "sync"
	PostgresWriteModeAsync PostgresWriteMode = "async"
)

type PostgresOptions struct {
	Mode          PostgresWriteMode
	BatchMax      int
	FlushInterval time.Duration
	QueueSize     int
}

func OpenPostgresStore(ctx context.Context, dsn string) (*PostgresStore, error) {
	return OpenPostgresStoreWithOptions(ctx, dsn, PostgresOptions{})
}

func OpenPostgresStoreWithOptions(ctx context.Context, dsn string, opts PostgresOptions) (*PostgresStore, error) {
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
	store := NewPostgresStoreWithOptions(db, opts)
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
	return NewPostgresStoreWithOptions(db, PostgresOptions{})
}

func NewPostgresStoreWithOptions(db *sql.DB, opts PostgresOptions) *PostgresStore {
	opts = normalizePostgresOptions(opts)
	store := &PostgresStore{memory: NewMemoryStore(), db: db, mode: opts.Mode}
	if opts.Mode == PostgresWriteModeAsync {
		store.materializer = NewMaterializer(db, opts.BatchMax, opts.FlushInterval, opts.QueueSize, store.persistDeltas, store.remember)
		store.materializer.Start()
	}
	return store
}

func normalizePostgresOptions(opts PostgresOptions) PostgresOptions {
	switch PostgresWriteMode(strings.ToLower(strings.TrimSpace(string(opts.Mode)))) {
	case PostgresWriteModeAsync:
		opts.Mode = PostgresWriteModeAsync
	default:
		opts.Mode = PostgresWriteModeSync
	}
	if opts.BatchMax <= 0 {
		opts.BatchMax = 256
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = time.Second
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = opts.BatchMax * 4
	}
	return opts
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
	var firstErr error
	if s.materializer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := s.materializer.Close(ctx); err != nil {
			firstErr = err
		}
		cancel()
	}
	if s.db == nil {
		return firstErr
	}
	if err := s.db.Close(); firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func (s *PostgresStore) LastError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *PostgresStore) NonBlockingPersistence() bool {
	return s != nil && s.mode == PostgresWriteModeAsync
}

func (s *PostgresStore) MaterializerStats() MaterializerStats {
	mode := string(PostgresWriteModeSync)
	if s != nil && s.mode != "" {
		mode = string(s.mode)
	}
	if s == nil || s.materializer == nil {
		return MaterializerStats{Mode: mode}
	}
	return s.materializer.Stats(mode)
}

func (s *PostgresStore) Flush(ctx context.Context) error {
	if s == nil || s.materializer == nil {
		return nil
	}
	return s.materializer.Flush(ctx)
}

func (s *PostgresStore) remember(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = err
}

func (s *PostgresStore) recordPersist(err error) {
	if err != nil || !s.NonBlockingPersistence() {
		s.remember(err)
	}
}
func (s *PostgresStore) CreateTask(task Task, idempotencyKey string) (Task, bool) {
	created, duplicate := s.memory.CreateTask(task, idempotencyKey)
	if !duplicate {
		s.recordPersist(s.persistTaskOrEnqueue(created))
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
	if err != nil {
		return task, err
	}
	if persistErr := s.persistTaskOrEnqueue(task); persistErr != nil {
		s.recordPersist(persistErr)
		if !s.NonBlockingPersistence() {
			return task, persistErr
		}
	}
	s.recordPersist(nil)
	return task, nil
}
func (s *PostgresStore) ValidateTaskLease(taskID, workerID string, leaseEpoch uint64) (Task, error) {
	return s.memory.ValidateTaskLease(taskID, workerID, leaseEpoch)
}

func (s *PostgresStore) RequeueExpiredRunningTasks(maxAge time.Duration) []Task {
	tasks := s.memory.RequeueExpiredRunningTasks(maxAge)
	for _, task := range tasks {
		s.recordPersist(s.persistTaskOrEnqueue(task))
	}
	return tasks
}

func (s *PostgresStore) RequeueTaskIfLeaseExpired(taskID string, leaseEpoch uint64, maxAge time.Duration) (Task, bool) {
	task, requeued := s.memory.RequeueTaskIfLeaseExpired(taskID, leaseEpoch, maxAge)
	if requeued {
		s.recordPersist(s.persistTaskOrEnqueue(task))
	}
	return task, requeued
}

func (s *PostgresStore) CompleteTask(taskID, workerID string, leaseEpoch uint64, status logservepb.TaskStatus, resultJSON []byte, taskErr string) (Task, error) {
	task, err := s.memory.CompleteTask(taskID, workerID, leaseEpoch, status, resultJSON, taskErr)
	if err != nil {
		return task, err
	}
	if persistErr := s.persistTaskOrEnqueue(task); persistErr != nil {
		s.recordPersist(persistErr)
		if !s.NonBlockingPersistence() {
			return task, persistErr
		}
	}
	s.recordPersist(nil)
	return task, nil
}
func (s *PostgresStore) RegisterModel(model *logservepb.ModelInfo) *logservepb.ModelInfo {
	registered := s.memory.RegisterModel(model)
	s.recordPersist(s.persistModelOrEnqueue(registered))
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
		s.recordPersist(s.persistWorkflowOrEnqueue(created))
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
	if err != nil {
		return state, err
	}
	if persistErr := s.persistWorkflowOrEnqueue(state); persistErr != nil {
		s.recordPersist(persistErr)
		if !s.NonBlockingPersistence() {
			return state, persistErr
		}
	}
	s.recordPersist(nil)
	return state, nil
}
func (s *PostgresStore) UpsertWorkflow(state workflow.State) {
	s.memory.UpsertWorkflow(state)
	s.recordPersist(s.persistWorkflowOrEnqueue(state))
}

func (s *PostgresStore) UpsertWorker(worker Worker) {
	s.memory.UpsertWorker(worker)
	current, _ := s.memory.GetWorker(worker.WorkerID)
	s.recordPersist(s.persistWorkerOrEnqueue(current))
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
	s.recordPersist(s.persistWorkerOrEnqueue(worker))
	return worker, existed
}

func (s *PostgresStore) IncrementWorkerLoad(workerID string) {
	s.memory.IncrementWorkerLoad(workerID)
	if worker, ok := s.memory.GetWorker(workerID); ok {
		s.recordPersist(s.persistWorkerOrEnqueue(worker))
	}
}

func (s *PostgresStore) DecrementWorkerLoad(workerID string) {
	s.memory.DecrementWorkerLoad(workerID)
	if worker, ok := s.memory.GetWorker(workerID); ok {
		s.recordPersist(s.persistWorkerOrEnqueue(worker))
	}
}

func (s *PostgresStore) CreateActor(state actor.State, idempotencyKey string) (actor.State, bool) {
	created, duplicate := s.memory.CreateActor(state, idempotencyKey)
	if !duplicate {
		s.recordPersist(s.persistActorOrEnqueue(created))
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
	if err != nil {
		return state, err
	}
	if persistErr := s.persistActorOrEnqueue(state); persistErr != nil {
		s.recordPersist(persistErr)
		if !s.NonBlockingPersistence() {
			return state, persistErr
		}
	}
	s.recordPersist(nil)
	return state, nil
}
func (s *PostgresStore) UpsertActor(state actor.State) {
	s.memory.UpsertActor(state)
	s.recordPersist(s.persistActorOrEnqueue(state))
}

type sqlExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (s *PostgresStore) persistTaskOrEnqueue(task Task) error {
	if !s.NonBlockingPersistence() {
		return s.persistTask(context.Background(), task)
	}
	return s.enqueueDelta(DeltaTask, task.TaskID, cloneTask(task))
}

func (s *PostgresStore) persistModelOrEnqueue(model *logservepb.ModelInfo) error {
	if !s.NonBlockingPersistence() {
		return s.persistModel(context.Background(), model)
	}
	return s.enqueueDelta(DeltaModel, ModelKey(model.GetName(), model.GetVersion()), cloneModel(model))
}

func (s *PostgresStore) persistWorkflowOrEnqueue(state workflow.State) error {
	if !s.NonBlockingPersistence() {
		return s.persistWorkflow(context.Background(), state)
	}
	return s.enqueueDelta(DeltaWorkflow, state.WorkflowID, cloneWorkflow(state))
}

func (s *PostgresStore) persistWorkerOrEnqueue(worker Worker) error {
	if !s.NonBlockingPersistence() {
		return s.persistWorker(context.Background(), worker)
	}
	return s.enqueueDelta(DeltaWorker, worker.WorkerID, cloneWorker(worker))
}

func (s *PostgresStore) persistActorOrEnqueue(state actor.State) error {
	if !s.NonBlockingPersistence() {
		return s.persistActor(context.Background(), state)
	}
	return s.enqueueDelta(DeltaActor, state.ActorID, cloneActor(state))
}

func (s *PostgresStore) enqueueDelta(kind DeltaKind, key string, payload any) error {
	if s.materializer == nil {
		return errors.New("metadata materializer is not configured")
	}
	return s.materializer.Enqueue(metadataDelta{
		kind:    kind,
		key:     key,
		payload: payload,
		version: s.deltaSeq.Add(1),
	})
}

func (s *PostgresStore) persistDeltas(ctx context.Context, deltas []metadataDelta) error {
	if len(deltas) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, delta := range deltas {
		if err := s.persistDeltaWith(ctx, tx, delta); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *PostgresStore) persistDeltaWith(ctx context.Context, exec sqlExecutor, delta metadataDelta) error {
	switch delta.kind {
	case DeltaTask:
		task, ok := delta.payload.(Task)
		if !ok {
			return errors.New("invalid task delta payload")
		}
		return s.persistTaskWith(ctx, exec, task)
	case DeltaWorker:
		worker, ok := delta.payload.(Worker)
		if !ok {
			return errors.New("invalid worker delta payload")
		}
		return s.persistWorkerWith(ctx, exec, worker)
	case DeltaModel:
		model, ok := delta.payload.(*logservepb.ModelInfo)
		if !ok {
			return errors.New("invalid model delta payload")
		}
		return s.persistModelWith(ctx, exec, model)
	case DeltaWorkflow:
		state, ok := delta.payload.(workflow.State)
		if !ok {
			return errors.New("invalid workflow delta payload")
		}
		return s.persistWorkflowWith(ctx, exec, state)
	case DeltaActor:
		state, ok := delta.payload.(actor.State)
		if !ok {
			return errors.New("invalid actor delta payload")
		}
		return s.persistActorWith(ctx, exec, state)
	default:
		return errors.New("unknown metadata delta kind")
	}
}
func (s *PostgresStore) persistTask(ctx context.Context, task Task) error {
	return s.persistTaskWith(ctx, s.db, task)
}

func (s *PostgresStore) persistTaskWith(ctx context.Context, exec sqlExecutor, task Task) error {
	_, err := exec.ExecContext(ctx, `
INSERT INTO task_instances (
  task_id, task_name, status, worker_id, workflow_id, step_id, target_worker_id,
  actor_id, actor_call_id, actor_epoch, actor_command_seq, task_lease_epoch, llm_model_name,
  llm_model_version, idempotency_key, idempotency_fingerprint, result_json, error, created_at, updated_at
) VALUES (
  $1, $2, $3, $4, $5, $6, $7,
  $8, $9, $10, $11, $12, $13,
  $14, $15, $16, $17::jsonb, $18, $19, $20
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
  actor_command_seq = EXCLUDED.actor_command_seq,
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
		task.ActorCommandSeq,
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
		_, err = exec.ExecContext(ctx, `
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
	return s.persistModelWith(ctx, s.db, model)
}

func (s *PostgresStore) persistModelWith(ctx context.Context, exec sqlExecutor, model *logservepb.ModelInfo) error {
	if model == nil {
		return nil
	}
	_, err := exec.ExecContext(ctx, `
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.persistWorkflowWith(ctx, tx, state); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) persistWorkflowWith(ctx context.Context, exec sqlExecutor, state workflow.State) error {
	definitionJSON, err := json.Marshal(state.Definition)
	if err != nil {
		return err
	}
	if _, err := exec.ExecContext(ctx, `
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

	stepStates := state.StepStatesInOrder()
	for _, step := range stepStates {
		if _, err := exec.ExecContext(ctx, `
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
	return nil
}

func (s *PostgresStore) persistWorker(ctx context.Context, worker Worker) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.persistWorkerWith(ctx, tx, worker); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) persistWorkerWith(ctx context.Context, exec sqlExecutor, worker Worker) error {
	labelsJSON, err := json.Marshal(worker.Labels)
	if err != nil {
		return err
	}
	if _, err := exec.ExecContext(ctx, `
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
	if _, err := exec.ExecContext(ctx, `DELETE FROM worker_model_cache WHERE worker_id = $1`, worker.WorkerID); err != nil {
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
		if _, err := exec.ExecContext(ctx, `
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
	return nil
}

func (s *PostgresStore) persistActor(ctx context.Context, state actor.State) error {
	return s.persistActorWith(ctx, s.db, state)
}

func (s *PostgresStore) persistActorWith(ctx context.Context, exec sqlExecutor, state actor.State) error {
	_, err := exec.ExecContext(ctx, `
INSERT INTO actor_instances (
  actor_id, class_name, class_source, status, owner_worker_id, epoch,
  command_count, submitted_command_count, snapshot_ref, snapshot_command_count, state_json,
  init_args_json, snapshot_every, idempotency_key, idempotency_fingerprint, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb, $12::jsonb, $13, $14, $15, $16, $17)
ON CONFLICT (actor_id) DO UPDATE SET
  class_name = EXCLUDED.class_name,
  class_source = EXCLUDED.class_source,
  status = EXCLUDED.status,
  owner_worker_id = EXCLUDED.owner_worker_id,
  epoch = EXCLUDED.epoch,
  command_count = EXCLUDED.command_count,
  submitted_command_count = EXCLUDED.submitted_command_count,
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
		state.SubmittedCommandCount,
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
