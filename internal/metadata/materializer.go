package metadata

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type DeltaKind string

const (
	DeltaTask     DeltaKind = "task"
	DeltaWorker   DeltaKind = "worker"
	DeltaModel    DeltaKind = "model"
	DeltaWorkflow DeltaKind = "workflow"
	DeltaActor    DeltaKind = "actor"
)

type metadataDelta struct {
	kind    DeltaKind
	key     string
	payload any
	version int64
}

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

type materializerFlush func(context.Context, []metadataDelta) error

type materializerFlushRequest struct {
	ctx   context.Context
	reply chan error
}

type Materializer struct {
	queue         chan metadataDelta
	batchMax      int
	flushInterval time.Duration
	db            *sql.DB

	flush  materializerFlush
	onErr  func(error)
	done   chan struct{}
	closed chan struct{}

	overflowMu     sync.Mutex
	overflow       map[string]metadataDelta
	overflowSignal chan struct{}
	flushRequests  chan materializerFlushRequest
	closeOnce      sync.Once

	pendingCount         atomic.Int64
	pendingKeys          atomic.Int64
	firstPendingUnixNano atomic.Int64
	flushCount           atomic.Uint64
	errorCount           atomic.Uint64

	statsMu           sync.Mutex
	lastFlushAt       time.Time
	lastSuccessAt     time.Time
	lastErrorAt       time.Time
	lastFlushDuration time.Duration
	lastFlushDeltas   int
	lastError         string
	closeErr          error
}

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

func (m *Materializer) Start() {
	go m.run()
}

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

func (m *Materializer) Flush(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	reply := make(chan error, 1)
	req := materializerFlushRequest{ctx: ctx, reply: reply}
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
	select {
	case <-m.closed:
		return m.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

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

func (m *Materializer) run() {
	defer close(m.closed)
	ticker := time.NewTicker(m.flushInterval)
	defer ticker.Stop()

	pending := make(map[string]metadataDelta)
	for {
		select {
		case delta := <-m.queue:
			m.pendingCount.Add(-1)
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
			m.drainQueue(pending)
			m.drainOverflow(pending)
			m.updatePendingKeys(pending)
			m.closeErr = m.flushAllPending(context.Background(), pending)
			return
		}
	}
}

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

func (m *Materializer) markPending(now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	m.firstPendingUnixNano.CompareAndSwap(0, now.UnixNano())
}

func (m *Materializer) updatePendingKeys(pending map[string]metadataDelta) {
	m.pendingKeys.Store(int64(len(pending)))
	if len(pending) == 0 && len(m.queue) == 0 && m.overflowLen() == 0 {
		m.firstPendingUnixNano.Store(0)
	}
}

func (m *Materializer) overflowLen() int {
	m.overflowMu.Lock()
	defer m.overflowMu.Unlock()
	return len(m.overflow)
}
func (m *Materializer) drainOverflow(pending map[string]metadataDelta) {
	m.overflowMu.Lock()
	for key, delta := range m.overflow {
		mergeDelta(pending, delta)
		delete(m.overflow, key)
	}
	m.overflowMu.Unlock()
}

func (m *Materializer) mergeOverflow(delta metadataDelta) {
	m.overflowMu.Lock()
	mergeDelta(m.overflow, delta)
	m.overflowMu.Unlock()
}

func (m *Materializer) signalOverflow() {
	select {
	case m.overflowSignal <- struct{}{}:
	default:
	}
}

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

func mergeDelta(pending map[string]metadataDelta, delta metadataDelta) {
	key := deltaMapKey(delta)
	if existing, ok := pending[key]; ok && existing.version > delta.version {
		return
	}
	pending[key] = delta
}

func deltaMapKey(delta metadataDelta) string {
	return string(delta.kind) + ":" + delta.key
}
