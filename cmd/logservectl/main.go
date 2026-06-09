package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type submitInput struct {
	TaskName       string          `json:"task_name"`
	FunctionName   string          `json:"function_name"`
	FunctionSource string          `json:"function_source"`
	Args           json.RawMessage `json:"args"`
	Kwargs         json.RawMessage `json:"kwargs"`
	IdempotencyKey string          `json:"idempotency_key"`
}

type submitOutput struct {
	TaskID string          `json:"task_id"`
	Status string          `json:"status"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type workflowSubmitInput struct {
	WorkflowName   string          `json:"workflow_name"`
	Definition     json.RawMessage `json:"definition"`
	IdempotencyKey string          `json:"idempotency_key"`
}

type workflowOutput struct {
	WorkflowID string                          `json:"workflow_id"`
	Status     string                          `json:"status"`
	Result     json.RawMessage                 `json:"result,omitempty"`
	ResultRef  string                          `json:"result_ref,omitempty"`
	Error      string                          `json:"error,omitempty"`
	Steps      []*logservepb.WorkflowStepState `json:"steps,omitempty"`
	Consistent bool                            `json:"consistent_with_metadata,omitempty"`
}

type actorCreateInput struct {
	ClassName      string          `json:"class_name"`
	ClassSource    string          `json:"class_source"`
	InitArgs       json.RawMessage `json:"init_args"`
	InitKwargs     json.RawMessage `json:"init_kwargs"`
	IdempotencyKey string          `json:"idempotency_key"`
	SnapshotEvery  uint32          `json:"snapshot_every"`
}

type actorCallInput struct {
	ActorID        string          `json:"actor_id"`
	MethodName     string          `json:"method_name"`
	Args           json.RawMessage `json:"args"`
	Kwargs         json.RawMessage `json:"kwargs"`
	IdempotencyKey string          `json:"idempotency_key"`
	TimeoutMs      int64           `json:"timeout_ms"`
}

type modelRegisterInput struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	SizeBytes uint64 `json:"size_bytes"`
	Path      string `json:"path"`
	Adapter   string `json:"adapter"`
}

type schedulerPolicyInput struct {
	Policy string `json:"policy"`
}

type llmSubmitInput struct {
	ModelName      string `json:"model_name"`
	ModelVersion   string `json:"model_version"`
	Prompt         string `json:"prompt"`
	MaxTokens      uint32 `json:"max_tokens"`
	Adapter        string `json:"adapter"`
	IdempotencyKey string `json:"idempotency_key"`
}

type backpressureInput struct {
	QueueHighWatermark  uint32 `json:"queue_high_watermark"`
	RedeliveryTimeoutMs int64  `json:"redelivery_timeout_ms"`
	LogAppendSlowMs     int64  `json:"log_append_slow_ms"`
}

type llmOutput struct {
	TaskID             string                 `json:"task_id"`
	Status             string                 `json:"status,omitempty"`
	Result             json.RawMessage        `json:"result,omitempty"`
	Error              string                 `json:"error,omitempty"`
	WorkerID           string                 `json:"worker_id,omitempty"`
	ModelName          string                 `json:"model_name,omitempty"`
	ModelVersion       string                 `json:"model_version,omitempty"`
	CacheHit           bool                   `json:"cache_hit,omitempty"`
	ModelLoadMs        int64                  `json:"model_load_ms,omitempty"`
	CheckpointFetchMs  int64                  `json:"checkpoint_fetch_ms,omitempty"`
	FirstTokenMs       int64                  `json:"first_token_ms,omitempty"`
	TotalLatencyMs     int64                  `json:"total_latency_ms,omitempty"`
	CacheUsedBytes     int64                  `json:"cache_used_bytes,omitempty"`
	CacheCapacityBytes int64                  `json:"cache_capacity_bytes,omitempty"`
	EvictionCount      int64                  `json:"eviction_count,omitempty"`
	Events             []*logservepb.LLMEvent `json:"events,omitempty"`
}

type actorOutput struct {
	ActorID                string          `json:"actor_id"`
	CallID                 string          `json:"call_id,omitempty"`
	ClassName              string          `json:"class_name,omitempty"`
	Status                 string          `json:"status"`
	OwnerWorkerID          string          `json:"owner_worker_id,omitempty"`
	Epoch                  uint64          `json:"epoch,omitempty"`
	CommandCount           uint64          `json:"command_count,omitempty"`
	SnapshotRef            string          `json:"snapshot_ref,omitempty"`
	SnapshotCommandCount   uint64          `json:"snapshot_command_count,omitempty"`
	Result                 json.RawMessage `json:"result,omitempty"`
	State                  json.RawMessage `json:"state,omitempty"`
	Error                  string          `json:"error,omitempty"`
	Consistent             bool            `json:"consistent_with_metadata,omitempty"`
	FullReplayCommands     uint64          `json:"full_replay_commands,omitempty"`
	SnapshotReplayCommands uint64          `json:"snapshot_replay_commands,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		fatal(errors.New("usage: logservectl <submit|status|workflow-submit|workflow-status|workflow-replay|model-register|scheduler-policy|llm-submit|llm-replay|backpressure-set|dashboard-snapshot|actor-create|actor-call|actor-status|actor-replay>"))
	}
	switch os.Args[1] {
	case "submit":
		if err := submit(os.Args[2:]); err != nil {
			fatal(err)
		}
	case "status":
		if err := status(os.Args[2:]); err != nil {
			fatal(err)
		}
	case "workflow-submit":
		if err := workflowSubmit(os.Args[2:]); err != nil {
			fatal(err)
		}
	case "workflow-status":
		if err := workflowStatus(os.Args[2:]); err != nil {
			fatal(err)
		}
	case "workflow-replay":
		if err := workflowReplay(os.Args[2:]); err != nil {
			fatal(err)
		}
	case "model-register":
		if err := modelRegister(os.Args[2:]); err != nil {
			fatal(err)
		}
	case "scheduler-policy":
		if err := schedulerPolicy(os.Args[2:]); err != nil {
			fatal(err)
		}
	case "llm-submit":
		if err := llmSubmit(os.Args[2:]); err != nil {
			fatal(err)
		}
	case "llm-replay":
		if err := llmReplay(os.Args[2:]); err != nil {
			fatal(err)
		}
	case "backpressure-set":
		if err := backpressureSet(os.Args[2:]); err != nil {
			fatal(err)
		}
	case "dashboard-snapshot":
		if err := dashboardSnapshot(os.Args[2:]); err != nil {
			fatal(err)
		}
	case "actor-create":
		if err := actorCreate(os.Args[2:]); err != nil {
			fatal(err)
		}
	case "actor-call":
		if err := actorCall(os.Args[2:]); err != nil {
			fatal(err)
		}
	case "actor-status":
		if err := actorStatus(os.Args[2:]); err != nil {
			fatal(err)
		}
	case "actor-replay":
		if err := actorReplay(os.Args[2:]); err != nil {
			fatal(err)
		}
	default:
		fatal(fmt.Errorf("unknown command %q", os.Args[1]))
	}
}

