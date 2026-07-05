package metadata

// This file adapts the in-memory metadata Store to PostgreSQL. Mutations update
// memory first, then persist either synchronously or through the async materializer.

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

// migrationsFS embeds the schema migration applied when a PostgresStore is opened.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// PostgresStore wraps a memory Store with PostgreSQL persistence. The memory store
// remains the read path and PostgreSQL is the durable projection of mutations.
type PostgresStore struct {
	// memory is the authoritative serving view; PostgreSQL is updated as its
	// durable projection rather than queried on normal reads.
	memory Store
	// db is owned by this store when opened through OpenPostgresStore* and shared
	// with the materializer for durable writes.
	db *sql.DB
	// mode decides whether mutation calls wait for SQL or only enqueue a delta.
	mode PostgresWriteMode
	// materializer is non-nil only in async mode and owns background batch writes.
	materializer *Materializer
	// deltaSeq versions async deltas so coalescing keeps the newest snapshot per key.
	deltaSeq atomic.Int64
	// mu protects last, which is written by both foreground sync paths and async callbacks.
	mu sync.Mutex
	// last records the most recent persistence/enqueue error observed by the store.
	last error
}

// PostgresWriteMode controls whether mutation calls wait for SQL writes.
type PostgresWriteMode string

// Postgres write modes choose whether the caller waits for SQL durability or
// only for an enqueue into the asynchronous materializer.
const (
	// PostgresWriteModeSync returns SQL persistence errors from mutation calls.
	PostgresWriteModeSync PostgresWriteMode = "sync"
	// PostgresWriteModeAsync returns after memory mutation and materializer enqueue.
	PostgresWriteModeAsync PostgresWriteMode = "async"
)

// PostgresOptions configures sync/async write behavior and async batch sizing.
type PostgresOptions struct {
	// Mode selects synchronous durability or asynchronous materialization.
	Mode PostgresWriteMode
	// BatchMax caps one async flush batch; non-positive values use defaults.
	BatchMax int
	// FlushInterval bounds async lag under low write volume.
	FlushInterval time.Duration
	// QueueSize bounds the fast enqueue channel before overflow coalescing is used.
	QueueSize int
}

// OpenPostgresStore opens a Postgres-backed store with default write options.
func OpenPostgresStore(ctx context.Context, dsn string) (*PostgresStore, error) {
	return OpenPostgresStoreWithOptions(ctx, dsn, PostgresOptions{})
}

// OpenPostgresStoreWithOptions opens the database, waits for connectivity, applies
// migrations, and returns a Store wrapper using the requested write mode.
func OpenPostgresStoreWithOptions(ctx context.Context, dsn string, opts PostgresOptions) (*PostgresStore, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("postgres dsn is required")
	}
	// sql.Open is lazy, so pingPostgres below is the actual connectivity gate.
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	if err := pingPostgres(ctx, db, 30*time.Second); err != nil {
		_ = db.Close()
		return nil, err
	}
	store := NewPostgresStoreWithOptions(db, opts)
	// Migrations run before the store is returned so callers never publish a
	// PostgresStore whose durable projection is missing required tables.
	if err := store.ApplyMigrations(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// pingPostgres retries until the database accepts connections, ctx is canceled,
// or the timeout expires.
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

		// Use a fixed short delay so startup tolerates container/database boot races
		// without hiding long outages beyond the explicit timeout.
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// NewPostgresStore wraps an existing DB handle in synchronous persistence mode.
func NewPostgresStore(db *sql.DB) *PostgresStore {
	return NewPostgresStoreWithOptions(db, PostgresOptions{})
}

// NewPostgresStoreWithOptions wraps an existing DB handle and starts the
// materializer when async writes are requested.
func NewPostgresStoreWithOptions(db *sql.DB, opts PostgresOptions) *PostgresStore {
	opts = normalizePostgresOptions(opts)
	store := &PostgresStore{memory: NewMemoryStore(), db: db, mode: opts.Mode}
	if opts.Mode == PostgresWriteModeAsync {
		// Async mode still mutates memory synchronously; the materializer only mirrors
		// accepted snapshots to PostgreSQL in the background.
		store.materializer = NewMaterializer(db, opts.BatchMax, opts.FlushInterval, opts.QueueSize, store.persistDeltas, store.remember)
		store.materializer.Start()
	}
	return store
}

// normalizePostgresOptions lowercases write mode input and fills conservative
// async batching defaults.
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

// ApplyMigrations executes the embedded schema SQL against the configured DB.
func (s *PostgresStore) ApplyMigrations(ctx context.Context) error {
	data, err := migrationsFS.ReadFile("migrations/001_init.sql")
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, string(data))
	return err
}

