package webapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type fakeClientConn struct {
	dashboard  *logservepb.DashboardSnapshot
	taskStatus *logservepb.GetTaskStatusResponse
	submitTask *logservepb.SubmitTaskResponse
	calls      []string
}

func (f *fakeClientConn) Invoke(_ context.Context, method string, _ any, reply any, _ ...grpc.CallOption) error {
	f.calls = append(f.calls, method)
	switch method {
	case logservepb.ControlService_GetDashboardSnapshot_FullMethodName:
		out, ok := reply.(*logservepb.DashboardSnapshot)
		if !ok {
			return errors.New("unexpected dashboard reply type")
		}
		if f.dashboard != nil {
			proto.Merge(out, f.dashboard)
		}
		return nil
	case logservepb.ControlService_GetTaskStatus_FullMethodName:
		out, ok := reply.(*logservepb.GetTaskStatusResponse)
		if !ok {
			return errors.New("unexpected task status reply type")
		}
		if f.taskStatus != nil {
			proto.Merge(out, f.taskStatus)
		}
		return nil
	case logservepb.ControlService_SubmitTask_FullMethodName:
		out, ok := reply.(*logservepb.SubmitTaskResponse)
		if !ok {
			return errors.New("unexpected submit task reply type")
		}
		if f.submitTask != nil {
			proto.Merge(out, f.submitTask)
		}
		return nil
	default:
		return errors.New("unexpected method: " + method)
	}
}

func (f *fakeClientConn) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, errors.New("streaming RPCs are not used by webapi tests")
}

func newTestServer(conn *fakeClientConn, token string, allowUnauthenticated bool) *Server {
	if conn == nil {
		conn = &fakeClientConn{}
	}
	s := &Server{
		cfg: Config{
			APIToken:             token,
			AllowUnauthenticated: allowUnauthenticated,
			RequestTimeout:       time.Second,
		},
		clients: &Clients{
			Control: logservepb.NewControlServiceClient(conn),
			Log:     logservepb.NewLogServiceClient(conn),
		},
		mux: http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

func TestMiddlewareRequiresBearerTokenForAPI(t *testing.T) {
	conn := &fakeClientConn{
		dashboard: &logservepb.DashboardSnapshot{
			QueueDepth:       2,
			SchedulingPolicy: logservepb.SchedulingPolicy_SCHEDULING_POLICY_LOCALITY_AWARE,
			Tasks: []*logservepb.DashboardTask{{
				TaskId:   "task-1",
				TaskName: "console_acceptance",
				Status:   logservepb.TaskStatus_TASK_STATUS_SUCCEEDED,
				WorkerId: "worker-1",
			}},
		},
	}
	srv := newTestServer(conn, "secret-token", false)

	unauthorized := httptest.NewRecorder()
	srv.Handler().ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/dashboard", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}
	if len(conn.calls) != 0 {
		t.Fatalf("unauthorized request reached backend: %v", conn.calls)
	}

	authorized := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/dashboard", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(authorized, req)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status = %d, body %s", authorized.Code, authorized.Body.String())
	}
	var payload DashboardDTO
	if err := json.Unmarshal(authorized.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode dashboard response: %v", err)
	}
	if payload.QueueDepth != 2 || len(payload.Tasks) != 1 || payload.Tasks[0].TaskName != "console_acceptance" {
		t.Fatalf("unexpected dashboard payload: %+v", payload)
	}
}

func TestHealthzBypassesAuth(t *testing.T) {
	srv := newTestServer(nil, "secret-token", false)

	resp := httptest.NewRecorder()
	srv.Handler().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/healthz", nil))

	if resp.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, body %s", resp.Code, resp.Body.String())
	}
}

func TestAllowUnauthenticatedBypassesToken(t *testing.T) {
	conn := &fakeClientConn{dashboard: &logservepb.DashboardSnapshot{}}
	srv := newTestServer(conn, "", true)

	resp := httptest.NewRecorder()
	srv.Handler().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/dashboard", nil))

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}
}

func TestDashboardSerializesEmptyCollectionsAsArrays(t *testing.T) {
	conn := &fakeClientConn{dashboard: &logservepb.DashboardSnapshot{}}
	srv := newTestServer(conn, "secret-token", false)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/dashboard", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	for _, want := range []string{`"tasks":[]`, `"workflows":[]`, `"actors":[]`, `"workers":[]`, `"models":[]`} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard response missing %s: %s", want, body)
		}
	}
	for _, notWant := range []string{`"tasks":null`, `"workflows":null`, `"actors":null`, `"workers":null`, `"models":null`} {
		if strings.Contains(body, notWant) {
			t.Fatalf("dashboard response contains %s: %s", notWant, body)
		}
	}
}

func TestDecodeJSONRejectsTrailingValue(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(`{"task_name":"x"} {"task_name":"y"}`))
	var input submitTaskRequest

	err := decodeJSON(httptest.NewRecorder(), req, &input)

	if err == nil {
		t.Fatal("expected trailing JSON error")
	}
	if !strings.Contains(err.Error(), "exactly one JSON value") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGetTaskMergesDashboardMetadata(t *testing.T) {
	conn := &fakeClientConn{
		taskStatus: &logservepb.GetTaskStatusResponse{
			TaskId:     "task-1",
			Status:     logservepb.TaskStatus_TASK_STATUS_SUCCEEDED,
			ResultJson: []byte(`3`),
		},
		dashboard: &logservepb.DashboardSnapshot{
			Tasks: []*logservepb.DashboardTask{{
				TaskId:          "task-1",
				TaskName:        "console_add",
				Status:          logservepb.TaskStatus_TASK_STATUS_SUCCEEDED,
				WorkerId:        "worker-1",
				WorkflowId:      "workflow-1",
				StepId:          "step-1",
				LlmModelName:    "model-A",
				LlmModelVersion: "v1",
			}},
		},
	}
	srv := newTestServer(conn, "secret-token", false)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/task-1", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}
	var task TaskDTO
	if err := json.Unmarshal(resp.Body.Bytes(), &task); err != nil {
		t.Fatalf("decode task response: %v", err)
	}
	if task.TaskName != "console_add" || task.WorkerID != "worker-1" || task.WorkflowID != "workflow-1" || task.StepID != "step-1" {
		t.Fatalf("metadata was not merged into task response: %+v", task)
	}
}

func TestStaticFallbackServesIndexForDeepLinks(t *testing.T) {
	dir := t.TempDir()
	index := []byte(`<!doctype html><title>LogServe Console</title><div id="root"></div>`)
	if err := os.WriteFile(filepath.Join(dir, "index.html"), index, 0o644); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(nil, "secret-token", false)
	srv.cfg.StaticDir = dir

	resp := httptest.NewRecorder()
	srv.Handler().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/tasks/task-1", nil))

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "LogServe Console") {
		t.Fatalf("static fallback body did not contain index: %s", resp.Body.String())
	}
}
