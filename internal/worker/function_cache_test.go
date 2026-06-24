package worker

import (
	"context"
	"testing"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/objectstore"
)

func TestFunctionCacheLoadsVerifiesAndCachesSource(t *testing.T) {
	ctx := context.Background()
	store, err := objectstore.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := "def add(a, b):\n    return a + b\n"
	hash := hashFunctionSource(source)
	ref, err := objectstore.PutBytes(ctx, store, "functions", []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	cache := newFunctionCache(store)
	task := &logservepb.TaskSpec{FunctionHash: hash, FunctionRef: ref}

	loaded, err := cache.SourceForTask(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if loaded != source {
		t.Fatalf("loaded source = %q, want original", loaded)
	}

	// The second lookup should use the in-memory worker cache even if the task no longer carries a ref.
	cached, err := cache.SourceForTask(ctx, &logservepb.TaskSpec{FunctionHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	if cached != source {
		t.Fatalf("cached source = %q, want original", cached)
	}
}

func TestFunctionCacheRejectsHashMismatch(t *testing.T) {
	ctx := context.Background()
	store, err := objectstore.OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := objectstore.PutBytes(ctx, store, "functions", []byte("def add(a, b):\n    return a + b\n"))
	if err != nil {
		t.Fatal(err)
	}
	cache := newFunctionCache(store)
	_, err = cache.SourceForTask(ctx, &logservepb.TaskSpec{FunctionHash: "sha256:not-source", FunctionRef: ref})
	if err == nil {
		t.Fatal("SourceForTask succeeded with mismatched function hash")
	}
}
