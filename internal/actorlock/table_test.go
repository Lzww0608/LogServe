package actorlock

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

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

func TestTableRetainsLockWhileHeld(t *testing.T) {
	table := NewTable()
	unlock := table.Lock("actor-1")
	defer unlock()
	if table.Len() != 1 {
		t.Fatalf("Len() = %d, want 1", table.Len())
	}
}

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
