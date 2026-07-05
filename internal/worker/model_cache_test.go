package worker

// This file exercises the worker checkpoint cache, especially the LRU,
// singleflight, manifest-replay, and concurrent cold-load edge cases.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
)

// TestModelCheckpointCacheCopiesAndHits verifies a cold checkpoint fetch becomes a cache hit on the next load.
func TestModelCheckpointCacheCopiesAndHits(t *testing.T) {
	sourceDir := t.TempDir()
	cacheDir := t.TempDir()
	writeCheckpoint(t, sourceDir, "model-A", "v1", bytes.Repeat([]byte("a"), 1024))

	cache := newModelCache(Config{
		ModelCheckpointSourceDir: sourceDir,
		ModelCacheDir:            cacheDir,
		ModelCacheCapacityBytes:  4096,
	})

	first, err := cache.ensureCheckpoint(context.Background(), "model-A", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if first.CacheHit {
		t.Fatal("first checkpoint load reported cache hit")
	}
	if first.CheckpointFetchMs <= 0 || first.ModelLoadMs <= 0 {
		t.Fatalf("first load metrics missing: fetch=%d load=%d", first.CheckpointFetchMs, first.ModelLoadMs)
	}
	if !cache.has("model-A", "v1") {
		t.Fatal("cache did not record model-A after fetch")
	}

	second, err := cache.ensureCheckpoint(context.Background(), "model-A", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if !second.CacheHit {
		t.Fatal("second checkpoint load should be a cache hit")
	}
	if second.CheckpointFetchMs != 0 {
		t.Fatalf("cache hit fetch ms = %d, want 0", second.CheckpointFetchMs)
	}
	if second.ModelLoadMs <= 0 {
		t.Fatalf("cache hit load ms = %d, want > 0", second.ModelLoadMs)
	}
}

// TestModelCheckpointCacheEvictsLRUWhenCapacityExceeded verifies capacity pressure removes the least-recently-used checkpoint.
func TestModelCheckpointCacheEvictsLRUWhenCapacityExceeded(t *testing.T) {
	sourceDir := t.TempDir()
	cacheDir := t.TempDir()
	writeCheckpoint(t, sourceDir, "model-A", "v1", bytes.Repeat([]byte("a"), 8))
	writeCheckpoint(t, sourceDir, "model-B", "v1", bytes.Repeat([]byte("b"), 8))

	cache := newModelCache(Config{
		ModelCheckpointSourceDir: sourceDir,
		ModelCacheDir:            cacheDir,
		ModelCacheCapacityBytes:  12,
	})
	if _, err := cache.ensureCheckpoint(context.Background(), "model-A", "v1"); err != nil {
		t.Fatal(err)
	}
	second, err := cache.ensureCheckpoint(context.Background(), "model-B", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if second.EvictionCount != 1 {
		t.Fatalf("eviction count = %d, want 1", second.EvictionCount)
	}
	if cache.has("model-A", "v1") {
		t.Fatal("model-A should have been evicted")
	}
	if !cache.has("model-B", "v1") {
		t.Fatal("model-B should be cached")
	}
	if _, err := os.Stat(cache.checkpointPath("model-A", "v1")); !os.IsNotExist(err) {
		t.Fatalf("model-A checkpoint still exists or stat failed with unexpected error: %v", err)
	}
}

// TestModelCheckpointCacheAccessPromotesLRU verifies cache hits promote entries before later evictions.
func TestModelCheckpointCacheAccessPromotesLRU(t *testing.T) {
	sourceDir := t.TempDir()
	cacheDir := t.TempDir()
	writeCheckpoint(t, sourceDir, "model-A", "v1", bytes.Repeat([]byte("a"), 8))
	writeCheckpoint(t, sourceDir, "model-B", "v1", bytes.Repeat([]byte("b"), 8))
	writeCheckpoint(t, sourceDir, "model-C", "v1", bytes.Repeat([]byte("c"), 8))

	cache := newModelCache(Config{
		ModelCheckpointSourceDir: sourceDir,
		ModelCacheDir:            cacheDir,
		ModelCacheCapacityBytes:  16,
	})
	if _, err := cache.ensureCheckpoint(context.Background(), "model-A", "v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.ensureCheckpoint(context.Background(), "model-B", "v1"); err != nil {
		t.Fatal(err)
	}
	if cache.lru.Front().Value.(*cacheEntry).key != modelKey("model-B", "v1") {
		t.Fatalf("front LRU entry = %s, want model-B:v1", cache.lru.Front().Value.(*cacheEntry).key)
	}

	hit, err := cache.ensureCheckpoint(context.Background(), "model-A", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if !hit.CacheHit {
		t.Fatal("reloading model-A should be a cache hit")
	}
	if cache.lru.Front().Value.(*cacheEntry).key != modelKey("model-A", "v1") {
		t.Fatalf("front LRU entry after access = %s, want model-A:v1", cache.lru.Front().Value.(*cacheEntry).key)
	}

	evict, err := cache.ensureCheckpoint(context.Background(), "model-C", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if evict.EvictionCount != 1 {
		t.Fatalf("eviction count = %d, want 1", evict.EvictionCount)
	}
	if !cache.has("model-A", "v1") {
		t.Fatal("recently accessed model-A should remain cached")
	}
	if cache.has("model-B", "v1") {
		t.Fatal("least recently used model-B should have been evicted")
	}
}

// TestModelCheckpointCacheUsesO1LRUIndex verifies the cache has both list and map structures needed for O(1) LRU operations.
func TestModelCheckpointCacheUsesO1LRUIndex(t *testing.T) {
	cache := newModelCache(Config{ModelCacheDir: t.TempDir(), ModelCacheCapacityBytes: 1024})
	if cache.lru == nil {
		t.Fatal("model cache LRU list is nil")
	}
	if cache.entries == nil {
		t.Fatal("model cache entries map is nil")
	}
	if cache.inflight == nil {
		t.Fatal("model cache inflight map is nil")
	}
}

// TestModelCheckpointCacheAllowsDifferentModelsToLoadConcurrently verifies per-model singleflight does not serialize unrelated models.
func TestModelCheckpointCacheAllowsDifferentModelsToLoadConcurrently(t *testing.T) {
	sourceDir := t.TempDir()
	cacheDir := t.TempDir()
	writeCheckpoint(t, sourceDir, "model-A", "v1", bytes.Repeat([]byte("a"), 8))
	writeCheckpoint(t, sourceDir, "model-B", "v1", bytes.Repeat([]byte("b"), 8))

	cache := newModelCache(Config{
		ModelCheckpointSourceDir: sourceDir,
		ModelCacheDir:            cacheDir,
		ModelCacheCapacityBytes:  32,
	})

	originalCopy := copyCheckpointFunc
	originalRead := readCheckpointFunc
	// Buffered start notifications let both copy goroutines report progress before
	// the test releases either one; an unbuffered channel could accidentally serialize the test.
	copyStarted := make(chan string, 2)
	releaseCopies := make(chan struct{})

	// Patch checkpoint I/O so the test can hold copies open and prove both model loads start concurrently.
	var activeCopies atomic.Int32
	copyCheckpointFunc = func(ctx context.Context, sourcePath, targetPath string) (int64, error) {
		activeCopies.Add(1)
		copyStarted <- filepath.Base(targetPath)
		select {
		case <-releaseCopies:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			return 0, err
		}
		if err := os.WriteFile(targetPath, []byte(filepath.Base(sourcePath)), 0o644); err != nil {
			return 0, err
		}
		return 1, nil
	}
	readCheckpointFunc = func(context.Context, string) (int64, error) {
		return 1, nil
	}
	defer func() {
		copyCheckpointFunc = originalCopy
		readCheckpointFunc = originalRead
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, name := range []string{"model-A", "model-B"} {
		name := name
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := cache.ensureCheckpoint(ctx, name, "v1")
			if err != nil {
				errs <- err
				return
			}
			if result.CacheHit {
				errs <- errUnexpectedCacheHit(name)
			}
		}()
	}

	started := map[string]bool{}
	// A short timeout catches an accidental global load lock while keeping the test
	// bounded if a goroutine fails before signaling copyStarted.
	timeout := time.After(500 * time.Millisecond)
	for len(started) < 2 {
		select {
		case checkpoint := <-copyStarted:
			started[checkpoint] = true
		case <-timeout:
			close(releaseCopies)
			wg.Wait()
			t.Fatalf("only %d checkpoint copy started with %d active copies; different model loads should not wait on one global lock", len(started), activeCopies.Load())
		}
	}
	close(releaseCopies)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestModelCheckpointCacheSnapshotDoesNotWaitForColdLoadIO verifies heartbeats can snapshot warm models while a cold load blocks on I/O.
func TestModelCheckpointCacheSnapshotDoesNotWaitForColdLoadIO(t *testing.T) {
	sourceDir := t.TempDir()
	cacheDir := t.TempDir()
	writeCheckpoint(t, sourceDir, "model-A", "v1", bytes.Repeat([]byte("a"), 8))

	cache := newModelCache(Config{
		ModelCheckpointSourceDir: sourceDir,
		ModelCacheDir:            cacheDir,
		ModelCacheCapacityBytes:  32,
		CachedModels:             []string{"prewarmed:v1"},
	})

	originalCopy := copyCheckpointFunc
	originalRead := readCheckpointFunc

	// The patched copy blocks after signaling so snapshotEntries can be checked during cold-load I/O.
	copyStarted := make(chan struct{})
	releaseCopy := make(chan struct{})
	copyCheckpointFunc = func(ctx context.Context, sourcePath, targetPath string) (int64, error) {
		close(copyStarted)
		select {
		case <-releaseCopy:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			return 0, err
		}
		if err := os.WriteFile(targetPath, []byte("checkpoint"), 0o644); err != nil {
			return 0, err
		}
		return 1, nil
	}
	readCheckpointFunc = func(context.Context, string) (int64, error) {
		return 1, nil
	}
	defer func() {
		copyCheckpointFunc = originalCopy
		readCheckpointFunc = originalRead
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	loadDone := make(chan error, 1)
	go func() {
		_, err := cache.ensureCheckpoint(ctx, "model-A", "v1")
		loadDone <- err
	}()

	select {
	case <-copyStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("checkpoint copy did not start")
	}

	// Buffer the result so the goroutine can finish even if the timeout branch wins.
	snapshotDone := make(chan []*logservepb.ModelCacheEntry, 1)
	go func() {
		snapshotDone <- cache.snapshotEntries()
	}()
	select {
	case entries := <-snapshotDone:
		if !cacheEntriesContain(entries, "prewarmed", "v1") {
			t.Fatalf("snapshot entries = %v, want prewarmed:v1 while checkpoint copy is blocked", entries)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("snapshotEntries waited for cold-load I/O")
	}

	close(releaseCopy)
	if err := <-loadDone; err != nil {
		t.Fatal(err)
	}
}

// TestWriteCheckpointManifestRewritesThroughTempFile verifies manifest rewrites leave no temp file and publish the newest metadata.
func TestWriteCheckpointManifestRewritesThroughTempFile(t *testing.T) {
	dir := t.TempDir()
	checkpointPath := filepath.Join(dir, "model.checkpoint")
	if err := os.WriteFile(checkpointPath, []byte("checkpoint"), 0o644); err != nil {
		t.Fatal(err)
	}

	firstAccess := time.UnixMilli(1000)
	if err := writeCheckpointManifest(checkpointPath, "model-A", "v1", 8, firstAccess); err != nil {
		t.Fatal(err)
	}
	secondAccess := time.UnixMilli(2000)
	if err := writeCheckpointManifest(checkpointPath, "model-A", "v1", 9, secondAccess); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(checkpointManifestPath(checkpointPath) + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("manifest temp file still exists or stat failed with unexpected error: %v", err)
	}

	data, err := os.ReadFile(checkpointManifestPath(checkpointPath))
	if err != nil {
		t.Fatal(err)
	}
	var manifest modelCacheManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SizeBytes != 9 || manifest.LastAccessMs != secondAccess.UnixMilli() {
		t.Fatalf("manifest = %+v, want rewritten size/access", manifest)
	}
}

// TestModelCheckpointCacheReportsExistingCheckpointOnStartup verifies manifest replay advertises cached checkpoints after restart.
func TestModelCheckpointCacheReportsExistingCheckpointOnStartup(t *testing.T) {
	sourceDir := t.TempDir()
	cacheDir := t.TempDir()
	writeCheckpoint(t, sourceDir, "model-A", "v1", bytes.Repeat([]byte("a"), 128))

	cache := newModelCache(Config{
		ModelCheckpointSourceDir: sourceDir,
		ModelCacheDir:            cacheDir,
		ModelCacheCapacityBytes:  1024,
	})
	if _, err := cache.ensureCheckpoint(context.Background(), "model-A", "v1"); err != nil {
		t.Fatal(err)
	}

	restarted := newModelCache(Config{
		ModelCheckpointSourceDir: sourceDir,
		ModelCacheDir:            cacheDir,
		ModelCacheCapacityBytes:  1024,
	})
	if !cacheEntriesContain(restarted.snapshotEntries(), "model-A", "v1") {
		t.Fatalf("restarted cache entries = %v, want model-A:v1", restarted.snapshotEntries())
	}
}

// TestModelCheckpointCacheSerializesConcurrentColdLoads verifies same-model callers share one cold fetch and the rest observe hits.
func TestModelCheckpointCacheSerializesConcurrentColdLoads(t *testing.T) {
	sourceDir := t.TempDir()
	cacheDir := t.TempDir()
	checkpointData := bytes.Repeat([]byte("z"), 2<<20)
	writeCheckpoint(t, sourceDir, "model-A", "v1", checkpointData)

	cache := newModelCache(Config{
		ModelCheckpointSourceDir: sourceDir,
		ModelCacheDir:            cacheDir,
		ModelCacheCapacityBytes:  int64(len(checkpointData) * 2),
	})

	const callers = 12
	start := make(chan struct{})
	results := make(chan checkpointLoadResult, callers)
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := cache.ensureCheckpoint(context.Background(), "model-A", "v1")
			if err != nil {
				errs <- err
				return
			}
			results <- result
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatal(err)
	}

	var misses, hits int
	for result := range results {
		if result.CacheHit {
			hits++
		} else {
			misses++
		}
	}
	if misses != 1 || hits != callers-1 {
		t.Fatalf("concurrent cold loads misses/hits = %d/%d, want 1/%d", misses, hits, callers-1)
	}
	info, err := os.Stat(cache.checkpointPath("model-A", "v1"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len(checkpointData)) {
		t.Fatalf("checkpoint size = %d, want %d", info.Size(), len(checkpointData))
	}
}

// writeCheckpoint creates a source checkpoint fixture in the directory layout sourcePath expects.
func writeCheckpoint(t *testing.T, root, name, version string, data []byte) {
	t.Helper()
	dir := filepath.Join(root, name+"-"+version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "checkpoint.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// cacheEntriesContain reports whether a snapshot includes a model/version entry.
func cacheEntriesContain(entries []*logservepb.ModelCacheEntry, name, version string) bool {
	for _, entry := range entries {
		if entry.GetName() == name && entry.GetVersion() == version {
			return true
		}
	}
	return false
}

// errUnexpectedCacheHit builds a typed test error for concurrent-load assertions.
func errUnexpectedCacheHit(name string) error {
	return &unexpectedCacheHitError{name: name}
}

// unexpectedCacheHitError preserves the model name in cache-hit assertion failures.
type unexpectedCacheHitError struct {
	name string
}

// Error returns the unexpected-cache-hit test failure text.
func (e *unexpectedCacheHitError) Error() string {
	return e.name + " unexpectedly reported a cache hit"
}
