package actorlock

// This file tests actor lock table eviction and actor-ID-level serialization.
import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

// TestTableEvictsIdleActorLock verifies a lock entry is removed after its only
// holder unlocks.
func TestTableEvictsIdleActorLock(t *testing.T) {
	table := NewTable()
	unlock := table.Lock("actor-1")
	if table.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", table.Len())
	}
	unlock()
	if table.Len() != 0 {
		t.Fatalf("Len() after unlock = %d, want 0", table.Len())
	}
}

// TestTableRetainsLockWhileHeld verifies Len reports a tracked actor while the
// actor lock is still held.
func TestTableRetainsLockWhileHeld(t *testing.T) {
	table := NewTable()
	unlock := table.Lock("actor-1")
	defer unlock()
	if table.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", table.Len())
	}
}

// TestTableConcurrentActorsIndependent verifies different actor IDs can acquire
// and release locks independently without leaking entries.
func TestTableConcurrentActorsIndependent(t *testing.T) {
	table := NewTable()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			actorID := fmt.Sprintf("actor-%d", id)
			unlock := table.Lock(actorID)
			unlock()
		}(i)
	}
	wg.Wait()
	if table.Len() != 0 {
		t.Fatalf("Len() after concurrent use = %d, want 0", table.Len())
	}
}

// TestTableSerializesConcurrentSameActor stresses contention on one actor ID and
// verifies only one goroutine enters that critical section at a time.
func TestTableSerializesConcurrentSameActor(t *testing.T) {
	table := NewTable()
	const goroutines = 64
	const iterations = 200

	start := make(chan struct{})
	var wg sync.WaitGroup
	var inCritical atomic.Int32
	var violations atomic.Int32

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < iterations; j++ {
				unlock := table.Lock("actor-1")
				if entered := inCritical.Add(1); entered != 1 {
					violations.Add(1)
				}

				// Yield inside the critical section to make accidental concurrent entry more
				// likely to surface during the stress loop.
				runtime.Gosched()
				inCritical.Add(-1)
				unlock()
			}
		}()
	}

	close(start)
	wg.Wait()
	if got := violations.Load(); got != 0 {
		t.Fatalf("same actor lock allowed %d concurrent critical sections", got)
	}
	if table.Len() != 0 {
		t.Fatalf("Len() after same-actor contention = %d, want 0", table.Len())
	}
}
