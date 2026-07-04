package worker

// This file owns the worker-local cache for Python function source.
// It is the boundary between TaskSpec function hashes, inline submitted source,
// and optional object-store references produced by the control plane.

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

// FunctionCache resolves Python function source by content hash for worker execution.
// It keeps a small in-memory cache and optionally fetches missing source from the configured object store.
type FunctionCache struct {
	mu      sync.RWMutex
	entries map[string]string
	store   objectstore.Store
}

// newFunctionCache creates an empty function source cache backed by the provided object store.
func newFunctionCache(store objectstore.Store) *FunctionCache {
	return &FunctionCache{
		entries: make(map[string]string),
		store:   store,
	}
}

// SourceForTask returns verified Python source for a task.
// Inline source is hash-checked and cached; otherwise the hash is looked up in memory or fetched by FunctionRef from object storage.
func (c *FunctionCache) SourceForTask(ctx context.Context, task *logservepb.TaskSpec) (string, error) {
	if task == nil {
		return "", errors.New("task is nil")
	}
	hash := task.GetFunctionHash()
	inline := task.GetFunctionSource()
	if hash == "" {
		return inline, nil
	}

	// Inline source is trusted only after recomputing the declared content hash.
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

	// A nil object store is valid only when the source was already inline or cached locally.
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

// storeSource records verified non-empty source under its content hash.
func (c *FunctionCache) storeSource(hash, source string) {
	if c == nil || hash == "" || source == "" {
		return
	}
	c.mu.Lock()
	c.entries[hash] = source
	c.mu.Unlock()
}

// hashFunctionSource returns the canonical sha256-prefixed content hash used by task specs and object-store entries.
func hashFunctionSource(source string) string {
	sum := sha256.Sum256([]byte(source))
	return "sha256:" + hex.EncodeToString(sum[:])
}