// Close flushes pending async metadata before closing the database handle and
// reports the first error observed.
func (s *PostgresStore) Close() error {
	var firstErr error
	if s.materializer != nil {
		// Close gives async mode a bounded final flush window before the DB handle is closed.
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

// LastError returns the last remembered persistence error, including async flush
// failures reported by the materializer.
func (s *PostgresStore) LastError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// NonBlockingPersistence reports whether mutations enqueue durable writes instead
// of waiting for SQL execution.
func (s *PostgresStore) NonBlockingPersistence() bool {
	return s != nil && s.mode == PostgresWriteModeAsync
}

// MaterializerStats exposes async flush state and returns a sync-mode placeholder
// when no materializer is configured.
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

// Flush waits for all queued async metadata writes; it is a no-op in sync mode.
func (s *PostgresStore) Flush(ctx context.Context) error {
	if s == nil || s.materializer == nil {
		return nil
	}
	return s.materializer.Flush(ctx)
}

// remember stores the last persistence error under a small mutex.
func (s *PostgresStore) remember(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = err
}

// recordPersist records sync write results immediately, while async mode records
// enqueue errors here and lets the materializer clear or set LastError on flush.
func (s *PostgresStore) recordPersist(err error) {
	if err != nil || !s.NonBlockingPersistence() {
		// Successful async enqueues do not clear a previous async flush error; the
		// materializer callback owns recovery/error reporting once it starts writing.
		s.remember(err)
	}
}

// CreateTask updates memory first and persists only newly created tasks;
// idempotent duplicates do not rewrite PostgreSQL.
func (s *PostgresStore) CreateTask(task Task, idempotencyKey string) (Task, bool) {
	created, duplicate := s.memory.CreateTask(task, idempotencyKey)
	if !duplicate {
		s.recordPersist(s.persistTaskOrEnqueue(created))
	}
	return created, duplicate
}

// GetTask reads from the memory projection.
func (s *PostgresStore) GetTask(taskID string) (Task, bool) {
	return s.memory.GetTask(taskID)
}

// GetTaskByIdempotencyKey reads the memory idempotency index.
func (s *PostgresStore) GetTaskByIdempotencyKey(idempotencyKey string) (Task, bool) {
	return s.memory.GetTaskByIdempotencyKey(idempotencyKey)
}

// ListTasks snapshots tasks from memory rather than querying PostgreSQL.
func (s *PostgresStore) ListTasks() []Task {
	return s.memory.ListTasks()
}

// LeaseTask updates the memory lease first, then persists or enqueues the new
// task state. Sync mode returns persistence errors to the caller.
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

// ValidateTaskLease checks the in-memory lease fence without touching PostgreSQL.
func (s *PostgresStore) ValidateTaskLease(taskID, workerID string, leaseEpoch uint64) (Task, error) {
	return s.memory.ValidateTaskLease(taskID, workerID, leaseEpoch)
}

// RequeueExpiredRunningTasks requeues expired leases in memory and persists each
// changed task state.
func (s *PostgresStore) RequeueExpiredRunningTasks(maxAge time.Duration) []Task {
	tasks := s.memory.RequeueExpiredRunningTasks(maxAge)
	for _, task := range tasks {
		s.recordPersist(s.persistTaskOrEnqueue(task))
	}
	return tasks
}

// RequeueTaskIfLeaseExpired persists the targeted requeue only when the lease
// actually expired and changed state.
func (s *PostgresStore) RequeueTaskIfLeaseExpired(taskID string, leaseEpoch uint64, maxAge time.Duration) (Task, bool) {
	task, requeued := s.memory.RequeueTaskIfLeaseExpired(taskID, leaseEpoch, maxAge)
	if requeued {
		s.recordPersist(s.persistTaskOrEnqueue(task))
	}
	return task, requeued
}

// CompleteTask commits the in-memory result after lease validation, then persists
// the completed task state.
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

// RegisterModel updates the memory registry and persists the normalized model
// record.
func (s *PostgresStore) RegisterModel(model *logservepb.ModelInfo) *logservepb.ModelInfo {
	registered := s.memory.RegisterModel(model)
	s.recordPersist(s.persistModelOrEnqueue(registered))
	return registered
}

// GetModel reads the memory model registry.
func (s *PostgresStore) GetModel(name, version string) (*logservepb.ModelInfo, bool) {
	return s.memory.GetModel(name, version)
}

// ListModels snapshots the memory model registry.
func (s *PostgresStore) ListModels() []*logservepb.ModelInfo {
	return s.memory.ListModels()
}

// CreateWorkflow updates memory and persists only the first state for an
// idempotency key.
func (s *PostgresStore) CreateWorkflow(state workflow.State, idempotencyKey string) (workflow.State, bool) {
	created, duplicate := s.memory.CreateWorkflow(state, idempotencyKey)
	if !duplicate {
		s.recordPersist(s.persistWorkflowOrEnqueue(created))
	}
	return created, duplicate
}

// GetWorkflow reads workflow state from memory.
func (s *PostgresStore) GetWorkflow(workflowID string) (workflow.State, bool) {
	return s.memory.GetWorkflow(workflowID)
}

// GetWorkflowByIdempotencyKey reads the memory workflow idempotency index.
func (s *PostgresStore) GetWorkflowByIdempotencyKey(idempotencyKey string) (workflow.State, bool) {
	return s.memory.GetWorkflowByIdempotencyKey(idempotencyKey)
}

// ListWorkflows snapshots workflows from memory.
func (s *PostgresStore) ListWorkflows() []workflow.State {
	return s.memory.ListWorkflows()
}

// UpdateWorkflow applies the memory mutation and persists the resulting snapshot
// if the callback succeeds.
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

// UpsertWorkflow replaces memory state and persists that snapshot.
func (s *PostgresStore) UpsertWorkflow(state workflow.State) {
	s.memory.UpsertWorkflow(state)
	s.recordPersist(s.persistWorkflowOrEnqueue(state))
}

// UpsertWorker refreshes memory, then persists the normalized worker snapshot
// read back from the memory store.
func (s *PostgresStore) UpsertWorker(worker Worker) {
	s.memory.UpsertWorker(worker)
	current, _ := s.memory.GetWorker(worker.WorkerID)
	s.recordPersist(s.persistWorkerOrEnqueue(current))
}

// GetWorker reads worker state from memory.
func (s *PostgresStore) GetWorker(workerID string) (Worker, bool) {
	return s.memory.GetWorker(workerID)
}

// ActiveWorkers returns scheduler-visible memory workers filtered by heartbeat.
func (s *PostgresStore) ActiveWorkers(maxAge time.Duration) []Worker {
	return s.memory.ActiveWorkers(maxAge)
}

// ListWorkers snapshots workers from memory.
func (s *PostgresStore) ListWorkers() []Worker {
	return s.memory.ListWorkers()
}

// Heartbeat updates memory liveness immediately and persists the worker snapshot.
func (s *PostgresStore) Heartbeat(workerID string, cachedModels map[string]bool) (Worker, bool) {
	worker, existed := s.memory.Heartbeat(workerID, cachedModels)
	s.recordPersist(s.persistWorkerOrEnqueue(worker))
	return worker, existed
}

// IncrementWorkerLoad changes the memory load counter and persists the refreshed
// worker when it exists.
func (s *PostgresStore) IncrementWorkerLoad(workerID string) {
	s.memory.IncrementWorkerLoad(workerID)
	if worker, ok := s.memory.GetWorker(workerID); ok {
		s.recordPersist(s.persistWorkerOrEnqueue(worker))
	}
}

// DecrementWorkerLoad changes the memory load counter and persists the refreshed
// worker when it exists.
func (s *PostgresStore) DecrementWorkerLoad(workerID string) {
	s.memory.DecrementWorkerLoad(workerID)
	if worker, ok := s.memory.GetWorker(workerID); ok {
		s.recordPersist(s.persistWorkerOrEnqueue(worker))
	}
}

// CreateActor stores actor state in memory and persists only new actor records.
func (s *PostgresStore) CreateActor(state actor.State, idempotencyKey string) (actor.State, bool) {
	created, duplicate := s.memory.CreateActor(state, idempotencyKey)
	if !duplicate {
		s.recordPersist(s.persistActorOrEnqueue(created))
	}
	return created, duplicate
}

// GetActor reads actor state from memory.
func (s *PostgresStore) GetActor(actorID string) (actor.State, bool) {
	return s.memory.GetActor(actorID)
}

// GetActorByIdempotencyKey reads the memory actor idempotency index.
func (s *PostgresStore) GetActorByIdempotencyKey(idempotencyKey string) (actor.State, bool) {
	return s.memory.GetActorByIdempotencyKey(idempotencyKey)
}

// ListActors snapshots actor state from memory.
func (s *PostgresStore) ListActors() []actor.State {
	return s.memory.ListActors()
}

// UpdateActor applies a memory actor mutation and persists the resulting snapshot
// when the callback succeeds.
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

// UpsertActor replaces memory actor state and persists that snapshot.
func (s *PostgresStore) UpsertActor(state actor.State) {
	s.memory.UpsertActor(state)
	s.recordPersist(s.persistActorOrEnqueue(state))
}

// sqlExecutor is the common subset shared by *sql.DB and *sql.Tx for persistence
// helpers.
type sqlExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// persistTaskOrEnqueue writes task state now in sync mode or enqueues a cloned
// delta in async mode.
func (s *PostgresStore) persistTaskOrEnqueue(task Task) error {
	if !s.NonBlockingPersistence() {
		return s.persistTask(context.Background(), task)
	}
	// Clone before enqueueing so later in-memory mutations cannot change the
	// snapshot the materializer is supposed to persist.
	return s.enqueueDelta(DeltaTask, task.TaskID, cloneTask(task))
}

// persistModelOrEnqueue writes model metadata now or enqueues a cloned async
// delta.
func (s *PostgresStore) persistModelOrEnqueue(model *logservepb.ModelInfo) error {
	if !s.NonBlockingPersistence() {
		return s.persistModel(context.Background(), model)
	}
	return s.enqueueDelta(DeltaModel, ModelKey(model.GetName(), model.GetVersion()), cloneModel(model))
}

// persistWorkflowOrEnqueue writes workflow state now or enqueues a cloned async
// delta.
func (s *PostgresStore) persistWorkflowOrEnqueue(state workflow.State) error {
	if !s.NonBlockingPersistence() {
		return s.persistWorkflow(context.Background(), state)
	}
	return s.enqueueDelta(DeltaWorkflow, state.WorkflowID, cloneWorkflow(state))
}

// persistWorkerOrEnqueue writes worker state now or enqueues a cloned async
// delta.
func (s *PostgresStore) persistWorkerOrEnqueue(worker Worker) error {
	if !s.NonBlockingPersistence() {
		return s.persistWorker(context.Background(), worker)
	}
	return s.enqueueDelta(DeltaWorker, worker.WorkerID, cloneWorker(worker))
}

// persistActorOrEnqueue writes actor state now or enqueues a cloned async delta.
func (s *PostgresStore) persistActorOrEnqueue(state actor.State) error {
	if !s.NonBlockingPersistence() {
		return s.persistActor(context.Background(), state)
	}
	return s.enqueueDelta(DeltaActor, state.ActorID, cloneActor(state))
}

// enqueueDelta assigns a monotonic version so the materializer can discard older
// coalesced writes for the same logical key.
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

// persistDeltas writes a materialized batch in one transaction so coalesced
// metadata updates become durable together.
func (s *PostgresStore) persistDeltas(ctx context.Context, deltas []metadataDelta) error {
	if len(deltas) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// Rollback is intentionally deferred even on success; database/sql treats it
	// as a no-op after Commit, and it keeps early returns transaction-safe.
	defer tx.Rollback()
	for _, delta := range deltas {
		// Each delta is already coalesced by logical key; writing them in one
		// transaction avoids partially materialized batches.
		if err := s.persistDeltaWith(ctx, tx, delta); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// persistDeltaWith validates the concrete delta payload type before dispatching
// to the table-specific UPSERT helper.
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

// persistTask writes one task through the store DB handle.
func (s *PostgresStore) persistTask(ctx context.Context, task Task) error {
	return s.persistTaskWith(ctx, s.db, task)
}

// persistTaskWith upserts task_instances and mirrors LLM task metadata into
// llm_requests when the task names a model.
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
		// llm_requests is a derived projection for LLM tasks only; regular tasks do
		// not create rows in that table.
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

// persistModel writes one model registry record through the store DB handle.
func (s *PostgresStore) persistModel(ctx context.Context, model *logservepb.ModelInfo) error {
	return s.persistModelWith(ctx, s.db, model)
}

// persistModelWith upserts a model registry row, applying the same v1/mock
// defaults as the in-memory store.
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

// persistWorkflow writes workflow instance and step rows in one transaction.
func (s *PostgresStore) persistWorkflow(ctx context.Context, state workflow.State) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// Rollback is intentionally deferred even on success; database/sql treats it
	// as a no-op after Commit, and it keeps early returns transaction-safe.
	defer tx.Rollback()
	if err := s.persistWorkflowWith(ctx, tx, state); err != nil {
		return err
	}
	return tx.Commit()
}

// persistWorkflowWith upserts the workflow header followed by each ordered step
// snapshot.
func (s *PostgresStore) persistWorkflowWith(ctx context.Context, exec sqlExecutor, state workflow.State) error {
	// Store the full workflow definition separately from input/output JSON so
	// replay/debug tooling can inspect the submitted graph shape.
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

	// Step order is derived from workflow.State so SQL writes remain deterministic
	// across map-backed and slice-backed workflow representations.
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

// persistWorker writes a worker and its cached-model rows in one transaction.
func (s *PostgresStore) persistWorker(ctx context.Context, worker Worker) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	// Rollback is intentionally deferred even on success; database/sql treats it
	// as a no-op after Commit, and it keeps early returns transaction-safe.
	defer tx.Rollback()
	if err := s.persistWorkerWith(ctx, tx, worker); err != nil {
		return err
	}
	return tx.Commit()
}

// persistWorkerWith upserts worker metadata and rebuilds the worker model-cache
// projection from the current snapshot.
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

	// Rebuild cache rows from scratch so false/removed model entries disappear from
	// the durable projection.
	if _, err := exec.ExecContext(ctx, `DELETE FROM worker_model_cache WHERE worker_id = $1`, worker.WorkerID); err != nil {
		return err
	}
	keys := make([]string, 0, len(worker.CachedModels))
	for key, cached := range worker.CachedModels {
		// Only true entries are persisted; false entries represent absent cache rows.
		if cached {
			keys = append(keys, key)
		}
	}

	// Sort cache keys to make SQL execution order deterministic for tests and logs.
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

// persistActor writes one actor state through the store DB handle.
func (s *PostgresStore) persistActor(ctx context.Context, state actor.State) error {
	return s.persistActorWith(ctx, s.db, state)
}

// persistActorWith upserts actor_instances, including snapshot and command-count
// fields used for actor replay/fencing.
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

// jsonValue maps empty JSON payloads to SQL NULL and non-empty payloads to jsonb
// strings.
func jsonValue(data []byte) any {
	// The store treats missing JSON and empty JSON as absent optional payloads;
	// callers that need an explicit JSON empty value must pass a non-empty buffer.
	if len(data) == 0 {
		return nil
	}
	return string(data)
}

// nullString maps empty Go strings to SQL NULL for optional text fields.
func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// nullTime maps non-positive millisecond timestamps to SQL NULL.
func nullTime(ms int64) any {
	if ms <= 0 {
		return nil
	}
	return time.UnixMilli(ms)
}

// msTime converts millisecond timestamps for required columns, using now when
// callers omitted a timestamp.
func msTime(ms int64) time.Time {
	if ms <= 0 {
		// Required timestamp columns cannot be NULL, so omitted metadata timestamps
		// materialize as the persistence time.
		return time.Now()
	}
	return time.UnixMilli(ms)
}

// firstNonEmpty returns the first non-empty value from a default chain.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// splitModelKey parses canonical name:version keys and defaults legacy keys
// without a separator to version v1.
func splitModelKey(key string) (string, string) {
	name, version, ok := strings.Cut(key, ":")
	if !ok {
		return key, "v1"
	}
	return name, version
}
