package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/objectstore"
)

type FunctionCache struct {
	mu      sync.RWMutex
	entries map[string]string
	store   objectstore.Store
}

func newFunctionCache(store objectstore.Store) *FunctionCache {
	return &FunctionCache{
		entries: make(map[string]string),
		store:   store,
	}
}

func (c *FunctionCache) SourceForTask(ctx context.Context, task *logservepb.TaskSpec) (string, error) {
	if task == nil {
		return "", errors.New("task is nil")
	}
	hash := task.GetFunctionHash()
	inline := task.GetFunctionSource()
	if hash == "" {
		return inline, nil
	}
	if inline != "" {
		if computed := hashFunctionSource(inline); computed != hash {
			return "", fmt.Errorf("function hash mismatch for %s: computed %s", hash, computed)
		}
		c.storeSource(hash, inline)
		return inline, nil
	}

	c.mu.RLock()
	cached, ok := c.entries[hash]
	c.mu.RUnlock()
	if ok {
		return cached, nil
	}
	ref := task.GetFunctionRef()
	if ref == "" {
		return "", fmt.Errorf("function source for %s is not cached and function_ref is empty", hash)
	}
	if c.store == nil {
		return "", errors.New("function object store is not configured")
	}
	data, err := objectstore.GetBytes(ctx, c.store, ref, -1)
	if err != nil {
		return "", err
	}
	source := string(data)
	if computed := hashFunctionSource(source); computed != hash {
		return "", fmt.Errorf("function object hash mismatch for %s: computed %s", hash, computed)
	}
	c.storeSource(hash, source)
	return source, nil
}

func (c *FunctionCache) storeSource(hash, source string) {
	if c == nil || hash == "" || source == "" {
		return
	}
	c.mu.Lock()
	c.entries[hash] = source
	c.mu.Unlock()
}

func hashFunctionSource(source string) string {
	sum := sha256.Sum256([]byte(source))
	return "sha256:" + hex.EncodeToString(sum[:])
}
