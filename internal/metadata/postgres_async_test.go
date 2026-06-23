package metadata

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

const recordingPostgresDriverName = "logserve_recording_postgres"

func init() {
	sql.Register(recordingPostgresDriverName, recordingPostgresDriver{})
}

type recordingPostgresDriver struct{}

type recordingPostgresConn struct {
	recorder *recordingPostgresRecorder
}

type recordingPostgresTx struct{}

type recordingPostgresRecorder struct {
	mu      sync.Mutex
	queries []string
	err     error
}

var recordingPostgresRecorders sync.Map

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

func (recordingPostgresDriver) Open(name string) (driver.Conn, error) {
	value, ok := recordingPostgresRecorders.Load(name)
	if !ok {
		return nil, errors.New("recording postgres recorder not found")
	}
	return &recordingPostgresConn{recorder: value.(*recordingPostgresRecorder)}, nil
}

func (c *recordingPostgresConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is unsupported")
}
func (c *recordingPostgresConn) Close() error               { return nil }
func (c *recordingPostgresConn) Begin() (driver.Tx, error)  { return recordingPostgresTx{}, nil }
func (c *recordingPostgresConn) Ping(context.Context) error { return nil }
func (c *recordingPostgresConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return recordingPostgresTx{}, nil
}
func (c *recordingPostgresConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	return c.recorder.exec(query)
}

func (recordingPostgresTx) Commit() error   { return nil }
func (recordingPostgresTx) Rollback() error { return nil }

func (r *recordingPostgresRecorder) exec(query string) (driver.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	r.queries = append(r.queries, query)
	return driver.RowsAffected(1), nil
}

func (r *recordingPostgresRecorder) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

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

func (r *recordingPostgresRecorder) countAll() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.queries)
}

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
