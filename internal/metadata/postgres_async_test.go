package metadata

// This file tests async Postgres persistence with a recording SQL driver so the
// materializer can be exercised without a real database server.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
)

// recordingPostgresDriverName names the in-process SQL driver used by async
// materializer tests and benchmarks.
const recordingPostgresDriverName = "logserve_recording_postgres"

// init registers the recording driver once for the metadata test package.
func init() {
	sql.Register(recordingPostgresDriverName, recordingPostgresDriver{})
}

// recordingPostgresDriver resolves test DSNs to per-test recorders.
type recordingPostgresDriver struct{}

// recordingPostgresConn implements the small database/sql driver surface needed
// by PostgresStore persistence helpers.
type recordingPostgresConn struct {
	recorder *recordingPostgresRecorder
}

// recordingPostgresTx is a no-op transaction used to test transactional code
// paths without a database.
type recordingPostgresTx struct{}

// recordingPostgresRecorder captures executed SQL and can inject execution
// failures for async error-path tests.
type recordingPostgresRecorder struct {
	mu      sync.Mutex
	queries []string
	err     error
}

// recordingPostgresRecorders maps DSNs to recorders so database/sql can open
// independent connections for each test.
var recordingPostgresRecorders sync.Map

// openRecordingPostgresDB creates a unique recorder-backed DB handle for one
// test or benchmark and cleans the registry on completion.
func openRecordingPostgresDB(t testing.TB) (*sql.DB, *recordingPostgresRecorder) {
	t.Helper()
	dsn := t.Name() + ":" + time.Now().Format("150405.000000000")
	recorder := &recordingPostgresRecorder{}
	recordingPostgresRecorders.Store(dsn, recorder)
	t.Cleanup(func() { recordingPostgresRecorders.Delete(dsn) })
	db, err := sql.Open(recordingPostgresDriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	return db, recorder
}

// Open resolves the DSN to its recorder and returns a lightweight test connection.
func (recordingPostgresDriver) Open(name string) (driver.Conn, error) {
	value, ok := recordingPostgresRecorders.Load(name)
	if !ok {
		return nil, errors.New("recording postgres recorder not found")
	}
	return &recordingPostgresConn{recorder: value.(*recordingPostgresRecorder)}, nil
}

// Prepare returns an error because production code under test uses ExecContext
// directly.
func (c *recordingPostgresConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is unsupported")
}

// Close satisfies driver.Conn; recorder-backed connections have no resources.
func (c *recordingPostgresConn) Close() error { return nil }

// Begin satisfies legacy transaction creation for database/sql.
func (c *recordingPostgresConn) Begin() (driver.Tx, error) { return recordingPostgresTx{}, nil }

// Ping satisfies driver.Pinger so OpenPostgresStore connectivity checks can pass.
func (c *recordingPostgresConn) Ping(context.Context) error { return nil }

// BeginTx returns a no-op transaction for batch materializer tests.
func (c *recordingPostgresConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return recordingPostgresTx{}, nil
}

// ExecContext records SQL text and returns the recorder-injected error if set.
func (c *recordingPostgresConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	return c.recorder.exec(query)
}

// Commit satisfies driver.Tx and intentionally has no side effects.
func (recordingPostgresTx) Commit() error { return nil }

// Rollback satisfies driver.Tx and intentionally has no side effects.
func (recordingPostgresTx) Rollback() error { return nil }

// exec records one query unless the recorder has been configured to fail.
func (r *recordingPostgresRecorder) exec(query string) (driver.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	r.queries = append(r.queries, query)
	return driver.RowsAffected(1), nil
}

// fail configures subsequent ExecContext calls to return err.
func (r *recordingPostgresRecorder) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

// countContains counts recorded SQL statements containing a fragment.
func (r *recordingPostgresRecorder) countContains(fragment string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, query := range r.queries {
		if strings.Contains(query, fragment) {
			count++
		}
	}
	return count
}

// countAll returns the total number of recorded SQL executions.
func (r *recordingPostgresRecorder) countAll() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.queries)
}

// TestPostgresStoreAsyncMaterializerCoalescesHeartbeats verifies repeated worker
// heartbeats are coalesced into one durable write after explicit flush.
func TestPostgresStoreAsyncMaterializerCoalescesHeartbeats(t *testing.T) {
	db, recorder := openRecordingPostgresDB(t)
	store := NewPostgresStoreWithOptions(db, PostgresOptions{
		Mode:          PostgresWriteModeAsync,
		BatchMax:      64,
		FlushInterval: time.Hour,
		QueueSize:     1,
	})
	defer store.Close()

	for i := 0; i < 5; i++ {
		store.Heartbeat("worker-1", map[string]bool{"model-A:v1": true})
	}
	worker, ok := store.GetWorker("worker-1")
	if !ok {
		t.Fatal("worker missing from memory store")
	}
	if worker.LastHeartbeat == 0 {
		t.Fatal("heartbeat did not update memory before postgres flush")
	}
	if got := recorder.countContains("INSERT INTO workers"); got != 0 {
		t.Fatalf("postgres writes before explicit flush = %d, want 0", got)
	}
	if err := store.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := recorder.countContains("INSERT INTO workers"); got != 1 {
		t.Fatalf("worker upserts = %d, want 1 after coalesced flush", got)
	}
	stats := store.MaterializerStats()
	if stats.FlushCount == 0 || stats.LastFlushDeltas != 1 {
		t.Fatalf("materializer stats = %+v, want one flushed coalesced delta", stats)
	}
}

