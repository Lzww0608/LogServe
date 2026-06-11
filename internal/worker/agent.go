package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/observability"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type Config struct {
	WorkerID                 string
	ControlAddr              string
	LogAddr                  string
	PythonPath               string
	ExecutorPath             string
	PollInterval             time.Duration
	MaxTasks                 int
	CachedModels             []string
	Capacity                 uint32
	TaskPoolSize             int
	LLMPoolSize              int
	ActorPoolSize            int
	MockModelLoad            time.Duration
	MockFirstToken           time.Duration
	VLLMBaseURL              string
	ModelCheckpointSourceDir string
	ModelCacheDir            string
	ModelCacheCapacityBytes  int64
}

type executorRequest struct {
	FunctionSource string          `json:"function_source"`
	FunctionName   string          `json:"function_name"`
	ArgsJSON       json.RawMessage `json:"args_json"`
}

type executorResponse struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	State  json.RawMessage `json:"state,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type actorExecutorRequest struct {
	Mode         string          `json:"mode"`
	ClassSource  string          `json:"class_source"`
	ClassName    string          `json:"class_name"`
	MethodName   string          `json:"method_name"`
	ArgsJSON     json.RawMessage `json:"args_json"`
	StateJSON    json.RawMessage `json:"state_json"`
	InitArgsJSON json.RawMessage `json:"init_args_json"`
}

type llmArgsPayload struct {
	Args   []json.RawMessage          `json:"args"`
	Kwargs map[string]json.RawMessage `json:"kwargs"`
}

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

type pythonRunner struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	scanner *bufio.Scanner
	stderr  *lockedBuffer
	mu      sync.Mutex
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type modelCache struct {
	mu            sync.RWMutex
	checkpointMu  sync.Mutex
	models        map[string]bool
	checkpoints   map[string]modelCacheEntry
	sourceDir     string
	cacheDir      string
	capacityBytes int64
	usedBytes     int64
}

type modelCacheEntry struct {
	path       string
	size       int64
	lastAccess time.Time
}

type modelCacheManifest struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	CheckpointFile string `json:"checkpoint_file"`
	SizeBytes      int64  `json:"size_bytes"`
	LastAccessMs   int64  `json:"last_access_ms"`
}

type checkpointLoadResult struct {
	CacheHit           bool
	CheckpointFetchMs  int64
	ModelLoadMs        int64
	CacheUsedBytes     int64
	CacheCapacityBytes int64
	EvictionCount      int64
}

type workerJob struct {
	task       *logservepb.TaskSpec
	enqueuedAt time.Time
}

type workerJobResult struct {
	task *logservepb.TaskSpec
	err  error
}

type localExecutorPool struct {
	cfg           Config
	cache         *modelCache
	controlClient logservepb.ControlServiceClient
	logClient     logservepb.LogServiceClient
	taskQueue     chan workerJob
	llmQueue      chan workerJob
	actorQueue    chan workerJob
	results       chan workerJobResult
	actorLocksMu  sync.Mutex
	actorLocks    map[string]*sync.Mutex
	closeOnce     sync.Once
	wg            sync.WaitGroup
}

func Run(ctx context.Context, cfg Config) error {
	if cfg.WorkerID == "" {
		cfg.WorkerID = "worker-1"
	}
	if cfg.PythonPath == "" {
		cfg.PythonPath = "python"
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 200 * time.Millisecond
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

	controlConn, err := grpc.NewClient(cfg.ControlAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer controlConn.Close()
	logConn, err := grpc.NewClient(cfg.LogAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer logConn.Close()

	controlClient := logservepb.NewControlServiceClient(controlConn)
	logClient := logservepb.NewLogServiceClient(logConn)
	cache := newModelCache(cfg)

	if _, err := controlClient.RegisterWorker(ctx, &logservepb.RegisterWorkerRequest{
		WorkerId:     cfg.WorkerID,
		CachedModels: cache.entries(),
		Capacity:     cfg.Capacity,
	}); err != nil {
		return err
	}
	observability.Info("worker_registered", map[string]any{"worker_id": cfg.WorkerID})

	pool, err := startLocalExecutorPool(ctx, cfg, cache, controlClient, logClient)
	if err != nil {
		return err
	}
	defer pool.Close()

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()
	completedTasks := 0
	dispatchedTasks := 0
	inFlight := 0
	localCapacity := int(cfg.Capacity)
	if localCapacity <= 0 {
		localCapacity = 1
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case result := <-pool.results:
			inFlight--
			completedTasks++
			if result.task == nil {
				continue
			}
			if result.err != nil {
				observability.Error("task_execution_failed", result.err, map[string]any{"worker_id": cfg.WorkerID, "task_id": result.task.GetTaskId()})
			}
			if cfg.MaxTasks > 0 && completedTasks >= cfg.MaxTasks {
				return nil
			}
		case <-ticker.C:
			if _, err := controlClient.Heartbeat(ctx, &logservepb.HeartbeatRequest{
				WorkerId:     cfg.WorkerID,
				CachedModels: cache.entries(),
			}); err != nil {
				observability.Error("worker_heartbeat_failed", err, map[string]any{"worker_id": cfg.WorkerID})
				continue
			}
			for inFlight < localCapacity {
				if cfg.MaxTasks > 0 && dispatchedTasks >= cfg.MaxTasks {
					break
				}
				resp, err := controlClient.PollTask(ctx, &logservepb.PollTaskRequest{WorkerId: cfg.WorkerID})
				if err != nil {
					observability.Error("worker_poll_failed", err, map[string]any{"worker_id": cfg.WorkerID})
					break
				}
				if !resp.GetHasTask() {
					break
				}
				if err := pool.Dispatch(ctx, resp.GetTask()); err != nil {
					observability.Error("worker_dispatch_failed", err, map[string]any{"worker_id": cfg.WorkerID, "task_id": resp.GetTask().GetTaskId()})
					break
				}
				inFlight++
				dispatchedTasks++
			}
		}
	}
}

func startLocalExecutorPool(ctx context.Context, cfg Config, cache *modelCache, controlClient logservepb.ControlServiceClient, logClient logservepb.LogServiceClient) (*localExecutorPool, error) {
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
		controlClient: controlClient,
		logClient:     logClient,
		taskQueue:     make(chan workerJob, queueSize),
		llmQueue:      make(chan workerJob, queueSize),
		actorQueue:    make(chan workerJob, queueSize),
		results:       make(chan workerJobResult, queueSize),
		actorLocks:    map[string]*sync.Mutex{},
	}

	for _, runner := range taskRunners {
		pool.wg.Add(1)
		go pool.runPythonWorker(ctx, runner, pool.taskQueue, false)
	}
	for _, runner := range actorRunners {
		pool.wg.Add(1)
		go pool.runPythonWorker(ctx, runner, pool.actorQueue, true)
	}
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

func (p *localExecutorPool) Dispatch(ctx context.Context, task *logservepb.TaskSpec) error {
	if task == nil {
		return errors.New("task is nil")
	}
	job := workerJob{task: task, enqueuedAt: time.Now()}
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

func (p *localExecutorPool) Close() {
	p.closeOnce.Do(func() {
		close(p.taskQueue)
		close(p.llmQueue)
		close(p.actorQueue)
		p.wg.Wait()
	})
}

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
			if actorOrdered {
				unlock = p.lockActor(job.task.GetActorId())
			}
			err := executeTask(ctx, p.cfg, runner, p.cache, p.controlClient, p.logClient, job.task, job.enqueuedAt)
			if unlock != nil {
				unlock()
			}
			p.finish(ctx, job.task, err)
		}
	}
}

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
			err := executeTask(ctx, p.cfg, nil, p.cache, p.controlClient, p.logClient, job.task, job.enqueuedAt)
			p.finish(ctx, job.task, err)
		}
	}
}

func (p *localExecutorPool) finish(ctx context.Context, task *logservepb.TaskSpec, err error) {
	select {
	case p.results <- workerJobResult{task: task, err: err}:
	case <-ctx.Done():
	}
}

func (p *localExecutorPool) lockActor(actorID string) func() {
	if actorID == "" {
		return nil
	}
	p.actorLocksMu.Lock()
	lock, ok := p.actorLocks[actorID]
	if !ok {
		lock = &sync.Mutex{}
		p.actorLocks[actorID] = lock
	}
	p.actorLocksMu.Unlock()

	lock.Lock()
	return lock.Unlock
}

func closeRunners(runners []*pythonRunner) {
	for _, runner := range runners {
		_ = runner.Close()
	}
}

func positiveInt(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

func executeTask(ctx context.Context, cfg Config, runner *pythonRunner, cache *modelCache, controlClient logservepb.ControlServiceClient, logClient logservepb.LogServiceClient, task *logservepb.TaskSpec, enqueuedAt time.Time) error {
	if task == nil {
		return errors.New("task is nil")
	}

	localQueueWaitMs := int64(0)
	if !enqueuedAt.IsZero() {
		localQueueWaitMs = time.Since(enqueuedAt).Milliseconds()
		if localQueueWaitMs < 0 {
			localQueueWaitMs = 0
		}
	}
	startPayload, _ := json.Marshal(map[string]any{
		"task_id":             task.GetTaskId(),
		"worker_id":           cfg.WorkerID,
		"task_lease_epoch":    task.GetTaskLeaseEpoch(),
		"local_queue_wait_ms": localQueueWaitMs,
	})
	if _, err := logClient.AppendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       taskStream(task.GetTaskId()),
		EventType:      "TaskStarted",
		IdempotencyKey: fmt.Sprintf("%s:started:%s:%d", task.GetTaskId(), cfg.WorkerID, task.GetTaskLeaseEpoch()),
		Payload:        startPayload,
	}); err != nil {
		return err
	}
	if _, err := controlClient.StartTask(ctx, &logservepb.StartTaskRequest{
		TaskId:         task.GetTaskId(),
		WorkerId:       cfg.WorkerID,
		TaskLeaseEpoch: task.GetTaskLeaseEpoch(),
	}); err != nil {
		return err
	}

	execCtx := ctx
	cancelExec := func() {}
	if task.GetTimeoutMs() > 0 {
		execCtx, cancelExec = context.WithTimeout(ctx, time.Duration(task.GetTimeoutMs())*time.Millisecond)
	}
	result, actorState, execErr := runExecutor(execCtx, cfg, runner, cache, controlClient, logClient, task)
	cancelExec()
	if errors.Is(execErr, context.DeadlineExceeded) && task.GetLlmModelName() == "" && runner != nil {
		if err := runner.Restart(ctx, cfg); err != nil {
			observability.Error("python_executor_restart_failed", err, map[string]any{"worker_id": cfg.WorkerID})
		}
	}
	status := logservepb.TaskStatus_TASK_STATUS_SUCCEEDED
	errText := ""
	eventType := "TaskCompleted"
	if execErr != nil {
		status = logservepb.TaskStatus_TASK_STATUS_FAILED
		if errors.Is(execErr, context.DeadlineExceeded) && task.GetTimeoutMs() > 0 {
			errText = fmt.Sprintf("task timed out after %dms", task.GetTimeoutMs())
		} else {
			errText = execErr.Error()
		}
		eventType = "TaskFailed"
	}
	payload := taskTerminalLogPayload(task, cfg.WorkerID, status, result, errText)

	if _, err := logClient.AppendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       taskStream(task.GetTaskId()),
		EventType:      eventType,
		IdempotencyKey: task.GetTaskId() + ":completed",
		Payload:        payload,
	}); err != nil {
		return err
	}
	if _, err := controlClient.CompleteTask(ctx, &logservepb.CompleteTaskRequest{
		TaskId:         task.GetTaskId(),
		WorkerId:       cfg.WorkerID,
		Status:         status,
		ResultJson:     result,
		Error:          errText,
		ActorStateJson: actorState,
		ActorEpoch:     task.GetActorEpoch(),
		TaskLeaseEpoch: task.GetTaskLeaseEpoch(),
	}); err != nil {
		return err
	}
	return execErr
}

func taskTerminalLogPayload(task *logservepb.TaskSpec, workerID string, status logservepb.TaskStatus, result []byte, errText string) []byte {
	payload := map[string]any{
		"task_id":          task.GetTaskId(),
		"worker_id":        workerID,
		"status":           status.String(),
		"task_lease_epoch": task.GetTaskLeaseEpoch(),
		"timestamp_ms":     time.Now().UnixMilli(),
	}
	if len(result) > 0 {
		payload["result_json"] = json.RawMessage(result)
	}
	if errText != "" {
		payload["error"] = errText
	}
	data, _ := json.Marshal(payload)
	return data
}

func runExecutor(ctx context.Context, cfg Config, runner *pythonRunner, cache *modelCache, controlClient logservepb.ControlServiceClient, logClient logservepb.LogServiceClient, task *logservepb.TaskSpec) ([]byte, []byte, error) {
	if task.GetLlmModelName() != "" {
		result, err := runLLMExecutor(ctx, cfg, cache, controlClient, logClient, task)
		return result, nil, err
	}
	if task.GetActorId() != "" {
		result, state, err := runActorExecutor(ctx, runner, task)
		return result, state, err
	}
	result, err := runPythonExecutor(ctx, runner, task)
	return result, nil, err
}

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
		_, _ = controlClient.Heartbeat(ctx, &logservepb.HeartbeatRequest{
			WorkerId:     cfg.WorkerID,
			CachedModels: cache.entries(),
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

func runPythonExecutor(ctx context.Context, runner *pythonRunner, task *logservepb.TaskSpec) ([]byte, error) {
	if runner == nil {
		return nil, errors.New("python runner is required for task execution")
	}
	req := executorRequest{
		FunctionSource: task.GetFunctionSource(),
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
	if len(resp.Result) == 0 {
		return []byte("null"), nil
	}
	return append([]byte(nil), resp.Result...), nil
}

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
	if len(resp.State) == 0 {
		resp.State = []byte("{}")
	}
	return append([]byte(nil), result...), append([]byte(nil), resp.State...), nil
}

func startPythonRunner(ctx context.Context, cfg Config) (*pythonRunner, error) {
	cmd := exec.CommandContext(ctx, cfg.PythonPath, cfg.ExecutorPath, "--loop")
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
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	return &pythonRunner{
		cmd:     cmd,
		stdin:   stdin,
		scanner: scanner,
		stderr:  stderr,
	}, nil
}

func (r *pythonRunner) Execute(ctx context.Context, req any) (executorResponse, error) {
	select {
	case <-ctx.Done():
		return executorResponse{}, ctx.Err()
	default:
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	data, err := json.Marshal(req)
	if err != nil {
		return executorResponse{}, err
	}
	if _, err := r.stdin.Write(append(data, '\n')); err != nil {
		return executorResponse{}, err
	}

	type scanResult struct {
		resp executorResponse
		err  error
	}
	scanner := r.scanner
	stderr := r.stderr
	cmd := r.cmd
	done := make(chan scanResult, 1)
	go func() {
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				done <- scanResult{err: err}
				return
			}
			if stderr.Len() > 0 {
				done <- scanResult{err: errors.New(stderr.String())}
				return
			}
			done <- scanResult{err: errors.New("python executor stopped")}
			return
		}
		var resp executorResponse
		if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
			done <- scanResult{err: err}
			return
		}
		done <- scanResult{resp: resp}
	}()

	select {
	case result := <-done:
		return result.resp, result.err
	case <-ctx.Done():
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
		return executorResponse{}, ctx.Err()
	}
}

func (r *pythonRunner) Restart(ctx context.Context, cfg Config) error {
	r.mu.Lock()
	defer r.mu.Unlock()

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
	r.scanner = next.scanner
	r.stderr = next.stderr
	return nil
}

func (r *pythonRunner) Close() error {
	_ = r.stdin.Close()
	return r.cmd.Wait()
}

func taskStream(taskID string) string {
	return "task:" + taskID
}

func newModelCache(cfg Config) *modelCache {
	cache := &modelCache{
		models:        map[string]bool{},
		checkpoints:   map[string]modelCacheEntry{},
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

func (c *modelCache) has(name, version string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := modelKey(name, version)
	if c.models[key] {
		return true
	}
	if c.cacheDir == "" {
		return false
	}
	return c.indexCheckpointLocked(name, version, time.Now())
}

func (c *modelCache) add(name, version string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.models[modelKey(name, version)] = true
}

func (c *modelCache) entries() []*logservepb.ModelCacheEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entries := make([]*logservepb.ModelCacheEntry, 0, len(c.models))
	for key := range c.models {
		name, version := splitModelKey(key)
		entries = append(entries, &logservepb.ModelCacheEntry{Name: name, Version: version})
	}
	return entries
}

func (c *modelCache) loadCheckpointManifests() {
	manifestPaths, err := filepath.Glob(filepath.Join(c.cacheDir, "*.manifest.json"))
	if err != nil {
		return
	}
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
		lastAccess := time.UnixMilli(manifest.LastAccessMs)
		if manifest.LastAccessMs == 0 {
			lastAccess = info.ModTime()
		}
		key := modelKey(name, version)
		c.checkpoints[key] = modelCacheEntry{
			path:       checkpointPath,
			size:       info.Size(),
			lastAccess: lastAccess,
		}
		c.models[key] = true
	}
	c.recalculateUsedLocked()
}

func (c *modelCache) usesCheckpointStore() bool {
	return c.sourceDir != "" && c.cacheDir != ""
}

func (c *modelCache) capacity() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.capacityBytes
}

func (c *modelCache) used() int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.usedBytes
}

func (c *modelCache) ensureCheckpoint(ctx context.Context, name, version string) (checkpointLoadResult, error) {
	if !c.usesCheckpointStore() {
		return checkpointLoadResult{}, errors.New("checkpoint cache is not configured")
	}
	c.checkpointMu.Lock()
	defer c.checkpointMu.Unlock()

	key := modelKey(name, version)
	now := time.Now()

	c.mu.Lock()
	if c.indexCheckpointLocked(name, version, now) {
		entry := c.checkpoints[key]
		c.mu.Unlock()
		loadMs, err := readCheckpoint(ctx, entry.path)
		if err != nil {
			return checkpointLoadResult{}, err
		}
		return checkpointLoadResult{
			CacheHit:           true,
			ModelLoadMs:        loadMs,
			CacheUsedBytes:     c.used(),
			CacheCapacityBytes: c.capacity(),
		}, nil
	}
	c.mu.Unlock()

	sourcePath, err := c.sourcePath(name, version)
	if err != nil {
		return checkpointLoadResult{}, err
	}
	info, err := os.Stat(sourcePath)
	if err != nil {
		return checkpointLoadResult{}, err
	}
	size := info.Size()
	if c.capacityBytes > 0 && size > c.capacityBytes {
		return checkpointLoadResult{}, fmt.Errorf("checkpoint %s:%s size %d exceeds cache capacity %d", name, version, size, c.capacityBytes)
	}

	c.mu.Lock()
	evictions, err := c.evictForLocked(size)
	c.mu.Unlock()
	if err != nil {
		return checkpointLoadResult{}, err
	}

	targetPath := c.checkpointPath(name, version)
	fetchMs, err := copyCheckpoint(ctx, sourcePath, targetPath)
	if err != nil {
		return checkpointLoadResult{}, err
	}
	loadMs, err := readCheckpoint(ctx, targetPath)
	if err != nil {
		return checkpointLoadResult{}, err
	}
	lastAccess := time.Now()
	if err := writeCheckpointManifest(targetPath, name, version, size, lastAccess); err != nil {
		return checkpointLoadResult{}, err
	}

	c.mu.Lock()
	c.checkpoints[key] = modelCacheEntry{path: targetPath, size: size, lastAccess: lastAccess}
	c.models[key] = true
	c.recalculateUsedLocked()
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

func (c *modelCache) indexCheckpointLocked(name, version string, at time.Time) bool {
	key := modelKey(name, version)
	if entry, ok := c.checkpoints[key]; ok {
		if _, err := os.Stat(entry.path); err == nil {
			entry.lastAccess = at
			c.checkpoints[key] = entry
			c.models[key] = true
			_ = writeCheckpointManifest(entry.path, name, version, entry.size, at)
			return true
		}
		delete(c.checkpoints, key)
		delete(c.models, key)
		c.recalculateUsedLocked()
	}
	path := c.checkpointPath(name, version)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	c.checkpoints[key] = modelCacheEntry{path: path, size: info.Size(), lastAccess: at}
	c.models[key] = true
	c.recalculateUsedLocked()
	_ = writeCheckpointManifest(path, name, version, info.Size(), at)
	return true
}

func (c *modelCache) evictForLocked(incomingBytes int64) (int64, error) {
	if c.capacityBytes <= 0 {
		return 0, nil
	}
	var evictions int64
	for c.usedBytes+incomingBytes > c.capacityBytes && len(c.checkpoints) > 0 {
		oldestKey := ""
		var oldest modelCacheEntry
		for key, entry := range c.checkpoints {
			if oldestKey == "" || entry.lastAccess.Before(oldest.lastAccess) {
				oldestKey = key
				oldest = entry
			}
		}
		if oldestKey == "" {
			break
		}
		if err := os.Remove(oldest.path); err != nil && !os.IsNotExist(err) {
			return evictions, err
		}
		if err := os.Remove(checkpointManifestPath(oldest.path)); err != nil && !os.IsNotExist(err) {
			return evictions, err
		}
		delete(c.checkpoints, oldestKey)
		delete(c.models, oldestKey)
		c.usedBytes -= oldest.size
		if c.usedBytes < 0 {
			c.usedBytes = 0
		}
		evictions++
	}
	return evictions, nil
}

func (c *modelCache) recalculateUsedLocked() {
	var used int64
	for _, entry := range c.checkpoints {
		used += entry.size
	}
	c.usedBytes = used
}

func (c *modelCache) checkpointPath(name, version string) string {
	return filepath.Join(c.cacheDir, safeModelFileName(name, version)+".checkpoint")
}

func checkpointManifestPath(checkpointPath string) string {
	return checkpointPath + ".manifest.json"
}

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
	tmpPath := manifestPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	_ = os.Remove(manifestPath)
	if err := os.Rename(tmpPath, manifestPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func (c *modelCache) sourcePath(name, version string) (string, error) {
	version = firstNonEmpty(version, "v1")
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

func safeModelFileName(name, version string) string {
	version = firstNonEmpty(version, "v1")
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", " ", "_")
	return replacer.Replace(name) + "-" + replacer.Replace(version)
}

func copyCheckpoint(ctx context.Context, sourcePath, targetPath string) (int64, error) {
	start := time.Now()
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return 0, err
	}
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

func positiveElapsedMs(start time.Time) int64 {
	elapsed := time.Since(start).Milliseconds()
	if elapsed == 0 {
		return 1
	}
	return elapsed
}

func modelKey(name, version string) string {
	if version == "" {
		version = "v1"
	}
	return name + ":" + version
}

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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

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

func callVLLM(ctx context.Context, cfg Config, modelName, version, prompt string, maxTokens uint32) (string, error) {
	if cfg.VLLMBaseURL == "" {
		return "", errors.New("LOGSERVE_VLLM_BASE_URL is required for vllm adapter")
	}
	model := modelName
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
	if decoded.Choices[0].Message.Content != "" {
		return decoded.Choices[0].Message.Content, nil
	}
	return decoded.Choices[0].Text, nil
}
