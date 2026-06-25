package actorlock

import (
	"fmt"
	"sync"
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
