package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/app/controlplane"
	"github.com/logserve/logserve/internal/app/logd"
	"github.com/logserve/logserve/internal/worker"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type workflowTestEnv struct {
	root          string
	logServer     *logd.Server
	controlServer *controlplane.Server
	controlClient logservepb.ControlServiceClient
	logClient     logservepb.LogServiceClient
	controlConn   *grpc.ClientConn
	logConn       *grpc.ClientConn
}

func TestWorkflowSimpleRAGReplayAndDedup(t *testing.T) {
	env := startWorkflowEnv(t)
	defer env.stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runWorkerForTest(ctx, t, env, "workflow-worker", 0)

	submitted := submitWorkflowForTest(t, env.controlClient, simpleRAGDefinition(t))
	status := waitWorkflow(t, env.controlClient, submitted.GetWorkflowId(), logservepb.WorkflowStatus_WORKFLOW_STATUS_COMPLETED)

	if string(status.GetResultJson()) != `"answer:hello:doc:vec:hello"` {
		t.Fatalf("result = %s", status.GetResultJson())
	}
	for _, step := range status.GetSteps() {
		if step.GetStatus() != logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SUCCEEDED {
			t.Fatalf("step %s status = %s", step.GetStepId(), step.GetStatus())
		}
		if step.GetAttempts() != 1 {
			t.Fatalf("step %s attempts = %d, want 1", step.GetStepId(), step.GetAttempts())
		}
	}

	replayed, err := env.controlClient.ReplayWorkflow(context.Background(), &logservepb.ReplayWorkflowRequest{WorkflowId: submitted.GetWorkflowId()})
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.GetConsistentWithMetadata() {
		t.Fatal("replayed workflow state is not consistent with metadata")
	}

	finalStep := status.GetSteps()[len(status.GetSteps())-1]
	if _, err := env.controlClient.CompleteTask(context.Background(), &logservepb.CompleteTaskRequest{
		TaskId:     finalStep.GetTaskId(),
		WorkerId:   "duplicate-worker",
		Status:     logservepb.TaskStatus_TASK_STATUS_SUCCEEDED,
		ResultJson: status.GetResultJson(),
	}); err != nil {
		t.Fatal(err)
	}
	records, err := env.logClient.ReadLog(context.Background(), &logservepb.ReadLogRequest{
		StreamId: "wf:" + submitted.GetWorkflowId(),
		FromSeq:  1,
		Limit:    100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := countWorkflowEvent(records.GetRecords(), "WorkflowCompleted"); got != 1 {
		t.Fatalf("WorkflowCompleted events = %d, want 1", got)
	}
}

func TestWorkflowWorkerRecoveryContinuesAfterCompletedStep(t *testing.T) {
	env := startWorkflowEnv(t)
	defer env.stop()

	firstCtx, firstCancel := context.WithCancel(context.Background())
	defer firstCancel()
	done := make(chan struct{})
	go func() {
		runWorkerForTest(firstCtx, t, env, "first-worker", 1)
		close(done)
	}()

	submitted := submitWorkflowForTest(t, env.controlClient, simpleRAGDefinition(t))
	afterFirst := waitWorkflowStep(t, env.controlClient, submitted.GetWorkflowId(), "embed", logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SUCCEEDED)
	<-done

	embed := stepByID(t, afterFirst, "embed")
	if embed.GetAttempts() != 1 {
		t.Fatalf("embed attempts after first worker = %d, want 1", embed.GetAttempts())
	}

	secondCtx, secondCancel := context.WithCancel(context.Background())
	defer secondCancel()
	go runWorkerForTest(secondCtx, t, env, "second-worker", 0)

	completed := waitWorkflow(t, env.controlClient, submitted.GetWorkflowId(), logservepb.WorkflowStatus_WORKFLOW_STATUS_COMPLETED)
	if string(completed.GetResultJson()) != `"answer:hello:doc:vec:hello"` {
		t.Fatalf("result = %s", completed.GetResultJson())
	}
	if stepByID(t, completed, "embed").GetAttempts() != 1 {
		t.Fatal("embed was re-executed after worker recovery")
	}
}

func TestWorkflowRetriesFailedStep(t *testing.T) {
	env := startWorkflowEnv(t)
	defer env.stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runWorkerForTest(ctx, t, env, "retry-worker", 0)

	marker := filepath.Join(t.TempDir(), "flaky-marker")
	submitted := submitWorkflowForTest(t, env.controlClient, retryDefinition(t, marker))
	status := waitWorkflow(t, env.controlClient, submitted.GetWorkflowId(), logservepb.WorkflowStatus_WORKFLOW_STATUS_COMPLETED)

	flaky := stepByID(t, status, "flaky")
	if flaky.GetAttempts() != 2 {
		t.Fatalf("flaky attempts = %d, want 2", flaky.GetAttempts())
	}
	if string(status.GetResultJson()) != `"value-ok"` {
		t.Fatalf("result = %s", status.GetResultJson())
	}
}

func TestWorkflowRetriesTimedOutStep(t *testing.T) {
	env := startWorkflowEnv(t)
	defer env.stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runWorkerForTest(ctx, t, env, "timeout-worker", 0)

	submitted := submitWorkflowForTest(t, env.controlClient, timeoutDefinition(t))
	status := waitWorkflowTerminal(t, env.controlClient, submitted.GetWorkflowId())
	if status.GetStatus() != logservepb.WorkflowStatus_WORKFLOW_STATUS_FAILED {
		t.Fatalf("workflow status = %s, want FAILED", status.GetStatus())
	}
	step := stepByID(t, status, "slow")
	if step.GetAttempts() != 2 {
		t.Fatalf("timeout step attempts = %d, want 2", step.GetAttempts())
	}
	if step.GetStatus() != logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_FAILED {
		t.Fatalf("timeout step status = %s, want FAILED", step.GetStatus())
	}
}

func startWorkflowEnv(t *testing.T) *workflowTestEnv {
	t.Helper()
	root := repoRoot(t)
	logServer, err := logd.Start("127.0.0.1:0", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	controlServer, err := controlplane.Start("127.0.0.1:0", logServer.Addr())
	if err != nil {
		_ = logServer.Stop()
		t.Fatal(err)
	}
	controlConn, err := grpc.NewClient(controlServer.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	logConn, err := grpc.NewClient(logServer.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	return &workflowTestEnv{
		root:          root,
		logServer:     logServer,
		controlServer: controlServer,
		controlClient: logservepb.NewControlServiceClient(controlConn),
		logClient:     logservepb.NewLogServiceClient(logConn),
		controlConn:   controlConn,
		logConn:       logConn,
	}
}

func (e *workflowTestEnv) stop() {
	_ = e.controlConn.Close()
	_ = e.logConn.Close()
	_ = e.controlServer.Stop()
	_ = e.logServer.Stop()
}

func (e *workflowTestEnv) restartControl(t *testing.T) {
	t.Helper()
	_ = e.controlConn.Close()
	_ = e.controlServer.Stop()

	controlServer, err := controlplane.Start("127.0.0.1:0", e.logServer.Addr())
	if err != nil {
		t.Fatal(err)
	}
	controlConn, err := grpc.NewClient(controlServer.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		_ = controlServer.Stop()
		t.Fatal(err)
	}
	e.controlServer = controlServer
	e.controlConn = controlConn
	e.controlClient = logservepb.NewControlServiceClient(controlConn)
}

func runWorkerForTest(ctx context.Context, t *testing.T, env *workflowTestEnv, workerID string, maxTasks int) {
	t.Helper()
	ensureExecutorDeps(t)
	err := worker.Run(ctx, worker.Config{
		WorkerID:     workerID,
		ControlAddr:  env.controlServer.Addr(),
		LogAddr:      env.logServer.Addr(),
		PythonPath:   "python",
		ExecutorPath: filepath.Join(env.root, "executor", "python", "server.py"),
		PollInterval: 20 * time.Millisecond,
		MaxTasks:     maxTasks,
	})
	if err != nil && err != context.Canceled {
		t.Errorf("worker %s stopped: %v", workerID, err)
	}
}

func submitWorkflowForTest(t *testing.T, client logservepb.ControlServiceClient, def map[string]any) *logservepb.SubmitWorkflowResponse {
	t.Helper()
	data, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.SubmitWorkflow(context.Background(), &logservepb.SubmitWorkflowRequest{
		WorkflowName:   def["workflow_name"].(string),
		DefinitionJson: data,
		IdempotencyKey: fmt.Sprintf("%s-%d", def["workflow_name"], time.Now().UnixNano()),
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func waitWorkflow(t *testing.T, client logservepb.ControlServiceClient, workflowID string, want logservepb.WorkflowStatus) *logservepb.GetWorkflowStatusResponse {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	var last *logservepb.GetWorkflowStatusResponse
	for time.Now().Before(deadline) {
		resp, err := client.GetWorkflowStatus(context.Background(), &logservepb.GetWorkflowStatusRequest{WorkflowId: workflowID})
		if err != nil {
			t.Fatal(err)
		}
		last = resp
		if resp.GetStatus() == want {
			return resp
		}
		if resp.GetStatus() == logservepb.WorkflowStatus_WORKFLOW_STATUS_FAILED {
			t.Fatalf("workflow failed: %s", resp.GetError())
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("workflow status = %s, want %s; last=%v", last.GetStatus(), want, last)
	return nil
}

func waitWorkflowTerminal(t *testing.T, client logservepb.ControlServiceClient, workflowID string) *logservepb.GetWorkflowStatusResponse {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	var last *logservepb.GetWorkflowStatusResponse
	for time.Now().Before(deadline) {
		resp, err := client.GetWorkflowStatus(context.Background(), &logservepb.GetWorkflowStatusRequest{WorkflowId: workflowID})
		if err != nil {
			t.Fatal(err)
		}
		last = resp
		if resp.GetStatus() == logservepb.WorkflowStatus_WORKFLOW_STATUS_COMPLETED || resp.GetStatus() == logservepb.WorkflowStatus_WORKFLOW_STATUS_FAILED {
			return resp
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("workflow did not reach a terminal state; last=%v", last)
	return nil
}

func waitWorkflowStep(t *testing.T, client logservepb.ControlServiceClient, workflowID, stepID string, want logservepb.WorkflowStepStatus) *logservepb.GetWorkflowStatusResponse {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	var last *logservepb.GetWorkflowStatusResponse
	for time.Now().Before(deadline) {
		resp, err := client.GetWorkflowStatus(context.Background(), &logservepb.GetWorkflowStatusRequest{WorkflowId: workflowID})
		if err != nil {
			t.Fatal(err)
		}
		last = resp
		step := stepByID(t, resp, stepID)
		if step.GetStatus() == want {
			return resp
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("step %s status did not reach %s; last=%v", stepID, want, last)
	return nil
}

func stepByID(t *testing.T, status *logservepb.GetWorkflowStatusResponse, stepID string) *logservepb.WorkflowStepState {
	t.Helper()
	for _, step := range status.GetSteps() {
		if step.GetStepId() == stepID {
			return step
		}
	}
	t.Fatalf("step %s not found", stepID)
	return nil
}

func countWorkflowEvent(records []*logservepb.LogRecord, eventType string) int {
	count := 0
	for _, rec := range records {
		if rec.GetEventType() == eventType {
			count++
		}
	}
	return count
}

func simpleRAGDefinition(t *testing.T) map[string]any {
	t.Helper()
	source := `
def embed(query):
    return "vec:" + query

def search(vec):
    return ["doc:" + vec]

def generate_mock(query, docs):
    return "answer:" + query + ":" + docs[0]
`
	return map[string]any{
		"workflow_name":   "simple_rag",
		"function_source": source,
		"max_attempts":    3,
		"timeout_ms":      30000,
		"result_step_id":  "generate_mock",
		"steps": []map[string]any{
			{
				"step_id":         "embed",
				"task_name":       "embed",
				"function_name":   "embed",
				"function_source": source,
				"args_json":       map[string]any{"args": []any{"hello"}, "kwargs": map[string]any{}},
				"depends_on":      []string{},
				"max_attempts":    3,
				"timeout_ms":      30000,
			},
			{
				"step_id":         "search",
				"task_name":       "search",
				"function_name":   "search",
				"function_source": source,
				"args_json":       map[string]any{"args": []any{map[string]any{"__step_ref__": "embed"}}, "kwargs": map[string]any{}},
				"depends_on":      []string{"embed"},
				"max_attempts":    3,
				"timeout_ms":      30000,
			},
			{
				"step_id":         "generate_mock",
				"task_name":       "generate_mock",
				"function_name":   "generate_mock",
				"function_source": source,
				"args_json":       map[string]any{"args": []any{"hello", map[string]any{"__step_ref__": "search"}}, "kwargs": map[string]any{}},
				"depends_on":      []string{"search"},
				"max_attempts":    3,
				"timeout_ms":      30000,
			},
		},
	}
}

func retryDefinition(t *testing.T, marker string) map[string]any {
	t.Helper()
	source := fmt.Sprintf(`
def flaky(value):
    import os
    path = %q
    if not os.path.exists(path):
        open(path, "w", encoding="utf-8").write("failed-once")
        raise RuntimeError("first attempt fails")
    return value + "-ok"

def finish(value):
    return value
`, marker)
	return map[string]any{
		"workflow_name":   "retry_workflow",
		"function_source": source,
		"max_attempts":    2,
		"timeout_ms":      30000,
		"result_step_id":  "finish",
		"steps": []map[string]any{
			{
				"step_id":         "flaky",
				"task_name":       "flaky",
				"function_name":   "flaky",
				"function_source": source,
				"args_json":       map[string]any{"args": []any{"value"}, "kwargs": map[string]any{}},
				"depends_on":      []string{},
				"max_attempts":    2,
				"timeout_ms":      30000,
			},
			{
				"step_id":         "finish",
				"task_name":       "finish",
				"function_name":   "finish",
				"function_source": source,
				"args_json":       map[string]any{"args": []any{map[string]any{"__step_ref__": "flaky"}}, "kwargs": map[string]any{}},
				"depends_on":      []string{"flaky"},
				"max_attempts":    2,
				"timeout_ms":      30000,
			},
		},
	}
}

func timeoutDefinition(t *testing.T) map[string]any {
	t.Helper()
	source := `
def slow():
    import time
    time.sleep(0.25)
    return "late"
`
	return map[string]any{
		"workflow_name":   "timeout_workflow",
		"function_source": source,
		"max_attempts":    2,
		"timeout_ms":      50,
		"result_step_id":  "slow",
		"steps": []map[string]any{
			{
				"step_id":         "slow",
				"task_name":       "slow",
				"function_name":   "slow",
				"function_source": source,
				"args_json":       map[string]any{"args": []any{}, "kwargs": map[string]any{}},
				"depends_on":      []string{},
				"max_attempts":    2,
				"timeout_ms":      50,
			},
		},
	}
}
