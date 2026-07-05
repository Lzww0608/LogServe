// Package worker implements the LogServe worker process runtime.
// It polls the control plane, executes Python, actor, and LLM tasks, and reports completions while maintaining local function and model caches.
package worker

// This file contains the worker runtime loop, executor pool, Python subprocess
// protocol, LLM adapters, and model checkpoint cache implementation.

import (
	"bufio"
	"bytes"
	"container/list"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/actorlock"
	"github.com/logserve/logserve/internal/eventcodec"
	"github.com/logserve/logserve/internal/objectstore"
	"github.com/logserve/logserve/internal/observability"
	"github.com/logserve/logserve/internal/rpcauth"
	"github.com/vmihailenco/msgpack/v5"
	"google.golang.org/grpc"
)

// Config carries runtime settings for Run.
// Zero values select worker defaults for identifiers, executor paths, polling, capacity, and mock LLM timings.
type Config struct {
	// WorkerID is the identity registered with the control plane; empty uses a local default.
	WorkerID string
	// ControlAddr and LogAddr are the gRPC endpoints used for task scheduling and event appends.
	ControlAddr string
	LogAddr     string
	// APIToken is optional bearer metadata for internal gRPC calls; empty falls back to LOGSERVE_API_TOKEN.
	APIToken string
	// PythonPath and ExecutorPath identify the Python interpreter and executor loop script.
	PythonPath   string
	ExecutorPath string
	// PollInterval bounds both empty-poll retry cadence and idle long-poll wait time.
	PollInterval time.Duration
	// HeartbeatInterval controls how often cached model state is re-advertised.
	HeartbeatInterval time.Duration
	// MaxTasks is a test/dev stop condition; zero means run until ctx is cancelled.
	MaxTasks int
	// CachedModels seeds the advertised warm-model set before any local LLM task runs.
	CachedModels []string
	// Capacity is the total local task concurrency advertised to the scheduler.
	Capacity uint32
	// TaskPoolSize, LLMPoolSize, and ActorPoolSize split local capacity by execution class.
	TaskPoolSize  int
	LLMPoolSize   int
	ActorPoolSize int
	// MockModelLoad and MockFirstToken make mock LLM latency visible in replayable event logs.
	MockModelLoad  time.Duration
	MockFirstToken time.Duration
	// VLLMBaseURL enables the vLLM adapter; empty falls back to LOGSERVE_VLLM_BASE_URL.
	VLLMBaseURL string
	// ModelCheckpointSourceDir and ModelCacheDir enable disk-backed checkpoint caching when both are set.
	ModelCheckpointSourceDir string
	ModelCacheDir            string
	// ModelCacheCapacityBytes caps on-disk checkpoint bytes; zero disables eviction by size.
	ModelCacheCapacityBytes int64
}

// executorRequest is the request envelope sent to the Python executor for stateless function calls.
type executorRequest struct {
	// FunctionSource is included only until a runner has learned FunctionHash.
	FunctionSource string `json:"function_source,omitempty"`
	// FunctionRef is retained for compatibility with JSON execution and debugging, but Go resolves refs first.
	FunctionRef string `json:"function_ref,omitempty"`
	// FunctionHash is the content identity shared with the function cache and Python runner.
	FunctionHash string `json:"function_hash,omitempty"`
	FunctionName string `json:"function_name"`
	// ArgsJSON remains raw JSON so Python observes the same submitted envelope.
	ArgsJSON json.RawMessage `json:"args_json"`
}

