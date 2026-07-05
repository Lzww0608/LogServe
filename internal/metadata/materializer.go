package metadata

// This file implements the asynchronous Postgres materializer used by
// PostgresStore when metadata writes should not block control-plane calls.

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// DeltaKind identifies the logical metadata table affected by a queued write.
type DeltaKind string

// Delta kinds namespace materializer keys so records from different metadata
// tables can share the same logical ID without coalescing into each other.
const (
	// DeltaTask persists one task_instances projection.
	DeltaTask DeltaKind = "task"
	// DeltaWorker persists one worker_instances projection.
	DeltaWorker DeltaKind = "worker"
	// DeltaModel persists one model_registry projection.
	DeltaModel DeltaKind = "model"
	// DeltaWorkflow persists one workflow_instances plus workflow_steps projection.
	DeltaWorkflow DeltaKind = "workflow"
	// DeltaActor persists one actor_instances projection.
	DeltaActor DeltaKind = "actor"
)

// metadataDelta is the unit of asynchronous persistence. version is monotonic per
// PostgresStore and lets coalescing keep the newest write for each logical key.
type metadataDelta struct {
	kind    DeltaKind
	key     string
	payload any
	version int64
}

// MaterializerStats exposes queue, batch, and last-flush state for operators and
// tests without exposing internal synchronization primitives.
type MaterializerStats struct {
	Mode                string
	PendingDeltas       int
	QueuedDeltas        int
	BatchMax            int
	FlushInterval       time.Duration
	FlushCount          uint64
	FlushErrorCount     uint64
	LastFlushAt         time.Time
	LastSuccessAt       time.Time
	LastErrorAt         time.Time
	LastFlushDuration   time.Duration
	LastFlushDeltas     int
	LastError           string
	EventualLagEstimate time.Duration
}

// materializerFlush persists one coalesced batch of metadata deltas.
type materializerFlush func(context.Context, []metadataDelta) error

// materializerFlushRequest synchronizes a caller-visible Flush request with the
// background materializer goroutine.
type materializerFlushRequest struct {
	ctx   context.Context
	reply chan error
}

// Materializer coalesces high-frequency metadata updates and flushes them to the
// durable backend on size, time, explicit Flush, or Close triggers.
type Materializer struct {
	// queue is the bounded fast path; when it is full, Enqueue coalesces by key in overflow.
	queue chan metadataDelta
	// batchMax caps one durable flush so a large burst does not monopolize the goroutine.
	batchMax int
	// flushInterval bounds eventual-consistency lag when traffic stays below batchMax.
	flushInterval time.Duration
	db            *sql.DB

	// flush performs the durable write; only the run goroutine calls it.
	flush materializerFlush
	// onErr reports both failed and recovered flush attempts to the owning store.
	onErr func(error)
	// done is closed once to stop accepting work and request final draining.
	done chan struct{}
	// closed is closed by run after the final drain sets closeErr.
	closed chan struct{}

	// overflowMu protects overflow because producers write it outside the run goroutine.
	overflowMu sync.Mutex
	// overflow stores the newest dropped-queue delta per logical key.
	overflow map[string]metadataDelta
	// overflowSignal is size one so many overflow writes collapse into one wake-up.
	overflowSignal chan struct{}
	// flushRequests hands synchronous Flush calls to the run goroutine that owns pending.
	flushRequests chan materializerFlushRequest
	// closeOnce guarantees done is closed exactly once across concurrent Close calls.
	closeOnce sync.Once

	// pendingCount tracks queued channel entries because len(queue) is only a snapshot.
	pendingCount atomic.Int64
	// pendingKeys exposes the goroutine-owned pending map size without taking its ownership.
	pendingKeys atomic.Int64
	// firstPendingUnixNano anchors lag estimates until all queue/overflow/pending state drains.
	firstPendingUnixNano atomic.Int64
	// flushCount and errorCount are monotonic counters read by Stats without statsMu.
	flushCount atomic.Uint64
	errorCount atomic.Uint64

	// statsMu protects the last-* fields that are updated as one flush observation.
	statsMu           sync.Mutex
	lastFlushAt       time.Time
	lastSuccessAt     time.Time
	lastErrorAt       time.Time
	lastFlushDuration time.Duration
	lastFlushDeltas   int
	lastError         string
	closeErr          error
}

// NewMaterializer constructs an async writer with conservative defaults for
// batch size, flush interval, and queue capacity.
func NewMaterializer(db *sql.DB, batchMax int, flushInterval time.Duration, queueSize int, flush materializerFlush, onErr func(error)) *Materializer {
	if batchMax <= 0 {
		batchMax = 256
	}
	if flushInterval <= 0 {
		flushInterval = time.Second
	}
	if queueSize <= 0 {
		queueSize = batchMax * 4
	}
	return &Materializer{
		queue:          make(chan metadataDelta, queueSize),
		batchMax:       batchMax,
		flushInterval:  flushInterval,
		db:             db,
		flush:          flush,
		onErr:          onErr,
		done:           make(chan struct{}),
		closed:         make(chan struct{}),
		overflow:       make(map[string]metadataDelta),
		overflowSignal: make(chan struct{}, 1),
		flushRequests:  make(chan materializerFlushRequest),
	}
}

