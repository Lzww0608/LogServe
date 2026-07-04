package actorlock

// This file provides a small per-actor lock table used to serialize actor work
// without forcing unrelated actors through one global mutex.
import "sync"

// Table holds per-actor mutexes and evicts idle entries after unlock so long-lived
// deployments do not grow without bound as actors are created and retired.
type Table struct {
	mu    sync.Mutex
	locks map[string]*entry
}

// entry is the lock state for one actor ID. refs counts goroutines that have
// either acquired the per-actor mutex or are waiting to acquire it.
type entry struct {
	mu   sync.Mutex
	refs int
}

// NewTable constructs an empty per-actor lock table.
func NewTable() *Table {
	return &Table{locks: make(map[string]*entry)}
}

// Lock acquires the per-actor mutex and returns an unlock function that may
// remove the entry when no other goroutine is waiting.
func (t *Table) Lock(actorID string) func() {
	if t == nil || actorID == "" {
		return func() {}
	}
	t.mu.Lock()
	e, ok := t.locks[actorID]
	if !ok {
		e = &entry{}
		t.locks[actorID] = e
	}

	// Increment refs before waiting on e.mu so the table keeps this entry alive
	// while a goroutine is queued behind an active actor lock holder.
	e.refs++
	t.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		t.mu.Lock()
		e.refs--
		if e.refs == 0 && t.locks[actorID] == e {

			// Only the last waiter/holder removes the entry, and only if no newer entry
			// has replaced this pointer for the same actor ID.
			delete(t.locks, actorID)
		}
		t.mu.Unlock()
	}
}

// Len returns the number of actor IDs currently tracked by the table.
func (t *Table) Len() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.locks)
}
