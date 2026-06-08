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
	WorkerID       string
	ControlAddr    string
	LogAddr        string
	PythonPath     string
	ExecutorPath   string
	PollInterval   time.Duration
	MaxTasks       int
	CachedModels   []string
	Capacity       uint32
	MockModelLoad  time.Duration
	MockFirstToken time.Duration
	VLLMBaseURL    string
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
	TaskID         string `json:"task_id,omitempty"`
	ModelName      string `json:"model_name,omitempty"`
	ModelVersion   string `json:"model_version,omitempty"`
	WorkerID       string `json:"worker_id,omitempty"`
	CacheHit       bool   `json:"cache_hit,omitempty"`
	ModelLoadMs    int64  `json:"model_load_ms,omitempty"`
	FirstTokenMs   int64  `json:"first_token_ms,omitempty"`
	TotalLatencyMs int64  `json:"total_latency_ms,omitempty"`
	TimestampMs    int64  `json:"timestamp_ms,omitempty"`
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
	mu     sync.RWMutex
	models map[string]bool
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
	cache := newModelCache(cfg.CachedModels)

	if _, err := controlClient.RegisterWorker(ctx, &logservepb.RegisterWorkerRequest{
		WorkerId:     cfg.WorkerID,
		CachedModels: cache.entries(),
		Capacity:     cfg.Capacity,
	}); err != nil {
		return err
	}
	observability.Info("worker_registered", map[string]any{"worker_id": cfg.WorkerID})

	runner, err := startPythonRunner(ctx, cfg)
	if err != nil {
		return err
	}
	defer runner.Close()

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()
	completedTasks := 0

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := controlClient.Heartbeat(ctx, &logservepb.HeartbeatRequest{
				WorkerId:     cfg.WorkerID,
				CachedModels: cache.entries(),
			}); err != nil {
				observability.Error("worker_heartbeat_failed", err, map[string]any{"worker_id": cfg.WorkerID})
				continue
			}
			resp, err := controlClient.PollTask(ctx, &logservepb.PollTaskRequest{WorkerId: cfg.WorkerID})
			if err != nil {
				observability.Error("worker_poll_failed", err, map[string]any{"worker_id": cfg.WorkerID})
				continue
			}
			if !resp.GetHasTask() {
				continue
			}
			if err := executeTask(ctx, cfg, runner, cache, controlClient, logClient, resp.GetTask()); err != nil {
				observability.Error("task_execution_failed", err, map[string]any{"worker_id": cfg.WorkerID, "task_id": resp.GetTask().GetTaskId()})
			}
			completedTasks++
			if cfg.MaxTasks > 0 && completedTasks >= cfg.MaxTasks {
				return nil
			}
		}
	}
}

func executeTask(ctx context.Context, cfg Config, runner *pythonRunner, cache *modelCache, controlClient logservepb.ControlServiceClient, logClient logservepb.LogServiceClient, task *logservepb.TaskSpec) error {
	if task == nil {
		return errors.New("task is nil")
	}

	startPayload, _ := json.Marshal(map[string]any{
		"task_id":          task.GetTaskId(),
		"worker_id":        cfg.WorkerID,
		"task_lease_epoch": task.GetTaskLeaseEpoch(),
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
	if errors.Is(execErr, context.DeadlineExceeded) && task.GetLlmModelName() == "" {
		if err := runner.Restart(ctx, cfg); err != nil {
			observability.Error("python_executor_restart_failed", err, map[string]any{"worker_id": cfg.WorkerID})
		}
	}
	status := logservepb.TaskStatus_TASK_STATUS_SUCCEEDED
	errText := ""
	payload := result
	eventType := "TaskCompleted"
	if execErr != nil {
		status = logservepb.TaskStatus_TASK_STATUS_FAILED
		if errors.Is(execErr, context.DeadlineExceeded) && task.GetTimeoutMs() > 0 {
			errText = fmt.Sprintf("task timed out after %dms", task.GetTimeoutMs())
		} else {
			errText = execErr.Error()
		}
		payload, _ = json.Marshal(map[string]string{"error": errText})
		eventType = "TaskFailed"
	}

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
	if !cacheHit && adapter == "mock" {
		if err := sleepContext(ctx, cfg.MockModelLoad); err != nil {
			return nil, err
		}
	}
	loadMs := int64(0)
	if !cacheHit {
		loadMs = time.Since(loadStart).Milliseconds()
		if loadMs == 0 {
			loadMs = 1
		}
		cache.add(modelName, version)
		_, _ = controlClient.Heartbeat(ctx, &logservepb.HeartbeatRequest{
			WorkerId:     cfg.WorkerID,
			CachedModels: cache.entries(),
		})
	}
	if err := appendLLMEvent(ctx, logClient, task, cfg.WorkerID, "ModelLoaded", llmEventPayload{
		CacheHit:    cacheHit,
		ModelLoadMs: loadMs,
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
		CacheHit:       cacheHit,
		ModelLoadMs:    loadMs,
		FirstTokenMs:   firstTokenMs,
		TotalLatencyMs: totalMs,
	}); err != nil {
		return nil, err
	}
	return json.Marshal(text)
}

func runPythonExecutor(ctx context.Context, runner *pythonRunner, task *logservepb.TaskSpec) ([]byte, error) {
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

func newModelCache(models []string) *modelCache {
	cache := &modelCache{models: map[string]bool{}}
	for _, model := range models {
		name, version := splitModelKey(model)
		if name == "" {
			continue
		}
		cache.models[modelKey(name, version)] = true
	}
	return cache
}

func (c *modelCache) has(name, version string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.models[modelKey(name, version)]
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