// TestPostgresStoreAsyncFlushErrorDoesNotBlockPrimaryPath ensures async write
// failures do not make foreground task mutations fail before Flush.
func TestPostgresStoreAsyncFlushErrorDoesNotBlockPrimaryPath(t *testing.T) {
	db, recorder := openRecordingPostgresDB(t)
	store := NewPostgresStoreWithOptions(db, PostgresOptions{
		Mode:          PostgresWriteModeAsync,
		BatchMax:      64,
		FlushInterval: time.Hour,
	})
	defer store.Close()
	recorder.fail(errors.New("postgres down"))

	created, duplicate := store.CreateTask(Task{
		TaskID:   "task-async-error",
		TaskName: "demo",
		Status:   logservepb.TaskStatus_TASK_STATUS_QUEUED,
	}, "")
	if duplicate {
		t.Fatal("unexpected duplicate task")
	}
	leased, err := store.LeaseTask(created.TaskID, "worker-1")
	if err != nil {
		t.Fatalf("LeaseTask returned postgres error in async mode: %v", err)
	}
	if leased.Status != logservepb.TaskStatus_TASK_STATUS_RUNNING {
		t.Fatalf("leased status = %s, want RUNNING", leased.Status)
	}
	if store.LastError() != nil {
		t.Fatalf("LastError before flush = %v, want nil", store.LastError())
	}
	if err := store.Flush(context.Background()); err == nil {
		t.Fatal("Flush succeeded despite driver failure")
	}
	if store.LastError() == nil {
		t.Fatal("LastError did not retain async flush failure")
	}
}

// TestMaterializerStatsReportLagBeforeFirstFlush verifies pending lag is visible
// before the first successful materializer flush and clears afterward.
func TestMaterializerStatsReportLagBeforeFirstFlush(t *testing.T) {
	materializer := NewMaterializer(nil, 64, time.Hour, 8, func(context.Context, []metadataDelta) error {
		return nil
	}, nil)
	materializer.Start()
	defer materializer.Close(context.Background())

	if err := materializer.Enqueue(metadataDelta{kind: DeltaWorker, key: "worker-lag", version: 1}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	stats := materializer.Stats("async")
	if stats.PendingDeltas == 0 {
		t.Fatalf("pending deltas = 0, want pending materialization work")
	}
	if stats.EventualLagEstimate <= 0 {
		t.Fatalf("eventual lag = %s, want positive lag before first flush", stats.EventualLagEstimate)
	}
	if err := materializer.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats = materializer.Stats("async")
	if stats.PendingDeltas != 0 {
		t.Fatalf("pending deltas after flush = %d, want 0", stats.PendingDeltas)
	}
	if stats.EventualLagEstimate != 0 {
		t.Fatalf("eventual lag after flush = %s, want 0", stats.EventualLagEstimate)
	}
}

// TestMaterializerFlushAllDrainsMultipleBatches verifies explicit flush drains
// more pending deltas than batchMax by issuing multiple batches.
func TestMaterializerFlushAllDrainsMultipleBatches(t *testing.T) {
	var batches [][]metadataDelta
	materializer := NewMaterializer(nil, 2, time.Hour, 8, func(_ context.Context, deltas []metadataDelta) error {
		if len(deltas) > 2 {
			t.Fatalf("batch size = %d, want at most 2", len(deltas))
		}
		batch := append([]metadataDelta(nil), deltas...)
		batches = append(batches, batch)
		return nil
	}, nil)
	pending := make(map[string]metadataDelta)
	for i := 0; i < 5; i++ {
		mergeDelta(pending, metadataDelta{
			kind:    DeltaWorker,
			key:     fmt.Sprintf("worker-%d", i),
			version: int64(i + 1),
		})
	}

	if err := materializer.flushAllPending(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending deltas after flush = %d, want 0", len(pending))
	}
	if len(batches) != 3 {
		t.Fatalf("batch count = %d, want 3", len(batches))
	}
	total := 0
	for _, batch := range batches {
		total += len(batch)
	}
	if total != 5 {
		t.Fatalf("flushed deltas = %d, want 5", total)
	}
}