// Start launches the single background goroutine that owns the pending delta map.
func (m *Materializer) Start() {
	go m.run()
}

// Enqueue records a metadata delta without blocking on durable I/O. When the
// bounded queue is full, the delta is merged into overflow storage by logical key.
func (m *Materializer) Enqueue(delta metadataDelta) error {
	if m == nil {
		return errors.New("metadata materializer is nil")
	}
	select {
	case <-m.done:
		return errors.New("metadata materializer is closed")
	default:
	}
	m.markPending(time.Now())
	select {
	case m.queue <- delta:
		m.pendingCount.Add(1)
		return nil
	default:
		m.mergeOverflow(delta)
		m.signalOverflow()
		return nil
	}
}

// Flush waits until all currently visible queued, overflow, and pending deltas
// have been persisted or ctx is canceled.
func (m *Materializer) Flush(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	reply := make(chan error, 1)
	req := materializerFlushRequest{ctx: ctx, reply: reply}
	// Flush must rendezvous with run because only that goroutine has a complete
	// view of the coalesced pending map.
	select {
	case m.flushRequests <- req:
	case <-m.closed:
		return m.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-reply:
		return err
	case <-m.closed:
		return m.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops accepting work and waits for the background goroutine to flush
// remaining deltas before returning.
func (m *Materializer) Close(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.closeOnce.Do(func() {
		close(m.done)
	})
	// Close observes the final drain result through closed/closeErr instead of
	// flushing directly from the caller goroutine.
	select {
	case <-m.closed:
		return m.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stats returns a point-in-time view of queue depth and last flush state. It is
// safe to call on nil materializers so sync Postgres mode can share one path.
func (m *Materializer) Stats(mode string) MaterializerStats {
	if m == nil {
		return MaterializerStats{Mode: mode}
	}
	firstPendingNano := m.firstPendingUnixNano.Load()
	m.statsMu.Lock()
	stats := MaterializerStats{
		Mode:              mode,
		PendingDeltas:     int(m.pendingKeys.Load()) + len(m.queue) + m.overflowLen(),
		QueuedDeltas:      len(m.queue),
		BatchMax:          m.batchMax,
		FlushInterval:     m.flushInterval,
		FlushCount:        m.flushCount.Load(),
		FlushErrorCount:   m.errorCount.Load(),
		LastFlushAt:       m.lastFlushAt,
		LastSuccessAt:     m.lastSuccessAt,
		LastErrorAt:       m.lastErrorAt,
		LastFlushDuration: m.lastFlushDuration,
		LastFlushDeltas:   m.lastFlushDeltas,
		LastError:         m.lastError,
	}
	m.statsMu.Unlock()
	if stats.PendingDeltas > 0 {
		switch {
		case firstPendingNano > 0:
			stats.EventualLagEstimate = time.Since(time.Unix(0, firstPendingNano))
		case !stats.LastSuccessAt.IsZero():
			stats.EventualLagEstimate = time.Since(stats.LastSuccessAt)
		}
	}
	return stats
}

// run owns the pending delta map and serializes queue drains, flush requests,
// interval flushes, and shutdown flushes.
func (m *Materializer) run() {
	defer close(m.closed)
	ticker := time.NewTicker(m.flushInterval)
	defer ticker.Stop()

	pending := make(map[string]metadataDelta)
	for {
		select {
		case delta := <-m.queue:
			m.pendingCount.Add(-1)
			// One received delta is enough to wake the owner goroutine; drainQueue
			// then folds the rest of the current burst into the same pending map.
			mergeDelta(pending, delta)
			m.drainQueue(pending)
			m.drainOverflow(pending)
			m.updatePendingKeys(pending)
			if len(pending) >= m.batchMax {
				_ = m.flushPending(context.Background(), pending)
			}
		case <-m.overflowSignal:
			m.drainOverflow(pending)
			m.updatePendingKeys(pending)
			if len(pending) >= m.batchMax {
				_ = m.flushPending(context.Background(), pending)
			}
		case req := <-m.flushRequests:
			// Explicit Flush observes all work visible to the goroutine before it
			// replies, including overflow entries that bypassed the bounded queue.
			m.drainQueue(pending)
			m.drainOverflow(pending)
			m.updatePendingKeys(pending)
			req.reply <- m.flushAllPending(req.ctx, pending)
		case <-ticker.C:
			m.drainQueue(pending)
			m.drainOverflow(pending)
			m.updatePendingKeys(pending)
			_ = m.flushAllPending(context.Background(), pending)
		case <-m.done:
			// Shutdown keeps final persistence inside the owner goroutine so no
			// other caller needs direct access to the pending map.
			m.drainQueue(pending)
			m.drainOverflow(pending)
			m.updatePendingKeys(pending)
			m.closeErr = m.flushAllPending(context.Background(), pending)
			return
		}
	}
}

// drainQueue opportunistically consumes all immediately available queue items so
// one wake-up can coalesce a burst into a single pending map.
func (m *Materializer) drainQueue(pending map[string]metadataDelta) {
	for {
		select {
		case delta := <-m.queue:
			m.pendingCount.Add(-1)
			mergeDelta(pending, delta)
		default:
			return
		}
	}
}

// markPending records the first observed pending timestamp used to estimate
// eventual-consistency lag.
func (m *Materializer) markPending(now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	m.firstPendingUnixNano.CompareAndSwap(0, now.UnixNano())
}

// updatePendingKeys refreshes observable pending counters and clears the lag
// anchor once every queue is empty.
func (m *Materializer) updatePendingKeys(pending map[string]metadataDelta) {
	m.pendingKeys.Store(int64(len(pending)))
	if len(pending) == 0 && len(m.queue) == 0 && m.overflowLen() == 0 {
		m.firstPendingUnixNano.Store(0)
	}
}

// overflowLen reports the number of coalesced overflow keys under its lock.
func (m *Materializer) overflowLen() int {
	m.overflowMu.Lock()
	defer m.overflowMu.Unlock()
	return len(m.overflow)
}

// drainOverflow moves overflow writes into the goroutine-owned pending map.
func (m *Materializer) drainOverflow(pending map[string]metadataDelta) {
	m.overflowMu.Lock()
	for key, delta := range m.overflow {
		mergeDelta(pending, delta)
		delete(m.overflow, key)
	}
	m.overflowMu.Unlock()
}

// mergeOverflow coalesces a dropped-queue delta by logical key while preserving
// the newest version.
func (m *Materializer) mergeOverflow(delta metadataDelta) {
	m.overflowMu.Lock()
	mergeDelta(m.overflow, delta)
	m.overflowMu.Unlock()
}

// signalOverflow wakes the run loop at most once per burst; a buffered channel
// intentionally collapses duplicate wake-ups.
func (m *Materializer) signalOverflow() {
	select {
	case m.overflowSignal <- struct{}{}:
	default:
	}
}

// flushPending persists a deterministic batch from the pending map and removes
// only the keys that were successfully written.
func (m *Materializer) flushPending(ctx context.Context, pending map[string]metadataDelta) error {
	if len(pending) == 0 {
		m.updatePendingKeys(pending)
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	keys := make([]string, 0, len(pending))
	for key := range pending {
		keys = append(keys, key)
	}
	// Stable ordering makes tests repeatable and prevents batch boundaries from
	// depending on Go's randomized map iteration.
	sort.Strings(keys)
	if m.batchMax > 0 && len(keys) > m.batchMax {
		keys = keys[:m.batchMax]
	}
	deltas := make([]metadataDelta, 0, len(keys))
	for _, key := range keys {
		deltas = append(deltas, pending[key])
	}

	started := time.Now()
	err := m.flush(ctx, deltas)
	elapsed := time.Since(started)
	if err != nil {
		m.updatePendingKeys(pending)
		m.recordFlush(err, started, elapsed, len(deltas))
		if m.onErr != nil {
			m.onErr(err)
		}
		return err
	}
	for _, key := range keys {
		delete(pending, key)
	}
	m.updatePendingKeys(pending)
	m.recordFlush(nil, started, elapsed, len(deltas))
	if m.onErr != nil {
		m.onErr(nil)
	}
	m.flushCount.Add(1)
	return nil
}

// flushAllPending repeatedly calls flushPending until the pending map is empty
// or the caller context stops the explicit flush.
func (m *Materializer) flushAllPending(ctx context.Context, pending map[string]metadataDelta) error {
	for len(pending) > 0 {
		if ctx != nil {
			select {
			case <-ctx.Done():
				m.updatePendingKeys(pending)
				return ctx.Err()
			default:
			}
		}
		if err := m.flushPending(ctx, pending); err != nil {
			return err
		}
	}
	m.updatePendingKeys(pending)
	return nil
}

// recordFlush updates operator-visible flush metrics and counts failures.
func (m *Materializer) recordFlush(err error, started time.Time, elapsed time.Duration, count int) {
	m.statsMu.Lock()
	defer m.statsMu.Unlock()
	m.lastFlushAt = started
	m.lastFlushDuration = elapsed
	m.lastFlushDeltas = count
	if err != nil {
		m.lastErrorAt = time.Now()
		m.lastError = err.Error()
		m.errorCount.Add(1)
		return
	}
	m.lastSuccessAt = time.Now()
	m.lastError = ""
}

// mergeDelta keeps the highest-version delta for a logical metadata key so older
// async writes cannot overwrite newer state in the same batch window.
func mergeDelta(pending map[string]metadataDelta, delta metadataDelta) {
	key := deltaMapKey(delta)
	if existing, ok := pending[key]; ok && existing.version > delta.version {
		return
	}
	pending[key] = delta
}

// deltaMapKey namespaces keys by metadata kind because task IDs, worker IDs, and
// model keys can otherwise share the same string value.
func deltaMapKey(delta metadataDelta) string {
	return string(delta.kind) + ":" + delta.key
}