func submit(args []string) error {
	fs := flag.NewFlagSet("submit", flag.ContinueOnError)
	controlAddr := fs.String("control-addr", getenv("LOGSERVE_CONTROL_ADDR", "127.0.0.1:50052"), "control service address")
	timeout := fs.Duration("timeout", 30*time.Second, "submit wait timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	var input submitInput
	if err := json.Unmarshal(data, &input); err != nil {
		return err
	}
	argsJSON, err := json.Marshal(map[string]json.RawMessage{
		"args":   defaultRaw(input.Args, []byte("[]")),
		"kwargs": defaultRaw(input.Kwargs, []byte("{}")),
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	conn, err := grpc.NewClient(*controlAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := logservepb.NewControlServiceClient(conn)

	resp, err := client.SubmitTask(ctx, &logservepb.SubmitTaskRequest{
		TaskName:       input.TaskName,
		FunctionName:   input.FunctionName,
		FunctionSource: input.FunctionSource,
		ArgsJson:       argsJSON,
		IdempotencyKey: input.IdempotencyKey,
	})
	if err != nil {
		return err
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			statusResp, err := client.GetTaskStatus(ctx, &logservepb.GetTaskStatusRequest{TaskId: resp.GetTaskId()})
			if err != nil {
				return err
			}
			switch statusResp.GetStatus() {
			case logservepb.TaskStatus_TASK_STATUS_SUCCEEDED:
				return writeJSON(submitOutput{TaskID: resp.GetTaskId(), Status: "SUCCEEDED", Result: statusResp.GetResultJson()})
			case logservepb.TaskStatus_TASK_STATUS_FAILED:
				return writeJSON(submitOutput{TaskID: resp.GetTaskId(), Status: "FAILED", Error: statusResp.GetError()})
			}
		}
	}
}

func status(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	controlAddr := fs.String("control-addr", getenv("LOGSERVE_CONTROL_ADDR", "127.0.0.1:50052"), "control service address")
	taskID := fs.String("task-id", "", "task id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *taskID == "" {
		return errors.New("task-id is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(*controlAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := logservepb.NewControlServiceClient(conn)
	resp, err := client.GetTaskStatus(ctx, &logservepb.GetTaskStatusRequest{TaskId: *taskID})
	if err != nil {
		return err
	}
	return writeJSON(resp)
}

func workflowSubmit(args []string) error {
	fs := flag.NewFlagSet("workflow-submit", flag.ContinueOnError)
	controlAddr := fs.String("control-addr", getenv("LOGSERVE_CONTROL_ADDR", "127.0.0.1:50052"), "control service address")
	timeout := fs.Duration("timeout", 60*time.Second, "workflow wait timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	var input workflowSubmitInput
	if err := json.Unmarshal(data, &input); err != nil {
		return err
	}
	if input.WorkflowName == "" {
		return errors.New("workflow_name is required")
	}
	if len(input.Definition) == 0 {
		return errors.New("definition is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	conn, err := grpc.NewClient(*controlAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := logservepb.NewControlServiceClient(conn)

	resp, err := client.SubmitWorkflow(ctx, &logservepb.SubmitWorkflowRequest{
		WorkflowName:   input.WorkflowName,
		DefinitionJson: input.Definition,
		IdempotencyKey: input.IdempotencyKey,
	})
	if err != nil {
		return err
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			statusResp, err := client.GetWorkflowStatus(ctx, &logservepb.GetWorkflowStatusRequest{WorkflowId: resp.GetWorkflowId()})
			if err != nil {
				return err
			}
			switch statusResp.GetStatus() {
			case logservepb.WorkflowStatus_WORKFLOW_STATUS_COMPLETED:
				return writeJSON(workflowOutput{
					WorkflowID: resp.GetWorkflowId(),
					Status:     "COMPLETED",
					Result:     statusResp.GetResultJson(),
					ResultRef:  statusResp.GetResultRef(),
					Steps:      statusResp.GetSteps(),
				})
			case logservepb.WorkflowStatus_WORKFLOW_STATUS_FAILED:
				return writeJSON(workflowOutput{
					WorkflowID: resp.GetWorkflowId(),
					Status:     "FAILED",
					Error:      statusResp.GetError(),
					Steps:      statusResp.GetSteps(),
				})
			}
		}
	}
}

func workflowStatus(args []string) error {
	fs := flag.NewFlagSet("workflow-status", flag.ContinueOnError)
	controlAddr := fs.String("control-addr", getenv("LOGSERVE_CONTROL_ADDR", "127.0.0.1:50052"), "control service address")
	workflowID := fs.String("workflow-id", "", "workflow id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *workflowID == "" {
		return errors.New("workflow-id is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(*controlAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := logservepb.NewControlServiceClient(conn)
	resp, err := client.GetWorkflowStatus(ctx, &logservepb.GetWorkflowStatusRequest{WorkflowId: *workflowID})
	if err != nil {
		return err
	}
	return writeJSON(resp)
}

func workflowReplay(args []string) error {
	fs := flag.NewFlagSet("workflow-replay", flag.ContinueOnError)
	controlAddr := fs.String("control-addr", getenv("LOGSERVE_CONTROL_ADDR", "127.0.0.1:50052"), "control service address")
	workflowID := fs.String("workflow-id", "", "workflow id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *workflowID == "" {
		return errors.New("workflow-id is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(*controlAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := logservepb.NewControlServiceClient(conn)
	resp, err := client.ReplayWorkflow(ctx, &logservepb.ReplayWorkflowRequest{WorkflowId: *workflowID})
	if err != nil {
		return err
	}
	replayed := resp.GetReplayed()
	return writeJSON(workflowOutput{
		WorkflowID: replayed.GetWorkflowId(),
		Status:     replayed.GetStatus().String(),
		Result:     replayed.GetResultJson(),
		ResultRef:  replayed.GetResultRef(),
		Error:      replayed.GetError(),
		Steps:      replayed.GetSteps(),
		Consistent: resp.GetConsistentWithMetadata(),
	})
}

func modelRegister(args []string) error {
	fs := flag.NewFlagSet("model-register", flag.ContinueOnError)
	controlAddr := fs.String("control-addr", getenv("LOGSERVE_CONTROL_ADDR", "127.0.0.1:50052"), "control service address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	var input modelRegisterInput
	if err := json.Unmarshal(data, &input); err != nil {
		return err
	}
	if input.Name == "" {
		return errors.New("name is required")
	}
	if input.Version == "" {
		input.Version = "v1"
	}
	if input.Adapter == "" {
		input.Adapter = "mock"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(*controlAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := logservepb.NewControlServiceClient(conn)
	resp, err := client.RegisterModel(ctx, &logservepb.RegisterModelRequest{Model: &logservepb.ModelInfo{
		Name:      input.Name,
		Version:   input.Version,
		SizeBytes: input.SizeBytes,
		Path:      input.Path,
		Adapter:   input.Adapter,
	}})
	if err != nil {
		return err
	}
	return writeJSON(resp.GetModel())
}

func schedulerPolicy(args []string) error {
	fs := flag.NewFlagSet("scheduler-policy", flag.ContinueOnError)
	controlAddr := fs.String("control-addr", getenv("LOGSERVE_CONTROL_ADDR", "127.0.0.1:50052"), "control service address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	var input schedulerPolicyInput
	if err := json.Unmarshal(data, &input); err != nil {
		return err
	}
	policy, err := parseSchedulingPolicy(input.Policy)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(*controlAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := logservepb.NewControlServiceClient(conn)
	resp, err := client.SetSchedulingPolicy(ctx, &logservepb.SetSchedulingPolicyRequest{Policy: policy})
	if err != nil {
		return err
	}
	return writeJSON(map[string]string{"policy": schedulingPolicyString(resp.GetPolicy())})
}

func llmSubmit(args []string) error {
	fs := flag.NewFlagSet("llm-submit", flag.ContinueOnError)
	controlAddr := fs.String("control-addr", getenv("LOGSERVE_CONTROL_ADDR", "127.0.0.1:50052"), "control service address")
	timeout := fs.Duration("timeout", 60*time.Second, "llm wait timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	var input llmSubmitInput
	if err := json.Unmarshal(data, &input); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	conn, err := grpc.NewClient(*controlAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := logservepb.NewControlServiceClient(conn)
	resp, err := client.SubmitLLM(ctx, &logservepb.SubmitLLMRequest{
		ModelName:      input.ModelName,
		ModelVersion:   input.ModelVersion,
		Prompt:         input.Prompt,
		MaxTokens:      input.MaxTokens,
		Adapter:        input.Adapter,
		IdempotencyKey: input.IdempotencyKey,
	})
	if err != nil {
		return err
	}

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			statusResp, err := client.GetTaskStatus(ctx, &logservepb.GetTaskStatusRequest{TaskId: resp.GetTaskId()})
			if err != nil {
				return err
			}
			switch statusResp.GetStatus() {
			case logservepb.TaskStatus_TASK_STATUS_SUCCEEDED:
				return writeJSON(llmOutput{
					TaskID:   resp.GetTaskId(),
					Status:   "SUCCEEDED",
					Result:   statusResp.GetResultJson(),
					WorkerID: statusResp.GetWorkerId(),
				})
			case logservepb.TaskStatus_TASK_STATUS_FAILED:
				return writeJSON(llmOutput{
					TaskID:   resp.GetTaskId(),
					Status:   "FAILED",
					Error:    statusResp.GetError(),
					WorkerID: statusResp.GetWorkerId(),
				})
			}
		}
	}
}

func llmReplay(args []string) error {
	fs := flag.NewFlagSet("llm-replay", flag.ContinueOnError)
	controlAddr := fs.String("control-addr", getenv("LOGSERVE_CONTROL_ADDR", "127.0.0.1:50052"), "control service address")
	taskID := fs.String("task-id", "", "llm task id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *taskID == "" {
		return errors.New("task-id is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(*controlAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := logservepb.NewControlServiceClient(conn)
	resp, err := client.ReplayLLM(ctx, &logservepb.ReplayLLMRequest{TaskId: *taskID})
	if err != nil {
		return err
	}
	return writeJSON(llmOutput{
		TaskID:             resp.GetTaskId(),
		ModelName:          resp.GetModelName(),
		ModelVersion:       resp.GetModelVersion(),
		WorkerID:           resp.GetWorkerId(),
		CacheHit:           resp.GetCacheHit(),
		ModelLoadMs:        resp.GetModelLoadMs(),
		CheckpointFetchMs:  resp.GetCheckpointFetchMs(),
		FirstTokenMs:       resp.GetFirstTokenMs(),
		TotalLatencyMs:     resp.GetTotalLatencyMs(),
		CacheUsedBytes:     resp.GetCacheUsedBytes(),
		CacheCapacityBytes: resp.GetCacheCapacityBytes(),
		EvictionCount:      resp.GetEvictionCount(),
		Events:             resp.GetEvents(),
	})
}

func backpressureSet(args []string) error {
	fs := flag.NewFlagSet("backpressure-set", flag.ContinueOnError)
	controlAddr := fs.String("control-addr", getenv("LOGSERVE_CONTROL_ADDR", "127.0.0.1:50052"), "control service address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	var input backpressureInput
	if err := json.Unmarshal(data, &input); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(*controlAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := logservepb.NewControlServiceClient(conn)
	resp, err := client.SetBackpressure(ctx, &logservepb.SetBackpressureRequest{
		QueueHighWatermark:  input.QueueHighWatermark,
		RedeliveryTimeoutMs: input.RedeliveryTimeoutMs,
		LogAppendSlowMs:     input.LogAppendSlowMs,
	})
	if err != nil {
		return err
	}
	return writeJSON(resp)
}

func dashboardSnapshot(args []string) error {
	fs := flag.NewFlagSet("dashboard-snapshot", flag.ContinueOnError)
	controlAddr := fs.String("control-addr", getenv("LOGSERVE_CONTROL_ADDR", "127.0.0.1:50052"), "control service address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(*controlAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := logservepb.NewControlServiceClient(conn)
	resp, err := client.GetDashboardSnapshot(ctx, &logservepb.GetDashboardSnapshotRequest{})
	if err != nil {
		return err
	}
	return writeJSON(resp)
}

func actorCreate(args []string) error {
	fs := flag.NewFlagSet("actor-create", flag.ContinueOnError)
	controlAddr := fs.String("control-addr", getenv("LOGSERVE_CONTROL_ADDR", "127.0.0.1:50052"), "control service address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	var input actorCreateInput
	if err := json.Unmarshal(data, &input); err != nil {
		return err
	}
	initJSON, err := json.Marshal(map[string]json.RawMessage{
		"args":   defaultRaw(input.InitArgs, []byte("[]")),
		"kwargs": defaultRaw(input.InitKwargs, []byte("{}")),
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(*controlAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := logservepb.NewControlServiceClient(conn)
	resp, err := client.CreateActor(ctx, &logservepb.CreateActorRequest{
		ClassName:      input.ClassName,
		ClassSource:    input.ClassSource,
		InitArgsJson:   initJSON,
		IdempotencyKey: input.IdempotencyKey,
		SnapshotEvery:  input.SnapshotEvery,
	})
	if err != nil {
		return err
	}
	return writeJSON(actorOutput{
		ActorID:       resp.GetActorId(),
		Status:        actorStatusString(resp.GetStatus()),
		OwnerWorkerID: resp.GetOwnerWorkerId(),
		Epoch:         resp.GetEpoch(),
	})
}

func actorCall(args []string) error {
	fs := flag.NewFlagSet("actor-call", flag.ContinueOnError)
	controlAddr := fs.String("control-addr", getenv("LOGSERVE_CONTROL_ADDR", "127.0.0.1:50052"), "control service address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	var input actorCallInput
	if err := json.Unmarshal(data, &input); err != nil {
		return err
	}
	argsJSON, err := json.Marshal(map[string]json.RawMessage{
		"args":   defaultRaw(input.Args, []byte("[]")),
		"kwargs": defaultRaw(input.Kwargs, []byte("{}")),
	})
	if err != nil {
		return err
	}
	timeout := time.Duration(input.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout+5*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(*controlAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := logservepb.NewControlServiceClient(conn)
	resp, err := client.CallActor(ctx, &logservepb.CallActorRequest{
		ActorId:        input.ActorID,
		MethodName:     input.MethodName,
		ArgsJson:       argsJSON,
		IdempotencyKey: input.IdempotencyKey,
		TimeoutMs:      input.TimeoutMs,
	})
	if err != nil {
		return err
	}
	out := actorOutput{
		ActorID: resp.GetActorId(),
		CallID:  resp.GetCallId(),
		Status:  taskStatusString(resp.GetStatus()),
		Result:  resp.GetResultJson(),
		Error:   resp.GetError(),
		Epoch:   resp.GetEpoch(),
	}
	if resp.GetStatus() == logservepb.TaskStatus_TASK_STATUS_FAILED {
		return writeJSON(out)
	}
	return writeJSON(out)
}

func actorStatus(args []string) error {
	fs := flag.NewFlagSet("actor-status", flag.ContinueOnError)
	controlAddr := fs.String("control-addr", getenv("LOGSERVE_CONTROL_ADDR", "127.0.0.1:50052"), "control service address")
	actorID := fs.String("actor-id", "", "actor id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *actorID == "" {
		return errors.New("actor-id is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(*controlAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := logservepb.NewControlServiceClient(conn)
	resp, err := client.GetActorStatus(ctx, &logservepb.GetActorStatusRequest{ActorId: *actorID})
	if err != nil {
		return err
	}
	return writeJSON(actorStatusOutput(resp))
}

func actorReplay(args []string) error {
	fs := flag.NewFlagSet("actor-replay", flag.ContinueOnError)
	controlAddr := fs.String("control-addr", getenv("LOGSERVE_CONTROL_ADDR", "127.0.0.1:50052"), "control service address")
	actorID := fs.String("actor-id", "", "actor id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *actorID == "" {
		return errors.New("actor-id is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.NewClient(*controlAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := logservepb.NewControlServiceClient(conn)
	resp, err := client.ReplayActor(ctx, &logservepb.ReplayActorRequest{ActorId: *actorID})
	if err != nil {
		return err
	}
	out := actorStatusOutput(resp.GetReplayed())
	out.Consistent = resp.GetConsistentWithMetadata()
	out.FullReplayCommands = resp.GetFullReplayCommands()
	out.SnapshotReplayCommands = resp.GetSnapshotReplayCommands()
	return writeJSON(out)
}

func writeJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func defaultRaw(value json.RawMessage, fallback []byte) json.RawMessage {
	if len(value) == 0 {
		return fallback
	}
	return value
}

func taskStatusString(status logservepb.TaskStatus) string {
	switch status {
	case logservepb.TaskStatus_TASK_STATUS_QUEUED:
		return "QUEUED"
	case logservepb.TaskStatus_TASK_STATUS_RUNNING:
		return "RUNNING"
	case logservepb.TaskStatus_TASK_STATUS_SUCCEEDED:
		return "SUCCEEDED"
	case logservepb.TaskStatus_TASK_STATUS_FAILED:
		return "FAILED"
	default:
		return "UNSPECIFIED"
	}
}

func actorStatusString(status logservepb.ActorStatus) string {
	switch status {
	case logservepb.ActorStatus_ACTOR_STATUS_ACTIVE:
		return "ACTIVE"
	case logservepb.ActorStatus_ACTOR_STATUS_UNAVAILABLE:
		return "UNAVAILABLE"
	default:
		return "UNSPECIFIED"
	}
}

func parseSchedulingPolicy(value string) (logservepb.SchedulingPolicy, error) {
	switch value {
	case "", "LOCALITY_AWARE", "locality-aware", "locality_aware":
		return logservepb.SchedulingPolicy_SCHEDULING_POLICY_LOCALITY_AWARE, nil
	case "RESOURCE_ONLY", "resource-only", "resource_only":
		return logservepb.SchedulingPolicy_SCHEDULING_POLICY_RESOURCE_ONLY, nil
	case "PREDICTED_LATENCY", "predicted-latency", "predicted_latency":
		return logservepb.SchedulingPolicy_SCHEDULING_POLICY_PREDICTED_LATENCY, nil
	default:
		return logservepb.SchedulingPolicy_SCHEDULING_POLICY_UNSPECIFIED, fmt.Errorf("unknown scheduling policy %q", value)
	}
}

func schedulingPolicyString(policy logservepb.SchedulingPolicy) string {
	switch policy {
	case logservepb.SchedulingPolicy_SCHEDULING_POLICY_RESOURCE_ONLY:
		return "RESOURCE_ONLY"
	case logservepb.SchedulingPolicy_SCHEDULING_POLICY_LOCALITY_AWARE:
		return "LOCALITY_AWARE"
	case logservepb.SchedulingPolicy_SCHEDULING_POLICY_PREDICTED_LATENCY:
		return "PREDICTED_LATENCY"
	default:
		return "UNSPECIFIED"
	}
}

func actorStatusOutput(resp *logservepb.GetActorStatusResponse) actorOutput {
	return actorOutput{
		ActorID:              resp.GetActorId(),
		ClassName:            resp.GetClassName(),
		Status:               actorStatusString(resp.GetStatus()),
		OwnerWorkerID:        resp.GetOwnerWorkerId(),
		Epoch:                resp.GetEpoch(),
		CommandCount:         resp.GetCommandCount(),
		SnapshotRef:          resp.GetSnapshotRef(),
		SnapshotCommandCount: resp.GetSnapshotCommandCount(),
		State:                resp.GetStateJson(),
	}
}
