package integration

// This file contains shared test helpers for integration tests that need the
// repository root or the Python executor's runtime dependencies.

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

// ensureExecutorDepsOnce serializes the optional Python dependency installation
// across integration tests that may start workers concurrently.
var ensureExecutorDepsOnce sync.Once
var ensureExecutorDepsErr error

// repoRoot returns the repository root by walking up from this helper file. It
// fails the test immediately if runtime caller metadata is unavailable.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// ensureExecutorDeps makes the Python executor dependencies available for tests
// that start a real worker. It probes for msgpack first and installs from the
// checked-in requirements file only once per test process.
func ensureExecutorDeps(t *testing.T) {
	t.Helper()
	// sync.Once prevents parallel tests from racing on the same pip install and
	// keeps later tests from repeating the dependency probe.
	ensureExecutorDepsOnce.Do(func() {
		// Probe first so normal developer machines do not pay the cost of invoking
		// pip when the executor dependency is already installed.
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
		// Bound the install step; a stuck resolver should fail this test instead of
		// hanging the whole integration package.
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
