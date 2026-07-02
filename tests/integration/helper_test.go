package integration

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
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
		probeCtx, probeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		probeErr := exec.CommandContext(probeCtx, "python", "-c", "import msgpack").Run()
		probeTimedOut := probeCtx.Err() == context.DeadlineExceeded
		probeCancel()
		if probeErr == nil {
			return
		}
		if probeTimedOut {
			ensureExecutorDepsErr = fmt.Errorf("check executor python deps timed out: %w", context.DeadlineExceeded)
			return
		}

		root := repoRoot(t)
		req := filepath.Join(root, "executor", "python", "requirements.txt")
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "python", "-m", "pip", "install", "-q", "-r", req)
		ensureExecutorDepsErr = cmd.Run()
		if ctx.Err() == context.DeadlineExceeded {
			ensureExecutorDepsErr = fmt.Errorf("install executor python deps timed out: %w", ctx.Err())
		}
	})
	if ensureExecutorDepsErr != nil {
		t.Fatalf("install executor python deps: %v", ensureExecutorDepsErr)
	}
}