// executorResponse is the executor reply shape shared by JSON and msgpack protocols.
type executorResponse struct {
	OK bool `json:"ok"`
	// Result and State are raw JSON documents, not decoded Go values.
	Result json.RawMessage `json:"result,omitempty"`
	State  json.RawMessage `json:"state,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// actorExecutorRequest is the Python executor envelope for actor method calls.
// It carries both the input state and the init arguments needed when the actor is first materialized.
type actorExecutorRequest struct {
	Mode        string `json:"mode"`
	ClassSource string `json:"class_source"`
	ClassName   string `json:"class_name"`
	MethodName  string `json:"method_name"`
	// ArgsJSON, StateJSON, and InitArgsJSON are passed through as JSON bytes to preserve actor semantics in Python.
	ArgsJSON     json.RawMessage `json:"args_json"`
	StateJSON    json.RawMessage `json:"state_json"`
	InitArgsJSON json.RawMessage `json:"init_args_json"`
}

// llmArgsPayload mirrors the JSON args envelope used by control-plane task submission for LLM prompts.
type llmArgsPayload struct {
	Args   []json.RawMessage          `json:"args"`
	Kwargs map[string]json.RawMessage `json:"kwargs"`
}

// llmEventPayload is appended to the per-task LLM log stream to make cache and latency behavior replayable.
type llmEventPayload struct {
	TaskID             string `json:"task_id,omitempty"`
	ModelName          string `json:"model_name,omitempty"`
	ModelVersion       string `json:"model_version,omitempty"`
	WorkerID           string `json:"worker_id,omitempty"`
	CacheHit           bool   `json:"cache_hit,omitempty"`
	CheckpointFetchMs  int64  `json:"checkpoint_fetch_ms,omitempty"`
	CacheUsedBytes     int64  `json:"cache_used_bytes,omitempty"`
	CacheCapacityBytes int64  `json:"cache_capacity_bytes,omitempty"`
	EvictionCount      int64  `json:"eviction_count,omitempty"`
	ModelLoadMs        int64  `json:"model_load_ms,omitempty"`
	FirstTokenMs       int64  `json:"first_token_ms,omitempty"`
	TotalLatencyMs     int64  `json:"total_latency_ms,omitempty"`
	TimestampMs        int64  `json:"timestamp_ms,omitempty"`
}

// Executor protocol constants bound the local Python subprocess wire format and frame sizes.
const (
	executorProtocolJSON    = "json"
	executorProtocolMsgpack = "msgpack"
	maxExecutorFrameBytes   = 16 << 20
	maxPooledExecutorFrame  = 4 << 20
)

// executorFrameBufferPool reuses medium-sized msgpack frames without retaining very large allocations forever.
var executorFrameBufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 0, 64*1024)
		return &buf
	},
}

// pythonRunner owns one long-lived Python executor subprocess.
// Calls are serialized through mu because the executor protocol is request-response over a single stdin/stdout pair.
type pythonRunner struct {
	cmd            *exec.Cmd
	stdin          io.WriteCloser
	stdout         *bufio.Reader
	protocol       string
	stderr         *lockedBuffer
	mu             sync.Mutex
	knownFunctions map[string]struct{}
}

// lockedBuffer captures subprocess stderr while allowing readResponseLocked to read it from another goroutine safely.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends stderr bytes under a mutex so executor failure diagnostics are not raced.
func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// Len returns the buffered stderr size under the same lock used by Write.
func (b *lockedBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

// String returns the current stderr text for error reporting.
func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// modelCache tracks warmed LLM models and optional on-disk checkpoint files.
// The disk-backed path uses an LRU list plus a map for O(1) promotion and eviction.
type modelCache struct {
	mu sync.Mutex
	// models is the scheduler-visible warm set; disk entries are mirrored here after validation.
	models map[string]bool
	// entries and lru track disk-backed checkpoints with O(1) lookup and promotion.
	entries map[string]*list.Element
	lru     *list.List
	// inflight is a per-model singleflight table, intentionally not a global load lock.
	inflight      map[string]*loadCall
	sourceDir     string
	cacheDir      string
	capacityBytes int64
	usedBytes     int64
}

// cacheEntry records one checkpoint file in the worker-local model cache.
type cacheEntry struct {
	key  string
	path string
	size int64
	// lastAccess is stored as Unix milliseconds so manifests are stable JSON across platforms.
	lastAccess int64
}

// loadCall coordinates concurrent cold loads for the same model checkpoint.
type loadCall struct {
	done   chan struct{}
	result checkpointLoadResult
	err    error
}

// modelCacheManifest persists enough metadata to rebuild the checkpoint LRU after a worker restart.
type modelCacheManifest struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	CheckpointFile string `json:"checkpoint_file"`
	SizeBytes      int64  `json:"size_bytes"`
	LastAccessMs   int64  `json:"last_access_ms"`
}

// checkpointLoadResult reports whether a model load hit cache and how much local cache work it performed.
type checkpointLoadResult struct {
	CacheHit           bool
	CheckpointFetchMs  int64
	ModelLoadMs        int64
	CacheUsedBytes     int64
	CacheCapacityBytes int64
	EvictionCount      int64
}

// workerJob is the internal queue item handed from the polling loop to an executor pool.
type workerJob struct {
	task       *logservepb.TaskSpec
	enqueuedAt time.Time
}

// workerJobResult carries an executor completion back to the main Run loop for batched acknowledgement.
type workerJobResult struct {
	task       *logservepb.TaskSpec
	completion *logservepb.CompleteTaskRequest
	err        error
}

// localExecutorPool separates regular Python tasks, actor tasks, and LLM tasks into dedicated queues.
// Actor tasks share an actor lock table so calls for the same actor are serialized even with multiple actor runners.
type localExecutorPool struct {
	cfg           Config
	cache         *modelCache
	functionCache *FunctionCache
	controlClient logservepb.ControlServiceClient
	logClient     logservepb.LogServiceClient
	// Separate queues prevent LLM model loads and actor serialization from starving normal Python calls.
	taskQueue  chan workerJob
	llmQueue   chan workerJob
	actorQueue chan workerJob
	// results is consumed only by Run, which owns in-flight counters and completion batching.
	results    chan workerJobResult
	actorLocks *actorlock.Table
	closeOnce  sync.Once
	wg         sync.WaitGroup
}

// Run starts the worker process loop.
// It registers with the control plane, heartbeats cached model state, polls for tasks up to local capacity, and batches task completions back to the control plane.
func Run(ctx context.Context, cfg Config) error {
	// Normalize process defaults in one place so command entrypoints can pass a
	// sparse Config while tests can override only the dimensions they need.
	if cfg.WorkerID == "" {
		cfg.WorkerID = "worker-1"
	}
	if cfg.PythonPath == "" {
		cfg.PythonPath = "python"
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 200 * time.Millisecond
	}
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = time.Second
	}
	if cfg.ExecutorPath == "" {
		cfg.ExecutorPath = filepath.Join("executor", "python", "server.py")
	}
	if cfg.Capacity == 0 {
		cfg.Capacity = 1
	}
	if cfg.TaskPoolSize <= 0 {
		cfg.TaskPoolSize = int(cfg.Capacity)
	}
	if cfg.LLMPoolSize <= 0 {
		cfg.LLMPoolSize = int(cfg.Capacity)
	}
	if cfg.ActorPoolSize <= 0 {
		cfg.ActorPoolSize = int(cfg.Capacity)
	}
	if cfg.MockModelLoad == 0 {
		cfg.MockModelLoad = 80 * time.Millisecond
	}
	if cfg.MockFirstToken == 0 {
		cfg.MockFirstToken = 15 * time.Millisecond
	}
	if cfg.VLLMBaseURL == "" {
		cfg.VLLMBaseURL = os.Getenv("LOGSERVE_VLLM_BASE_URL")
	}
	if cfg.APIToken == "" {
		cfg.APIToken = os.Getenv(rpcauth.EnvAPIToken)
	}

	controlConn, err := grpc.NewClient(cfg.ControlAddr, rpcauth.InsecureDialOptions(cfg.APIToken)...)
	if err != nil {
		return err
	}
	defer controlConn.Close()
	logConn, err := grpc.NewClient(cfg.LogAddr, rpcauth.InsecureDialOptions(cfg.APIToken)...)
	if err != nil {
		return err
	}
	defer logConn.Close()

	controlClient := logservepb.NewControlServiceClient(controlConn)
	logClient := logservepb.NewLogServiceClient(logConn)
	cache := newModelCache(cfg)
	// Function source may be sent inline, cached locally, or fetched lazily from
	// the object store when the control plane sends only a function_ref.
	functionStore, err := objectstore.OpenFromEnv(ctx)
	if err != nil {
		return err
	}

	if _, err := controlClient.RegisterWorker(ctx, &logservepb.RegisterWorkerRequest{
		WorkerId:     cfg.WorkerID,
		CachedModels: cache.snapshotEntries(),
		Capacity:     cfg.Capacity,
	}); err != nil {
		return err
	}
	observability.Info("worker_registered", map[string]any{"worker_id": cfg.WorkerID})

	pool, err := startLocalExecutorPool(ctx, cfg, cache, newFunctionCache(functionStore), controlClient, logClient)
	if err != nil {
		return err
	}
	defer pool.Close()

	heartbeatTicker := time.NewTicker(cfg.HeartbeatInterval)
	defer heartbeatTicker.Stop()

	// The zero-delay timer triggers an immediate first poll; resetTimer handles the stopped-timer drain edge case after that.
	pollTimer := time.NewTimer(0)
	defer pollTimer.Stop()

	completedTasks := 0
	dispatchedTasks := 0
	inFlight := 0
	localCapacity := int(cfg.Capacity)
	if localCapacity <= 0 {
		localCapacity = 1
	}
	pendingCompletions := make([]*logservepb.CompleteTaskRequest, 0, localCapacity)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case result := <-pool.results:
			collectWorkerResult(cfg, result, &inFlight, &completedTasks, &pendingCompletions)

			// Drain already-finished jobs so one wakeup can produce a single CompleteTasks batch.
			drainWorkerResults(cfg, pool.results, &inFlight, &completedTasks, &pendingCompletions)
			flushCompletions(ctx, cfg.WorkerID, controlClient, pendingCompletions)
			pendingCompletions = pendingCompletions[:0]
			if cfg.MaxTasks > 0 && completedTasks >= cfg.MaxTasks {
				return nil
			}
			if inFlight < localCapacity {
				resetTimer(pollTimer, 0)
			}
		case <-heartbeatTicker.C:
			if _, err := controlClient.Heartbeat(ctx, &logservepb.HeartbeatRequest{
				WorkerId:     cfg.WorkerID,
				CachedModels: cache.snapshotEntries(),
			}); err != nil {
				observability.Error("worker_heartbeat_failed", err, map[string]any{"worker_id": cfg.WorkerID})
			}
		case <-pollTimer.C:
			if inFlight >= localCapacity || (cfg.MaxTasks > 0 && dispatchedTasks >= cfg.MaxTasks) {
				resetTimer(pollTimer, cfg.PollInterval)
				continue
			}
			idleCapacity := localCapacity - inFlight
			if cfg.MaxTasks > 0 {
				remaining := cfg.MaxTasks - dispatchedTasks
				if remaining < idleCapacity {
					idleCapacity = remaining
				}
			}
			if idleCapacity <= 0 {
				resetTimer(pollTimer, cfg.PollInterval)
				continue
			}

			// Long-poll only when this worker is otherwise idle; active local work should keep the loop responsive.
			waitMs := int64(0)
			if inFlight == 0 {
				waitMs = cfg.PollInterval.Milliseconds()
			}
			resp, err := controlClient.PollTask(ctx, &logservepb.PollTaskRequest{
				WorkerId:      cfg.WorkerID,
				MaxTasks:      uint32(idleCapacity),
				WaitTimeoutMs: waitMs,
			})
			if err != nil {
				observability.Error("worker_poll_failed", err, map[string]any{"worker_id": cfg.WorkerID})
				resetTimer(pollTimer, cfg.PollInterval)
				continue
			}
			tasks := pollTasks(resp)
			if len(tasks) == 0 {
				if inFlight == 0 {
					// PollTask already waited up to PollInterval when idle, so the next poll can start immediately.
					resetTimer(pollTimer, 0)
				} else {
					resetTimer(pollTimer, cfg.PollInterval)
				}
				continue
			}
			for _, task := range tasks {
				if task == nil || inFlight >= localCapacity || (cfg.MaxTasks > 0 && dispatchedTasks >= cfg.MaxTasks) {
					break
				}
				if err := pool.Dispatch(ctx, task); err != nil {
					observability.Error("worker_dispatch_failed", err, map[string]any{"worker_id": cfg.WorkerID, "task_id": task.GetTaskId()})
					break
				}
				inFlight++
				dispatchedTasks++
			}
			if inFlight < localCapacity && (cfg.MaxTasks == 0 || dispatchedTasks < cfg.MaxTasks) {
				resetTimer(pollTimer, 0)
			} else {
				resetTimer(pollTimer, cfg.PollInterval)
			}
		}
	}
}

// resetTimer safely reuses a time.Timer by draining a pending tick before Reset.
func resetTimer(timer *time.Timer, delay time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(delay)
}

// pollTasks normalizes both batch PollTask responses and the older single-task response fields.
func pollTasks(resp *logservepb.PollTaskResponse) []*logservepb.TaskSpec {
	if resp == nil {
		return nil
	}
	if tasks := resp.GetTasks(); len(tasks) > 0 {
		return tasks
	}
	if resp.GetHasTask() && resp.GetTask() != nil {
		return []*logservepb.TaskSpec{resp.GetTask()}
	}
	return nil
}

// collectWorkerResult updates main-loop counters and buffers an accepted completion for batch submission.
func collectWorkerResult(cfg Config, result workerJobResult, inFlight *int, completedTasks *int, pendingCompletions *[]*logservepb.CompleteTaskRequest) {
	if *inFlight > 0 {
		*inFlight = *inFlight - 1
	}
	// MaxTasks counts local execution attempts, including failures that never produce an accepted completion.
	*completedTasks = *completedTasks + 1
	if result.completion != nil {
		*pendingCompletions = append(*pendingCompletions, result.completion)
	}
	if result.task == nil {
		return
	}
	if result.err != nil {
		observability.Error("task_execution_failed", result.err, map[string]any{"worker_id": cfg.WorkerID, "task_id": result.task.GetTaskId()})
	}
}

// drainWorkerResults opportunistically consumes all currently-ready executor results without blocking.
func drainWorkerResults(cfg Config, results <-chan workerJobResult, inFlight *int, completedTasks *int, pendingCompletions *[]*logservepb.CompleteTaskRequest) {
	for {
		select {
		case result := <-results:
			collectWorkerResult(cfg, result, inFlight, completedTasks, pendingCompletions)
		default:
			return
		}
	}
}

// flushCompletions sends a batch acknowledgement to the control plane and logs per-task rejections.
func flushCompletions(ctx context.Context, workerID string, controlClient logservepb.ControlServiceClient, completions []*logservepb.CompleteTaskRequest) {
	if len(completions) == 0 {
		return
	}
	resp, err := controlClient.CompleteTasks(ctx, &logservepb.CompleteTaskBatchRequest{Tasks: completions})
	if err != nil {
		observability.Error("worker_complete_batch_failed", err, map[string]any{"worker_id": workerID, "count": len(completions)})
		return
	}
	for _, result := range resp.GetResults() {
		if result.GetAccepted() {
			continue
		}
		errText := result.GetError()
		if errText == "" {
			errText = "completion was rejected"
		}
		observability.Error("worker_complete_failed", errors.New(errText), map[string]any{"worker_id": workerID, "task_id": result.GetTaskId()})
	}
}

// startLocalExecutorPool creates dedicated executor workers for Python functions, actors, and LLM requests.
// If any Python subprocess fails to start, already-started subprocesses are closed before returning the error.
func startLocalExecutorPool(ctx context.Context, cfg Config, cache *modelCache, functionCache *FunctionCache, controlClient logservepb.ControlServiceClient, logClient logservepb.LogServiceClient) (*localExecutorPool, error) {
	queueSize := positiveInt(int(cfg.Capacity), 1)
	taskPoolSize := positiveInt(cfg.TaskPoolSize, 1)
	llmPoolSize := positiveInt(cfg.LLMPoolSize, 1)
	actorPoolSize := positiveInt(cfg.ActorPoolSize, 1)

	taskRunners := make([]*pythonRunner, 0, taskPoolSize)
	actorRunners := make([]*pythonRunner, 0, actorPoolSize)
	for i := 0; i < taskPoolSize; i++ {
		runner, err := startPythonRunner(ctx, cfg)
		if err != nil {
			closeRunners(taskRunners)
			return nil, err
		}
		taskRunners = append(taskRunners, runner)
	}
	// Actor jobs use separate subprocesses because their adapter protocol carries
	// state and init args, and per-actor locks can otherwise hold regular tasks behind them.
	for i := 0; i < actorPoolSize; i++ {
		runner, err := startPythonRunner(ctx, cfg)
		if err != nil {
			closeRunners(taskRunners)
			closeRunners(actorRunners)
			return nil, err
		}
		actorRunners = append(actorRunners, runner)
	}

	pool := &localExecutorPool{
		cfg:           cfg,
		cache:         cache,
		functionCache: functionCache,
		controlClient: controlClient,
		logClient:     logClient,
		taskQueue:     make(chan workerJob, queueSize),
		llmQueue:      make(chan workerJob, queueSize),
		actorQueue:    make(chan workerJob, queueSize),
		results:       make(chan workerJobResult, queueSize),
		actorLocks:    actorlock.NewTable(),
	}

	for _, runner := range taskRunners {
		pool.wg.Add(1)
		go pool.runPythonWorker(ctx, runner, pool.taskQueue, false)
	}
	for _, runner := range actorRunners {
		pool.wg.Add(1)
		go pool.runPythonWorker(ctx, runner, pool.actorQueue, true)
	}
	// LLM workers do not need Python runners; keeping them in Go avoids reserving
	// subprocesses for simulated or OpenAI-compatible model calls.
	for i := 0; i < llmPoolSize; i++ {
		pool.wg.Add(1)
		go pool.runLLMWorker(ctx, pool.llmQueue)
	}

	observability.Info("worker_executor_pool_started", map[string]any{
		"worker_id":       cfg.WorkerID,
		"capacity":        cfg.Capacity,
		"task_pool_size":  taskPoolSize,
		"llm_pool_size":   llmPoolSize,
		"actor_pool_size": actorPoolSize,
	})
	return pool, nil
}

// Dispatch routes a task to the queue implied by its spec and waits until it is enqueued or the context is cancelled.
func (p *localExecutorPool) Dispatch(ctx context.Context, task *logservepb.TaskSpec) error {
	if task == nil {
		return errors.New("task is nil")
	}
	job := workerJob{task: task, enqueuedAt: time.Now()}

	// LLM and actor tasks use separate pools so slow model loads or actor ordering do not block regular Python calls.
	queue := p.taskQueue
	if task.GetLlmModelName() != "" {
		queue = p.llmQueue
	} else if task.GetActorId() != "" {
		queue = p.actorQueue
	}

	select {
	case queue <- job:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops accepting new work and waits for all executor goroutines to exit.
func (p *localExecutorPool) Close() {
	p.closeOnce.Do(func() {
		close(p.taskQueue)
		close(p.llmQueue)
		close(p.actorQueue)
		p.wg.Wait()
	})
}

// runPythonWorker executes regular Python or actor jobs with one subprocess runner.
// When actorOrdered is true, it holds the actor-specific lock across execution so state updates remain serial.
func (p *localExecutorPool) runPythonWorker(ctx context.Context, runner *pythonRunner, queue <-chan workerJob, actorOrdered bool) {
	defer p.wg.Done()
	defer runner.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case job, ok := <-queue:
			if !ok {
				return
			}
			var unlock func()

			// The lock is per actor ID rather than global, allowing different actors to run concurrently.
			if actorOrdered {
				unlock = p.lockActor(job.task.GetActorId())
			}
			completion, err := executeTask(ctx, p.cfg, runner, p.cache, p.functionCache, p.controlClient, p.logClient, job.task, job.enqueuedAt)
			if unlock != nil {
				unlock()
			}
			p.finish(ctx, job.task, completion, err)
		}
	}
}

// runLLMWorker executes LLM tasks without a Python subprocess because mock and vLLM adapters are handled in Go.
func (p *localExecutorPool) runLLMWorker(ctx context.Context, queue <-chan workerJob) {
	defer p.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case job, ok := <-queue:
			if !ok {
				return
			}
			completion, err := executeTask(ctx, p.cfg, nil, p.cache, p.functionCache, p.controlClient, p.logClient, job.task, job.enqueuedAt)
			p.finish(ctx, job.task, completion, err)
		}
	}
}

// finish publishes a worker result unless the parent context has already been cancelled.
func (p *localExecutorPool) finish(ctx context.Context, task *logservepb.TaskSpec, completion *logservepb.CompleteTaskRequest, err error) {
	select {
	case p.results <- workerJobResult{task: task, completion: completion, err: err}:
	case <-ctx.Done():
	}
}

// lockActor returns an unlock callback for the actor ID, or nil when the pool is not fully initialized.
func (p *localExecutorPool) lockActor(actorID string) func() {
	if p == nil || p.actorLocks == nil {
		return nil
	}
	return p.actorLocks.Lock(actorID)
}

// closeRunners best-effort closes subprocesses during partial pool startup failure.
func closeRunners(runners []*pythonRunner) {
	for _, runner := range runners {
		_ = runner.Close()
	}
}

// positiveInt applies a fallback when a configured pool or queue size is not positive.
func positiveInt(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

// executeTask claims a task lease, runs the appropriate executor with the task timeout, and builds the completion request.
// On Python timeouts it restarts the subprocess because the request-response stream can no longer be trusted.
func executeTask(ctx context.Context, cfg Config, runner *pythonRunner, cache *modelCache, functionCache *FunctionCache, controlClient logservepb.ControlServiceClient, logClient logservepb.LogServiceClient, task *logservepb.TaskSpec, enqueuedAt time.Time) (*logservepb.CompleteTaskRequest, error) {
	if task == nil {
		return nil, errors.New("task is nil")
	}
	_ = enqueuedAt

	// StartTask claims the current lease epoch before local execution, preventing
	// stale or already-reassigned work from being completed by this process.
	if _, err := controlClient.StartTask(ctx, &logservepb.StartTaskRequest{
		TaskId:         task.GetTaskId(),
		WorkerId:       cfg.WorkerID,
		TaskLeaseEpoch: task.GetTaskLeaseEpoch(),
	}); err != nil {
		return nil, err
	}

	execCtx := ctx
	cancelExec := func() {}
	if task.GetTimeoutMs() > 0 {
		execCtx, cancelExec = context.WithTimeout(ctx, time.Duration(task.GetTimeoutMs())*time.Millisecond)
	}
	result, actorState, execErr := runExecutor(execCtx, cfg, runner, cache, functionCache, controlClient, logClient, task)
	cancelExec()

	// A timed-out Python call may leave bytes unread on stdout, so the runner is replaced before reuse.
	if errors.Is(execErr, context.DeadlineExceeded) && task.GetLlmModelName() == "" && runner != nil {
		if err := runner.Restart(ctx, cfg); err != nil {
			observability.Error("python_executor_restart_failed", err, map[string]any{"worker_id": cfg.WorkerID})
		}
	}
	status := logservepb.TaskStatus_TASK_STATUS_SUCCEEDED
	errText := ""
	if execErr != nil {
		status = logservepb.TaskStatus_TASK_STATUS_FAILED
		if errors.Is(execErr, context.DeadlineExceeded) && task.GetTimeoutMs() > 0 {
			errText = fmt.Sprintf("task timed out after %dms", task.GetTimeoutMs())
		} else {
			errText = execErr.Error()
		}
	}
	return &logservepb.CompleteTaskRequest{
		TaskId:         task.GetTaskId(),
		WorkerId:       cfg.WorkerID,
		Status:         status,
		ResultJson:     result,
		Error:          errText,
		ActorStateJson: actorState,
		ActorEpoch:     task.GetActorEpoch(),
		TaskLeaseEpoch: task.GetTaskLeaseEpoch(),
	}, execErr
}

// runExecutor dispatches a TaskSpec to the LLM, actor, or regular Python execution path and returns result plus optional actor state.
func runExecutor(ctx context.Context, cfg Config, runner *pythonRunner, cache *modelCache, functionCache *FunctionCache, controlClient logservepb.ControlServiceClient, logClient logservepb.LogServiceClient, task *logservepb.TaskSpec) ([]byte, []byte, error) {
	if task.GetLlmModelName() != "" {
		result, err := runLLMExecutor(ctx, cfg, cache, controlClient, logClient, task)
		return result, nil, err
	}
	if task.GetActorId() != "" {
		result, state, err := runActorExecutor(ctx, runner, task)
		return result, state, err
	}
	result, err := runPythonExecutor(ctx, runner, functionCache, task)
	return result, nil, err
}

// runLLMExecutor simulates or forwards an LLM request and records model-load lifecycle events to the log.
// It treats missing versions and adapters as v1/mock so older task submissions remain executable.
func runLLMExecutor(ctx context.Context, cfg Config, cache *modelCache, controlClient logservepb.ControlServiceClient, logClient logservepb.LogServiceClient, task *logservepb.TaskSpec) ([]byte, error) {
	modelName := task.GetLlmModelName()
	version := task.GetLlmModelVersion()
	if version == "" {
		version = "v1"
	}
	adapter := task.GetLlmAdapter()
	if adapter == "" {
		adapter = "mock"
	}
	prompt, err := promptFromArgs(task.GetArgsJson())
	if err != nil {
		return nil, err
	}

	start := time.Now()
	cacheHit := cache.has(modelName, version)
	if err := appendLLMEvent(ctx, logClient, task, cfg.WorkerID, "ModelLoadStarted", llmEventPayload{
		CacheHit: cacheHit,
	}); err != nil {
		return nil, err
	}

	loadStart := time.Now()
	checkpoint := checkpointLoadResult{
		CacheHit:           cacheHit,
		CacheUsedBytes:     cache.used(),
		CacheCapacityBytes: cache.capacity(),
	}

	// A configured checkpoint store takes precedence over in-memory warm markers so cache-hit metrics reflect disk state.
	if cache.usesCheckpointStore() {
		checkpoint, err = cache.ensureCheckpoint(ctx, modelName, version)
		if err != nil {
			return nil, err
		}
		cacheHit = checkpoint.CacheHit
	} else if !cacheHit && adapter == "mock" {
		if err := sleepContext(ctx, cfg.MockModelLoad); err != nil {
			return nil, err
		}
	}
	loadMs := checkpoint.ModelLoadMs
	if !cacheHit {
		if loadMs == 0 {
			loadMs = time.Since(loadStart).Milliseconds()
		}
		if loadMs == 0 {
			loadMs = 1
		}
		if !cache.usesCheckpointStore() {
			cache.add(modelName, version)
		}
		// Refresh the control plane promptly after a cold load so future scheduling
		// can account for the model before the next periodic heartbeat.
		_, _ = controlClient.Heartbeat(ctx, &logservepb.HeartbeatRequest{
			WorkerId:     cfg.WorkerID,
			CachedModels: cache.snapshotEntries(),
		})
	}
	if err := appendLLMEvent(ctx, logClient, task, cfg.WorkerID, "ModelLoaded", llmEventPayload{
		CacheHit:           cacheHit,
		CheckpointFetchMs:  checkpoint.CheckpointFetchMs,
		CacheUsedBytes:     checkpoint.CacheUsedBytes,
		CacheCapacityBytes: checkpoint.CacheCapacityBytes,
		EvictionCount:      checkpoint.EvictionCount,
		ModelLoadMs:        loadMs,
	}); err != nil {
		return nil, err
	}

	firstTokenStart := time.Now()
	var text string
	if adapter == "vllm" {
		text, err = callVLLM(ctx, cfg, modelName, version, prompt, task.GetLlmMaxTokens())
	} else {
		if err := sleepContext(ctx, cfg.MockFirstToken); err != nil {
			return nil, err
		}
		text = fmt.Sprintf("mock:%s:%s:%s", modelName, version, prompt)
	}
	firstTokenMs := time.Since(firstTokenStart).Milliseconds()
	if firstTokenMs == 0 {
		firstTokenMs = 1
	}
	if err != nil {
		return nil, err
	}
	totalMs := time.Since(start).Milliseconds()
	if totalMs == 0 {
		totalMs = 1
	}
	if err := appendLLMEvent(ctx, logClient, task, cfg.WorkerID, "LLMCompleted", llmEventPayload{
		CacheHit:           cacheHit,
		CheckpointFetchMs:  checkpoint.CheckpointFetchMs,
		CacheUsedBytes:     checkpoint.CacheUsedBytes,
		CacheCapacityBytes: checkpoint.CacheCapacityBytes,
		EvictionCount:      checkpoint.EvictionCount,
		ModelLoadMs:        loadMs,
		FirstTokenMs:       firstTokenMs,
		TotalLatencyMs:     totalMs,
	}); err != nil {
		return nil, err
	}
	return json.Marshal(text)
}

// runPythonExecutor resolves function source by hash when needed, invokes the Python runner, and returns raw JSON result bytes.
func runPythonExecutor(ctx context.Context, runner *pythonRunner, functionCache *FunctionCache, task *logservepb.TaskSpec) ([]byte, error) {
	if runner == nil {
		return nil, errors.New("python runner is required for task execution")
	}
	if functionCache == nil {
		functionCache = newFunctionCache(nil)
	}
	hash := task.GetFunctionHash()
	source := task.GetFunctionSource()

	// Once a runner has loaded a function hash, later tasks can send the hash without resending source.
	if hash != "" && !runner.knowsFunction(hash) {
		loaded, err := functionCache.SourceForTask(ctx, task)
		if err != nil {
			return nil, err
		}
		source = loaded
	}
	req := executorRequest{
		FunctionSource: source,
		FunctionRef:    task.GetFunctionRef(),
		FunctionHash:   hash,
		FunctionName:   task.GetFunctionName(),
		ArgsJSON:       append([]byte(nil), task.GetArgsJson()...),
	}
	resp, err := runner.Execute(ctx, req)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, errors.New(resp.Error)
	}
	if hash != "" && source != "" {
		runner.markFunction(hash)
	}
	if len(resp.Result) == 0 {
		return []byte("null"), nil
	}
	return append([]byte(nil), resp.Result...), nil
}

// runActorExecutor invokes the Python actor adapter with current actor state and returns result plus updated state.
func runActorExecutor(ctx context.Context, runner *pythonRunner, task *logservepb.TaskSpec) ([]byte, []byte, error) {
	if runner == nil {
		return nil, nil, errors.New("python runner is required for actor execution")
	}
	req := actorExecutorRequest{
		Mode:         "actor",
		ClassSource:  task.GetActorClassSource(),
		ClassName:    task.GetActorClassName(),
		MethodName:   task.GetActorMethod(),
		ArgsJSON:     append([]byte(nil), task.GetArgsJson()...),
		StateJSON:    append([]byte(nil), task.GetActorStateJson()...),
		InitArgsJSON: append([]byte(nil), task.GetActorInitArgsJson()...),
	}
	resp, err := runner.Execute(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	if !resp.OK {
		return nil, nil, errors.New(resp.Error)
	}
	result := resp.Result
	if len(result) == 0 {
		result = []byte("null")
	}

	// Empty state means a newly initialized actor with no persisted fields, not an executor failure.
	if len(resp.State) == 0 {
		resp.State = []byte("{}")
	}
	return append([]byte(nil), result...), append([]byte(nil), resp.State...), nil
}

// startPythonRunner launches the Python executor subprocess in msgpack mode by default, with JSON as an env-selected fallback.
func startPythonRunner(ctx context.Context, cfg Config) (*pythonRunner, error) {
	// The protocol is process-wide because one runner speaks one framing format for its lifetime.
	protocol := strings.ToLower(strings.TrimSpace(os.Getenv("LOGSERVE_EXECUTOR_PROTOCOL")))
	if protocol == "" {
		protocol = executorProtocolMsgpack
	}
	args := []string{cfg.ExecutorPath, "--loop-msgpack"}
	if protocol == executorProtocolJSON {
		args = []string{cfg.ExecutorPath, "--loop"}
	} else {
		protocol = executorProtocolMsgpack
	}

	cmd := exec.CommandContext(ctx, cfg.PythonPath, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &lockedBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &pythonRunner{
		cmd:            cmd,
		stdin:          stdin,
		stdout:         bufio.NewReaderSize(stdout, 64*1024),
		protocol:       protocol,
		stderr:         stderr,
		knownFunctions: make(map[string]struct{}),
	}, nil
}

// knowsFunction reports whether this subprocess has already received source for the function hash.
func (r *pythonRunner) knowsFunction(hash string) bool {
	if r == nil || hash == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.knownFunctions[hash]
	return ok
}

// markFunction records that this subprocess can execute future calls by function hash alone.
func (r *pythonRunner) markFunction(hash string) {
	if r == nil || hash == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.knownFunctions == nil {
		r.knownFunctions = make(map[string]struct{})
	}
	r.knownFunctions[hash] = struct{}{}
}

// Execute sends one request to the subprocess and waits for one response.
// The runner mutex protects the ordered stdin/stdout protocol from concurrent callers.
func (r *pythonRunner) Execute(ctx context.Context, req any) (executorResponse, error) {
	select {
	case <-ctx.Done():
		return executorResponse{}, ctx.Err()
	default:
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.protocol == executorProtocolJSON {
		return r.executeJSONLocked(ctx, req)
	}
	return r.executeMsgpackLocked(ctx, req)
}

// executeJSONLocked writes a newline-delimited JSON request and reads a JSON response.
// The caller must hold r.mu.
func (r *pythonRunner) executeJSONLocked(ctx context.Context, req any) (executorResponse, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return executorResponse{}, err
	}
	if _, err := r.stdin.Write(append(data, '\n')); err != nil {
		return executorResponse{}, err
	}

	return r.readResponseLocked(ctx, func() (executorResponse, error) {
		line, err := r.stdout.ReadBytes('\n')
		if err != nil {
			return executorResponse{}, executorReadError(err, r.stderr)
		}
		var resp executorResponse
		if err := json.Unmarshal(bytes.TrimSpace(line), &resp); err != nil {
			return executorResponse{}, err
		}
		return resp, nil
	})
}

// executeMsgpackLocked writes a length-prefixed msgpack request and reads a msgpack response.
// The caller must hold r.mu.
func (r *pythonRunner) executeMsgpackLocked(ctx context.Context, req any) (executorResponse, error) {
	data, err := marshalExecutorRequestMsgpack(req)
	if err != nil {
		return executorResponse{}, err
	}
	if err := writeExecutorFrame(r.stdin, data); err != nil {
		return executorResponse{}, err
	}

	return r.readResponseLocked(ctx, func() (executorResponse, error) {
		data, err := readExecutorFrame(r.stdout)
		if err != nil {
			return executorResponse{}, executorReadError(err, r.stderr)
		}
		resp, err := unmarshalExecutorResponseMsgpack(data)
		putExecutorFrameBuffer(data)
		return resp, err
	})
}

// readResponseLocked runs the blocking stdout read in a goroutine so context cancellation can kill the subprocess.
func (r *pythonRunner) readResponseLocked(ctx context.Context, read func() (executorResponse, error)) (executorResponse, error) {
	type scanResult struct {
		resp executorResponse
		err  error
	}
	cmd := r.cmd
	done := make(chan scanResult, 1)
	go func() {
		resp, err := read()
		done <- scanResult{resp: resp, err: err}
	}()

	select {
	case result := <-done:
		return result.resp, result.err
	case <-ctx.Done():
		// Killing the process is the only reliable way to unblock a stuck pipe read on all supported platforms.
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		// done is buffered so the reader goroutine can still publish if it unblocks after this timeout.
		return executorResponse{}, ctx.Err()
	}
}

// marshalExecutorRequestMsgpack converts typed executor requests into the Python msgpack map format.
// JSON payload fields stay as raw bytes so Python receives the exact submitted JSON envelope.
func marshalExecutorRequestMsgpack(req any) ([]byte, error) {
	fields := map[string]any{}
	switch typed := req.(type) {
	// Keep JSON as bytes instead of decoded msgpack objects to avoid changing Python-side parsing semantics.
	case executorRequest:
		if typed.FunctionSource != "" {
			fields["function_source"] = typed.FunctionSource
		}
		if typed.FunctionRef != "" {
			fields["function_ref"] = typed.FunctionRef
		}
		if typed.FunctionHash != "" {
			fields["function_hash"] = typed.FunctionHash
		}
		fields["function_name"] = typed.FunctionName
		fields["args_json"] = []byte(typed.ArgsJSON)
	case actorExecutorRequest:
		fields["mode"] = typed.Mode
		fields["class_source"] = typed.ClassSource
		fields["class_name"] = typed.ClassName
		fields["method_name"] = typed.MethodName
		fields["args_json"] = []byte(typed.ArgsJSON)
		fields["state_json"] = []byte(typed.StateJSON)
		fields["init_args_json"] = []byte(typed.InitArgsJSON)
	default:
		return nil, fmt.Errorf("unsupported executor request type %T", req)
	}
	return msgpack.Marshal(fields)
}

// unmarshalExecutorResponseMsgpack decodes the Python reply and normalizes result/state into raw JSON bytes.
// It accepts both *_json byte fields and legacy decoded result/state fields.
func unmarshalExecutorResponseMsgpack(data []byte) (executorResponse, error) {
	var fields map[string]any
	if err := msgpack.Unmarshal(data, &fields); err != nil {
		return executorResponse{}, err
	}
	resp := executorResponse{
		OK:    boolValue(fields["ok"]),
		Error: eventcodec.StringValue(fields["error"]),
	}

	// Prefer explicit raw JSON bytes; fallback fields are marshaled back to JSON for compatibility.
	if result := eventcodec.BytesValue(fields["result_json"]); len(result) > 0 {
		resp.Result = append([]byte(nil), result...)
	} else if value, ok := fields["result"]; ok {
		data, err := json.Marshal(value)
		if err != nil {
			return executorResponse{}, err
		}
		resp.Result = data
	}
	if state := eventcodec.BytesValue(fields["state_json"]); len(state) > 0 {
		resp.State = append([]byte(nil), state...)
	} else if value, ok := fields["state"]; ok {
		data, err := json.Marshal(value)
		if err != nil {
			return executorResponse{}, err
		}
		resp.State = data
	}
	return resp, nil
}

// writeExecutorFrame writes a big-endian length prefix followed by the msgpack payload.
func writeExecutorFrame(w io.Writer, data []byte) error {
	if len(data) > maxExecutorFrameBytes {
		return fmt.Errorf("executor frame %d exceeds max %d", len(data), maxExecutorFrameBytes)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(data)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}

// readExecutorFrame reads a bounded length-prefixed executor frame and returns a buffer the caller may return to the pool.
func readExecutorFrame(r io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(header[:])
	// Reject the advertised size before allocating so a corrupted subprocess cannot force unbounded memory growth.
	if size > maxExecutorFrameBytes {
		return nil, fmt.Errorf("executor frame %d exceeds max %d", size, maxExecutorFrameBytes)
	}
	data := getExecutorFrameBuffer(int(size))
	if _, err := io.ReadFull(r, data); err != nil {
		putExecutorFrameBuffer(data)
		return nil, err
	}
	return data, nil
}

// getExecutorFrameBuffer returns a reusable buffer for medium frames and allocates directly for oversized frames.
func getExecutorFrameBuffer(size int) []byte {
	if size > maxPooledExecutorFrame {
		return make([]byte, size)
	}
	ptr := executorFrameBufferPool.Get().(*[]byte)
	buf := *ptr
	if cap(buf) < size {
		return make([]byte, size)
	}
	return buf[:size]
}

// putExecutorFrameBuffer returns only bounded-capacity frame buffers to the pool to avoid retaining large spikes.
func putExecutorFrameBuffer(buf []byte) {
	if cap(buf) == 0 || cap(buf) > maxPooledExecutorFrame {
		return
	}
	buf = buf[:0]
	executorFrameBufferPool.Put(&buf)
}

// executorReadError prefers captured stderr over a generic pipe error when the subprocess exits.
func executorReadError(err error, stderr *lockedBuffer) error {
	if stderr != nil && stderr.Len() > 0 {
		return errors.New(stderr.String())
	}
	if errors.Is(err, io.EOF) {
		return errors.New("python executor stopped")
	}
	return err
}

// boolValue reads a loosely typed msgpack map boolean without panicking on malformed replies.
func boolValue(value any) bool {
	v, _ := value.(bool)
	return v
}

// Restart replaces the subprocess after a timeout or stream failure and clears runner-local function knowledge.
func (r *pythonRunner) Restart(ctx context.Context, cfg Config) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Keep the runner locked while replacing process handles so no caller can write into the old stdin.
	if r.stdin != nil {
		_ = r.stdin.Close()
	}
	if r.cmd != nil && r.cmd.Process != nil {
		_ = r.cmd.Process.Kill()
		_ = r.cmd.Wait()
	}
	next, err := startPythonRunner(ctx, cfg)
	if err != nil {
		return err
	}
	r.cmd = next.cmd
	r.stdin = next.stdin
	r.stdout = next.stdout
	r.protocol = next.protocol
	r.stderr = next.stderr
	r.knownFunctions = make(map[string]struct{})
	return nil
}

// Close closes stdin and waits for the subprocess to exit.
func (r *pythonRunner) Close() error {
	_ = r.stdin.Close()
	return r.cmd.Wait()
}

// taskStream returns the per-task log stream name used for task-scoped events.
func taskStream(taskID string) string {
	return "task:" + taskID
}

// newModelCache initializes in-memory warm-model state and restores disk checkpoint manifests when configured.
func newModelCache(cfg Config) *modelCache {
	cache := &modelCache{
		models:        map[string]bool{},
		entries:       map[string]*list.Element{},
		lru:           list.New(),
		inflight:      map[string]*loadCall{},
		sourceDir:     cfg.ModelCheckpointSourceDir,
		cacheDir:      cfg.ModelCacheDir,
		capacityBytes: cfg.ModelCacheCapacityBytes,
	}
	if cache.cacheDir != "" {
		_ = os.MkdirAll(cache.cacheDir, 0o755)
		cache.loadCheckpointManifests()
	}
	for _, model := range cfg.CachedModels {
		name, version := splitModelKey(model)
		if name == "" {
			continue
		}
		cache.models[modelKey(name, version)] = true
	}
	return cache
}

// has reports whether a model is warm, checking disk-backed checkpoint state before the in-memory marker set.
func (c *modelCache) has(name, version string) bool {
	key := modelKey(name, version)
	if c.cacheDir != "" {
		if _, ok := c.cachedCheckpoint(name, version, time.Now()); ok {
			return true
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.models[key]
}

// add marks a model as warm in the lightweight in-memory cache used when no checkpoint store is configured.
func (c *modelCache) add(name, version string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.models[modelKey(name, version)] = true
}

// snapshotEntries returns the worker-advertised model cache state for registration and heartbeats.
// Disk-backed entries are reported in LRU order before preconfigured warm markers.
func (c *modelCache) snapshotEntries() []*logservepb.ModelCacheEntry {
	c.mu.Lock()
	keys := make([]string, 0, len(c.models))
	seen := make(map[string]struct{}, len(c.models))

	// Capture keys under the lock, then build protobuf messages after unlocking to keep cache lock hold time short.
	for element := c.lru.Front(); element != nil; element = element.Next() {
		entry := element.Value.(*cacheEntry)
		if !c.models[entry.key] {
			continue
		}
		keys = append(keys, entry.key)
		seen[entry.key] = struct{}{}
	}
	for key := range c.models {
		if _, ok := seen[key]; ok {
			continue
		}
		keys = append(keys, key)
	}
	c.mu.Unlock()

	entries := make([]*logservepb.ModelCacheEntry, 0, len(keys))
	for _, key := range keys {
		name, version := splitModelKey(key)
		entries = append(entries, &logservepb.ModelCacheEntry{Name: name, Version: version})
	}
	return entries
}

// loadCheckpointManifests rebuilds the disk-backed cache index from manifest files on startup.
// Invalid manifests or missing checkpoint files are ignored so a partial cache directory cannot prevent worker startup.
func (c *modelCache) loadCheckpointManifests() {
	manifestPaths, err := filepath.Glob(filepath.Join(c.cacheDir, "*.manifest.json"))
	if err != nil {
		return
	}
	loaded := make([]cacheEntry, 0, len(manifestPaths))
	for _, manifestPath := range manifestPaths {
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			continue
		}
		var manifest modelCacheManifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			continue
		}
		name, version := manifest.Name, firstNonEmpty(manifest.Version, "v1")
		if name == "" {
			continue
		}
		checkpointFile := filepath.Base(manifest.CheckpointFile)
		if checkpointFile == "." || checkpointFile == string(filepath.Separator) || checkpointFile == "" {
			continue
		}
		checkpointPath := filepath.Join(c.cacheDir, checkpointFile)
		info, err := os.Stat(checkpointPath)
		if err != nil || info.IsDir() {
			continue
		}
		lastAccess := manifest.LastAccessMs
		if lastAccess == 0 {
			lastAccess = info.ModTime().UnixMilli()
		}
		loaded = append(loaded, cacheEntry{
			key:        modelKey(name, version),
			path:       checkpointPath,
			size:       info.Size(),
			lastAccess: lastAccess,
		})
	}

	// Load oldest first because putEntryLocked pushes to the front, leaving the newest access at the LRU front.
	sort.Slice(loaded, func(i, j int) bool {
		return loaded[i].lastAccess < loaded[j].lastAccess
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range loaded {
		c.putEntryLocked(entry)
	}
}

// usesCheckpointStore reports whether both source and destination directories are configured for checkpoint caching.
func (c *modelCache) usesCheckpointStore() bool {
	return c.sourceDir != "" && c.cacheDir != ""
}

// capacity returns the configured checkpoint cache byte limit under lock.
func (c *modelCache) capacity() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.capacityBytes
}

// used returns current checkpoint cache usage under lock.
func (c *modelCache) used() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usedBytes
}

// ensureCheckpoint makes a model checkpoint available locally and returns load/cache metrics.
// Concurrent cold loads for the same model share one loadCall, while different models can load in parallel.
func (c *modelCache) ensureCheckpoint(ctx context.Context, name, version string) (checkpointLoadResult, error) {
	if !c.usesCheckpointStore() {
		return checkpointLoadResult{}, errors.New("checkpoint cache is not configured")
	}
	key := modelKey(name, version)
	for {
		// Re-check cache state before joining or creating a cold load; another goroutine may have just finished.
		if entry, ok := c.cachedCheckpoint(name, version, time.Now()); ok {
			loadMs, err := readCheckpointFunc(ctx, entry.path)
			if err != nil {
				return checkpointLoadResult{}, err
			}
			used, capacity := c.cacheUsage()
			return checkpointLoadResult{
				CacheHit:           true,
				ModelLoadMs:        loadMs,
				CacheUsedBytes:     used,
				CacheCapacityBytes: capacity,
			}, nil
		}

		call, wait := c.loadCallForKey(key)
		if wait {
			select {
			case <-call.done:
				if call.err != nil {
					return checkpointLoadResult{}, call.err
				}
				// Waiters re-check the cache instead of returning the producer's
				// result so their metrics describe a hit on the now-local file.
				continue
			case <-ctx.Done():
				return checkpointLoadResult{}, ctx.Err()
			}
		}

		// The producer performs copy/read work outside c.mu; only the per-key inflight entry serializes same-model callers.
		result, err := c.loadCheckpoint(ctx, key, name, version)
		c.finishLoadCall(key, call, result, err)
		return result, err
	}
}

// cachedCheckpoint returns a valid checkpoint entry, refreshing LRU and manifest access metadata.
// It removes stale in-memory entries whose files disappeared and can discover an existing file by path.
func (c *modelCache) cachedCheckpoint(name, version string, at time.Time) (cacheEntry, bool) {
	key := modelKey(name, version)
	lastAccess := at.UnixMilli()

	c.mu.Lock()
	if element, ok := c.entries[key]; ok {
		entry := element.Value.(*cacheEntry)
		if _, err := os.Stat(entry.path); err == nil {
			entry.lastAccess = lastAccess
			c.models[key] = true
			c.lru.MoveToFront(element)
			snapshot := *entry
			c.mu.Unlock()

			// Manifest I/O is intentionally outside the cache lock so heartbeats and other loads are not blocked by disk writes.
			_ = writeCheckpointManifest(snapshot.path, name, version, snapshot.size, at)
			return snapshot, true
		}
		c.removeElementLocked(element)
	}
	c.mu.Unlock()

	path := c.checkpointPath(name, version)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return cacheEntry{}, false
	}
	entry := cacheEntry{key: key, path: path, size: info.Size(), lastAccess: lastAccess}
	c.mu.Lock()
	c.putEntryLocked(entry)
	c.mu.Unlock()
	_ = writeCheckpointManifest(path, name, version, info.Size(), at)
	return entry, true
}

// loadCallForKey implements per-model singleflight for checkpoint cold loads.
func (c *modelCache) loadCallForKey(key string) (*loadCall, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if call, ok := c.inflight[key]; ok {
		return call, true
	}
	call := &loadCall{done: make(chan struct{})}
	c.inflight[key] = call
	return call, false
}

// finishLoadCall publishes the cold-load result and releases waiters if this call is still current for the key.
func (c *modelCache) finishLoadCall(key string, call *loadCall, result checkpointLoadResult, err error) {
	c.mu.Lock()
	// Ignore stale producers defensively if a future change replaces the inflight call for this key.
	if current := c.inflight[key]; current == call {
		call.result = result
		call.err = err
		delete(c.inflight, key)
		close(call.done)
	}
	c.mu.Unlock()
}

// loadCheckpoint copies a source checkpoint into the local cache, simulates model load by reading it, then inserts it into the LRU.
func (c *modelCache) loadCheckpoint(ctx context.Context, key, name, version string) (checkpointLoadResult, error) {
	sourcePath, err := c.sourcePath(name, version)
	if err != nil {
		return checkpointLoadResult{}, err
	}
	info, err := os.Stat(sourcePath)
	if err != nil {
		return checkpointLoadResult{}, err
	}
	size := info.Size()

	// Reject oversize checkpoints before copying because eviction can never make enough room for them.
	if c.capacityBytes > 0 && size > c.capacityBytes {
		return checkpointLoadResult{}, fmt.Errorf("checkpoint %s:%s size %d exceeds cache capacity %d", name, version, size, c.capacityBytes)
	}

	targetPath := c.checkpointPath(name, version)
	fetchMs, err := copyCheckpointFunc(ctx, sourcePath, targetPath)
	if err != nil {
		return checkpointLoadResult{}, err
	}
	loadMs, err := readCheckpointFunc(ctx, targetPath)
	if err != nil {
		return checkpointLoadResult{}, err
	}
	lastAccess := time.Now()
	if err := writeCheckpointManifest(targetPath, name, version, size, lastAccess); err != nil {
		return checkpointLoadResult{}, err
	}

	c.mu.Lock()
	evictions, err := c.evictForLocked(size)
	if err != nil {
		c.mu.Unlock()
		return checkpointLoadResult{}, err
	}
	c.putEntryLocked(cacheEntry{key: key, path: targetPath, size: size, lastAccess: lastAccess.UnixMilli()})
	used := c.usedBytes
	capacity := c.capacityBytes
	c.mu.Unlock()

	return checkpointLoadResult{
		CacheHit:           false,
		CheckpointFetchMs:  fetchMs,
		ModelLoadMs:        loadMs,
		CacheUsedBytes:     used,
		CacheCapacityBytes: capacity,
		EvictionCount:      evictions,
	}, nil
}

// evictForLocked removes least-recently-used checkpoint files until the incoming entry fits.
// The caller must hold c.mu.
func (c *modelCache) evictForLocked(incomingBytes int64) (int64, error) {
	if c.capacityBytes <= 0 {
		return 0, nil
	}
	var evictions int64
	for c.usedBytes+incomingBytes > c.capacityBytes && c.lru.Len() > 0 {
		element := c.lru.Back()
		entry := element.Value.(*cacheEntry)
		if err := os.Remove(entry.path); err != nil && !os.IsNotExist(err) {
			return evictions, err
		}
		if err := os.Remove(checkpointManifestPath(entry.path)); err != nil && !os.IsNotExist(err) {
			return evictions, err
		}
		c.removeElementLocked(element)
		evictions++
	}
	return evictions, nil
}

// putEntryLocked inserts or updates a cache entry, preserving O(1) lookup and LRU promotion.
// The caller must hold c.mu.
func (c *modelCache) putEntryLocked(entry cacheEntry) {
	if element, ok := c.entries[entry.key]; ok {
		current := element.Value.(*cacheEntry)
		c.usedBytes += entry.size - current.size
		*current = entry
		c.lru.MoveToFront(element)
	} else {
		element := c.lru.PushFront(&entry)
		c.entries[entry.key] = element
		c.usedBytes += entry.size
	}
	if c.usedBytes < 0 {
		c.usedBytes = 0
	}
	c.models[entry.key] = true
}

// removeElementLocked deletes one LRU element and adjusts the accounting.
// The caller must hold c.mu.
func (c *modelCache) removeElementLocked(element *list.Element) {
	entry := element.Value.(*cacheEntry)
	delete(c.entries, entry.key)
	delete(c.models, entry.key)
	c.lru.Remove(element)
	c.usedBytes -= entry.size
	if c.usedBytes < 0 {
		c.usedBytes = 0
	}
}

// cacheUsage returns used and capacity bytes as a consistent pair.
func (c *modelCache) cacheUsage() (int64, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usedBytes, c.capacityBytes
}

// checkpointPath returns the worker-local checkpoint path for a model/version pair.
func (c *modelCache) checkpointPath(name, version string) string {
	return filepath.Join(c.cacheDir, safeModelFileName(name, version)+".checkpoint")
}

// checkpointManifestPath returns the metadata sidecar path for a cached checkpoint file.
func checkpointManifestPath(checkpointPath string) string {
	return checkpointPath + ".manifest.json"
}

// writeCheckpointManifest rewrites checkpoint metadata through a temp file before replacing the manifest.
// The replace step preserves a whole-file manifest across crashes and works around Windows rename semantics.
func writeCheckpointManifest(checkpointPath, name, version string, size int64, lastAccess time.Time) error {
	manifest := modelCacheManifest{
		Name:           name,
		Version:        firstNonEmpty(version, "v1"),
		CheckpointFile: filepath.Base(checkpointPath),
		SizeBytes:      size,
		LastAccessMs:   lastAccess.UnixMilli(),
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	manifestPath := checkpointManifestPath(checkpointPath)

	// The temp file keeps readers from observing a partially written JSON manifest.
	tmpPath := manifestPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	if err := replaceFileWithRename(tmpPath, manifestPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// sourcePath resolves supported source checkpoint layouts for a model/version pair.
func (c *modelCache) sourcePath(name, version string) (string, error) {
	version = firstNonEmpty(version, "v1")

	// Support both directory-based and flat-file layouts so tests and local demos can use simple fixtures.
	candidates := []string{
		filepath.Join(c.sourceDir, name+"-"+version, "checkpoint.bin"),
		filepath.Join(c.sourceDir, name, version, "checkpoint.bin"),
		filepath.Join(c.sourceDir, name+"-"+version+".bin"),
		filepath.Join(c.sourceDir, safeModelFileName(name, version)+".bin"),
	}
	for _, candidate := range candidates {
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("checkpoint source for %s:%s not found under %s", name, version, c.sourceDir)
}

// safeModelFileName converts a model/version pair into a filesystem-safe basename.
func safeModelFileName(name, version string) string {
	version = firstNonEmpty(version, "v1")
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_")
	return replacer.Replace(name) + "-" + replacer.Replace(version)
}

// copyCheckpointFunc and readCheckpointFunc are indirections used by tests to control checkpoint I/O timing.
var (
	copyCheckpointFunc = copyCheckpoint
	readCheckpointFunc = readCheckpoint
)

// copyCheckpoint copies a checkpoint through a temporary file and returns elapsed fetch time in milliseconds.
// It checks context cancellation between reads so shutdown or task timeout can abandon the copy cleanly.
func copyCheckpoint(ctx context.Context, sourcePath, targetPath string) (int64, error) {
	start := time.Now()
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return 0, err
	}

	// Copy into a sibling temp file so a failed or cancelled fetch never replaces a valid checkpoint.
	tmpPath := targetPath + ".tmp"
	source, err := os.Open(sourcePath)
	if err != nil {
		return 0, err
	}
	defer source.Close()
	target, err := os.Create(tmpPath)
	if err != nil {
		return 0, err
	}
	buf := make([]byte, 64*1024)
	for {
		select {
		case <-ctx.Done():
			_ = target.Close()
			_ = os.Remove(tmpPath)
			return 0, ctx.Err()
		default:
		}
		n, readErr := source.Read(buf)
		if n > 0 {
			if _, err := target.Write(buf[:n]); err != nil {
				_ = target.Close()
				_ = os.Remove(tmpPath)
				return 0, err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = target.Close()
			_ = os.Remove(tmpPath)
			return 0, readErr
		}
	}
	if err := target.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return 0, err
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		_ = os.Remove(tmpPath)
		return 0, err
	}
	return positiveElapsedMs(start), nil
}

// readCheckpoint reads the entire checkpoint file to model the cost of loading it into memory.
func readCheckpoint(ctx context.Context, path string) (int64, error) {
	start := time.Now()
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	buf := make([]byte, 64*1024)
	for {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
		}
		_, err := file.Read(buf)
		if errors.Is(err, io.EOF) {
			return positiveElapsedMs(start), nil
		}
		if err != nil {
			return 0, err
		}
	}
}

// positiveElapsedMs rounds sub-millisecond timings up to one millisecond for user-visible metrics.
func positiveElapsedMs(start time.Time) int64 {
	elapsed := time.Since(start).Milliseconds()
	if elapsed == 0 {
		return 1
	}
	return elapsed
}

// modelKey returns the stable cache key for a model/version pair, defaulting an empty version to v1.
func modelKey(name, version string) string {
	if version == "" {
		version = "v1"
	}
	return name + ":" + version
}

// splitModelKey parses cache keys and legacy configured model strings, defaulting the version to v1.
func splitModelKey(value string) (string, string) {
	parts := strings.SplitN(strings.TrimSpace(value), ":", 2)
	if len(parts) == 0 || parts[0] == "" {
		return "", ""
	}
	if len(parts) == 1 || parts[1] == "" {
		return parts[0], "v1"
	}
	return parts[0], parts[1]
}

// firstNonEmpty returns the first non-empty value in order, or an empty string when all values are empty.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// promptFromArgs extracts an LLM prompt from the submitted args envelope.
// It accepts either args[0] or kwargs.prompt and preserves non-string JSON as raw text when needed.
func promptFromArgs(data []byte) (string, error) {
	var payload llmArgsPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", err
	}
	if len(payload.Args) > 0 {
		var prompt string
		if err := json.Unmarshal(payload.Args[0], &prompt); err == nil {
			return prompt, nil
		}
		return string(payload.Args[0]), nil
	}
	if raw, ok := payload.Kwargs["prompt"]; ok {
		var prompt string
		if err := json.Unmarshal(raw, &prompt); err == nil {
			return prompt, nil
		}
		return string(raw), nil
	}
	return "", errors.New("llm prompt is required")
}

// appendLLMEvent writes one model lifecycle event to the task-scoped LLM log stream.
// The idempotency key is deterministic per task and event type so retries do not duplicate lifecycle events.
func appendLLMEvent(ctx context.Context, logClient logservepb.LogServiceClient, task *logservepb.TaskSpec, workerID, eventType string, payload llmEventPayload) error {
	now := time.Now().UnixMilli()
	payload.TaskID = task.GetTaskId()
	payload.ModelName = task.GetLlmModelName()
	payload.ModelVersion = task.GetLlmModelVersion()
	if payload.ModelVersion == "" {
		payload.ModelVersion = "v1"
	}
	payload.WorkerID = workerID
	payload.TimestampMs = now
	data, _ := json.Marshal(payload)
	_, err := logClient.AppendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       "llm:" + task.GetTaskId(),
		EventType:      eventType,
		IdempotencyKey: task.GetTaskId() + ":" + eventType,
		Payload:        data,
	})
	return err
}

// sleepContext waits for the mock latency duration while still honoring task cancellation.
func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// callVLLM sends an OpenAI-compatible chat completion request to the configured vLLM endpoint.
func callVLLM(ctx context.Context, cfg Config, modelName, version, prompt string, maxTokens uint32) (string, error) {
	if cfg.VLLMBaseURL == "" {
		return "", errors.New("LOGSERVE_VLLM_BASE_URL is required for vllm adapter")
	}
	model := modelName

	// Non-default versions are encoded into the model name because the vLLM API accepts a single model field.
	if version != "" && version != "v1" {
		model = modelName + ":" + version
	}
	body, _ := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"max_tokens": maxTokens,
	})
	url := strings.TrimRight(cfg.VLLMBaseURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("vllm request failed: %s: %s", resp.Status, string(data))
	}
	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Text string `json:"text"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return "", err
	}
	if len(decoded.Choices) == 0 {
		return "", errors.New("vllm response has no choices")
	}
	// Prefer chat-completions content, but accept text for OpenAI-compatible
	// servers that still return completion-style choices.
	if decoded.Choices[0].Message.Content != "" {
		return decoded.Choices[0].Message.Content, nil
	}
	return decoded.Choices[0].Text, nil
}
