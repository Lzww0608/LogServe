package actorlock

import "sync"

// Table holds per-actor mutexes and evicts idle entries after unlock so long-lived
// deployments do not grow without bound as actors are created and retired.
type Table struct {
	mu    sync.Mutex
	locks map[string]*entry
}

type entry struct {
	mu   sync.Mutex
	refs int
}

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
	e.refs++
	t.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		t.mu.Lock()
		e.refs--
		if e.refs == 0 && t.locks[actorID] == e {
			delete(t.locks, actorID)
		}
		t.mu.Unlock()
	}
}

func (t *Table) Len() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.locks)
}
