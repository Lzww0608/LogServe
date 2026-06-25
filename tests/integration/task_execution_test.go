package integration

import (
	"context"
	"encoding/json"
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

func TestTaskExecutionEndToEnd(t *testing.T) {
	ensureExecutorDeps(t)
	root := repoRoot(t)
	logServer, err := logd.Start("127.0.0.1:0", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer logServer.Stop()

	controlServer, err := controlplane.Start("127.0.0.1:0", logServer.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer controlServer.Stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = worker.Run(ctx, worker.Config{
			WorkerID:     "test-worker",
			ControlAddr:  controlServer.Addr(),
			LogAddr:      logServer.Addr(),
			PythonPath:   "python",
			ExecutorPath: filepath.Join(root, "executor", "python", "server.py"),
			PollInterval: 20 * time.Millisecond,
		})
	}()

	conn, err := grpc.NewClient(controlServer.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	controlClient := logservepb.NewControlServiceClient(conn)

	argsJSON, _ := json.Marshal(map[string]any{"args": []int{1, 2}, "kwargs": map[string]any{}})
	submitted, err := controlClient.SubmitTask(context.Background(), &logservepb.SubmitTaskRequest{
		TaskName:       "add",
		FunctionName:   "add",
		FunctionSource: "def add(a, b):\n    return a + b\n",
		ArgsJson:       argsJSON,
		IdempotencyKey: "integration-add",
	})
	if err != nil {
		t.Fatal(err)
	}

	var status *logservepb.GetTaskStatusResponse
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, err = controlClient.GetTaskStatus(context.Background(), &logservepb.GetTaskStatusRequest{TaskId: submitted.GetTaskId()})
		if err != nil {
			t.Fatal(err)
		}
		if status.GetStatus() == logservepb.TaskStatus_TASK_STATUS_SUCCEEDED {
			break
		}
		if status.GetStatus() == logservepb.TaskStatus_TASK_STATUS_FAILED {
			t.Fatalf("task failed: %s", status.GetError())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if status.GetStatus() != logservepb.TaskStatus_TASK_STATUS_SUCCEEDED {
		t.Fatalf("status = %s, want SUCCEEDED", status.GetStatus())
	}
	if string(status.GetResultJson()) != "3" {
		t.Fatalf("result = %s, want 3", status.GetResultJson())
	}

	logConn, err := grpc.NewClient(logServer.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer logConn.Close()
	logClient := logservepb.NewLogServiceClient(logConn)
	records, err := logClient.ReadLog(context.Background(), &logservepb.ReadLogRequest{
		StreamId: "task:" + submitted.GetTaskId(),
		FromSeq:  1,
		Limit:    10,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(records.GetRecords()))
	for _, rec := range records.GetRecords() {
		got = append(got, rec.GetEventType())
	}
	want := []string{"TaskSubmitted", "TaskStarted", "TaskCompleted"}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}
}
