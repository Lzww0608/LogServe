package integration

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

var ensureExecutorDepsOnce sync.Once
var ensureExecutorDepsErr error

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func ensureExecutorDeps(t *testing.T) {
	t.Helper()
	ensureExecutorDepsOnce.Do(func() {
		root := repoRoot(t)
		req := filepath.Join(root, "executor", "python", "requirements.txt")
		cmd := exec.Command("python", "-m", "pip", "install", "-q", "-r", req)
		ensureExecutorDepsErr = cmd.Run()
	})
	if ensureExecutorDepsErr != nil {
		t.Fatalf("install executor python deps: %v", ensureExecutorDepsErr)
	}
}
