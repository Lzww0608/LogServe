package worker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/logserve/logserve/gen/logservepb"
)

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
	if !cacheEntriesContain(restarted.entries(), "model-A", "v1") {
		t.Fatalf("restarted cache entries = %v, want model-A:v1", restarted.entries())
	}
}

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

func cacheEntriesContain(entries []*logservepb.ModelCacheEntry, name, version string) bool {
	for _, entry := range entries {
		if entry.GetName() == name && entry.GetVersion() == version {
			return true
		}
	}
	return false
}
