package webapi

// This file exercises the web API through a fake gRPC client and real HTTP
// handlers, covering auth, RBAC, SSE, templates, and DTO behavior.

import (
	"bufio"
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
	"github.com/logserve/logserve/internal/eventcodec"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// fakeClientConn is an in-memory grpc.ClientConnInterface used to exercise webapi
// handlers without starting real control or log services.
type fakeClientConn struct {
	dashboard          *logservepb.DashboardSnapshot
	taskStatus         *logservepb.GetTaskStatusResponse
	workflowStatus     *logservepb.GetWorkflowStatusResponse
	submitTask         *logservepb.SubmitTaskResponse
	submitTaskReqs     []*logservepb.SubmitTaskRequest
	streams            *logservepb.ListStreamsResponse
	readLog            *logservepb.ReadLogResponse
	stats              *logservepb.GetStreamStatsResponse
	listStreamsReq     *logservepb.ListStreamsRequest
	readLogReq         *logservepb.ReadLogRequest
	readLogReqs        []*logservepb.ReadLogRequest
	statsReqs          []*logservepb.GetStreamStatsRequest
	setBackpressureReq *logservepb.SetBackpressureRequest
	appendLogReqs      []*logservepb.AppendLogRequest
	calls              []string
}

// Invoke records the method/request and fills canned protobuf responses for the
// subset of RPCs covered by webapi tests.
func (f *fakeClientConn) Invoke(_ context.Context, method string, req any, reply any, _ ...grpc.CallOption) error {
	if method == logservepb.LogService_AppendLog_FullMethodName {
		if in, ok := req.(*logservepb.AppendLogRequest); ok {
			f.appendLogReqs = append(f.appendLogReqs, proto.Clone(in).(*logservepb.AppendLogRequest))
		}
		if out, ok := reply.(*logservepb.AppendLogResponse); ok {
			out.Seq = uint64(len(f.appendLogReqs))
			out.TimestampMs = int64(len(f.appendLogReqs))
		}
		return nil
	}
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
	case logservepb.ControlService_GetWorkflowStatus_FullMethodName:
		out, ok := reply.(*logservepb.GetWorkflowStatusResponse)
		if !ok {
			return errors.New("unexpected workflow status reply type")
		}
		if f.workflowStatus != nil {
			proto.Merge(out, f.workflowStatus)
		}
		return nil
	case logservepb.ControlService_SubmitTask_FullMethodName:
		if in, ok := req.(*logservepb.SubmitTaskRequest); ok {
			f.submitTaskReqs = append(f.submitTaskReqs, proto.Clone(in).(*logservepb.SubmitTaskRequest))
		}
		out, ok := reply.(*logservepb.SubmitTaskResponse)
		if !ok {
			return errors.New("unexpected submit task reply type")
		}
		if f.submitTask != nil {
			proto.Merge(out, f.submitTask)
		}
		return nil
	case logservepb.ControlService_SetBackpressure_FullMethodName:
		in, ok := req.(*logservepb.SetBackpressureRequest)
		if !ok {
			return errors.New("unexpected set backpressure request type")
		}
		out, ok := reply.(*logservepb.SetBackpressureResponse)
		if !ok {
			return errors.New("unexpected set backpressure reply type")
		}
		f.setBackpressureReq = proto.Clone(in).(*logservepb.SetBackpressureRequest)
		out.QueueHighWatermark = in.GetQueueHighWatermark()
		out.RedeliveryTimeoutMs = in.GetRedeliveryTimeoutMs()
		out.LogAppendSlowMs = in.GetLogAppendSlowMs()
		if f.dashboard == nil {
			f.dashboard = &logservepb.DashboardSnapshot{}
		}
		f.dashboard.QueueHighWatermark = out.GetQueueHighWatermark()
		f.dashboard.RedeliveryTimeoutMs = out.GetRedeliveryTimeoutMs()
		f.dashboard.LogAppendSlowMs = out.GetLogAppendSlowMs()
		return nil
	case logservepb.LogService_ListStreams_FullMethodName:
		if in, ok := req.(*logservepb.ListStreamsRequest); ok {
			f.listStreamsReq = proto.Clone(in).(*logservepb.ListStreamsRequest)
		}
		out, ok := reply.(*logservepb.ListStreamsResponse)
		if !ok {
			return errors.New("unexpected list streams reply type")
		}
		if f.streams != nil {
			proto.Merge(out, f.streams)
		}
		return nil
	case logservepb.LogService_ReadLog_FullMethodName:
		in, ok := req.(*logservepb.ReadLogRequest)
		if ok {
			f.readLogReq = proto.Clone(in).(*logservepb.ReadLogRequest)
			f.readLogReqs = append(f.readLogReqs, proto.Clone(in).(*logservepb.ReadLogRequest))
		}
		out, ok := reply.(*logservepb.ReadLogResponse)
		if !ok {
			return errors.New("unexpected read log reply type")
		}
		if f.readLog != nil {
			limit := int(in.GetLimit())
			if limit <= 0 {
				limit = len(f.readLog.GetRecords())
			}
			fromSeq := in.GetFromSeq()
			for _, record := range f.readLog.GetRecords() {
				if record.GetSeq() < fromSeq {
					continue
				}
				out.Records = append(out.Records, proto.Clone(record).(*logservepb.LogRecord))
				if len(out.Records) >= limit {
					break
				}
			}
		}
		return nil
	case logservepb.LogService_GetStreamStats_FullMethodName:
		if in, ok := req.(*logservepb.GetStreamStatsRequest); ok {
			f.statsReqs = append(f.statsReqs, proto.Clone(in).(*logservepb.GetStreamStatsRequest))
		}
		out, ok := reply.(*logservepb.GetStreamStatsResponse)
		if !ok {
			return errors.New("unexpected stream stats reply type")
		}
		if f.stats != nil {
			proto.Merge(out, f.stats)
		}
		return nil
	default:
		return errors.New("unexpected method: " + method)
	}
}

// NewStream reports unsupported streaming because these tests exercise unary
// client calls through HTTP handlers.
func (f *fakeClientConn) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, errors.New("streaming RPCs are not used by webapi tests")
}

// newTestServer wires a Server around fake control/log clients for handler tests.
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
		mux:                   http.NewServeMux(),
		functionRegistryCache: newFunctionRegistryCache(),
	}
	s.registerRoutes()
	return s
}

// TestMiddlewareRequiresBearerTokenForAPI verifies API routes reject missing or
// incorrect bearer tokens and accept the configured token.
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

// TestHealthzBypassesAuth verifies the health endpoint remains available without
// API credentials.
func TestHealthzBypassesAuth(t *testing.T) {
	srv := newTestServer(nil, "secret-token", false)

	resp := httptest.NewRecorder()
	srv.Handler().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/healthz", nil))

	if resp.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, body %s", resp.Code, resp.Body.String())
	}
}

// TestAllowUnauthenticatedBypassesToken verifies local development mode bypasses
// bearer-token checks.
func TestAllowUnauthenticatedBypassesToken(t *testing.T) {
	conn := &fakeClientConn{dashboard: &logservepb.DashboardSnapshot{}}
	srv := newTestServer(conn, "", true)

	resp := httptest.NewRecorder()
	srv.Handler().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/dashboard", nil))

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}
}

// TestDashboardSerializesEmptyCollectionsAsArrays locks down non-null JSON arrays
// for dashboard collection fields.
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

// TestDecodeJSONRejectsTrailingValue verifies strict request-body decoding rejects
// multiple top-level JSON values.
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

// TestGetTaskMergesDashboardMetadata ensures task detail responses combine
// authoritative status with dashboard-only metadata.
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

// TestListTasksFiltersAndPaginates covers task status filtering, search, and
// pagination metadata.
func TestListTasksFiltersAndPaginates(t *testing.T) {
	conn := &fakeClientConn{
		dashboard: &logservepb.DashboardSnapshot{Tasks: []*logservepb.DashboardTask{
			{TaskId: "task-1", TaskName: "alpha", Status: logservepb.TaskStatus_TASK_STATUS_QUEUED, WorkerId: "worker-a", WorkflowId: "wf-1"},
			{TaskId: "task-2", TaskName: "needle", Status: logservepb.TaskStatus_TASK_STATUS_SUCCEEDED, WorkerId: "worker-a", WorkflowId: "wf-1"},
			{TaskId: "task-3", TaskName: "needle-second", Status: logservepb.TaskStatus_TASK_STATUS_SUCCEEDED, WorkerId: "worker-a", WorkflowId: "wf-1"},
			{TaskId: "task-4", TaskName: "needle", Status: logservepb.TaskStatus_TASK_STATUS_SUCCEEDED, WorkerId: "worker-b", WorkflowId: "wf-1"},
			{TaskId: "task-5", TaskName: "needle", Status: logservepb.TaskStatus_TASK_STATUS_SUCCEEDED, WorkerId: "worker-a", WorkflowId: "wf-2"},
		}},
	}
	srv := newTestServer(conn, "secret-token", false)

	first := httptest.NewRecorder()
	firstReq := httptest.NewRequest(http.MethodGet, "/api/tasks?status=succeeded&q=needle&worker_id=worker-a&workflow_id=wf-1&limit=1", nil)
	firstReq.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(first, firstReq)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, body %s", first.Code, first.Body.String())
	}
	var firstPayload struct {
		Tasks         []TaskDTO `json:"tasks"`
		Limit         int       `json:"limit"`
		TotalCount    int       `json:"total_count"`
		NextPageToken string    `json:"next_page_token"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstPayload); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	if len(firstPayload.Tasks) != 1 || firstPayload.Tasks[0].TaskID != "task-2" || firstPayload.Limit != 1 || firstPayload.TotalCount != 2 || firstPayload.NextPageToken != "1" {
		t.Fatalf("first page = %+v", firstPayload)
	}

	second := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodGet, "/api/tasks?status=succeeded&q=needle&worker_id=worker-a&workflow_id=wf-1&limit=1&page_token="+firstPayload.NextPageToken, nil)
	secondReq.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(second, secondReq)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, body %s", second.Code, second.Body.String())
	}
	var secondPayload struct {
		Tasks         []TaskDTO `json:"tasks"`
		TotalCount    int       `json:"total_count"`
		NextPageToken string    `json:"next_page_token"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondPayload); err != nil {
		t.Fatalf("decode second response: %v", err)
	}
	if len(secondPayload.Tasks) != 1 || secondPayload.Tasks[0].TaskID != "task-3" || secondPayload.TotalCount != 2 || secondPayload.NextPageToken != "" {
		t.Fatalf("second page = %+v", secondPayload)
	}
}

// TestListPaginationRejectsInvalidQuery verifies shared pagination validation is
// surfaced as a bad request.
func TestListPaginationRejectsInvalidQuery(t *testing.T) {
	cases := []string{
		"/api/tasks?limit=0",
		"/api/tasks?limit=101",
		"/api/workflows?page_token=-1",
		"/api/workflows?page_token=not-a-number",
	}
	for _, target := range cases {
		conn := &fakeClientConn{dashboard: &logservepb.DashboardSnapshot{}}
		srv := newTestServer(conn, "secret-token", false)
		resp := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Authorization", "Bearer secret-token")
		srv.Handler().ServeHTTP(resp, req)
		if resp.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, body %s", target, resp.Code, resp.Body.String())
		}
	}
}

// TestTaskOperationRequiresToken verifies retry/resubmit endpoints remain protected
// by API authentication.
func TestTaskOperationRequiresToken(t *testing.T) {
	conn := &fakeClientConn{}
	srv := newTestServer(conn, "secret-token", false)

	resp := httptest.NewRecorder()
	srv.Handler().ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/tasks/task-1/retry", nil))

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}
	if len(conn.calls) != 0 {
		t.Fatalf("unauthorized task operation reached backend: %v", conn.calls)
	}
}

// TestRetryFailedTaskSubmitsOriginalSpecAsNewTask verifies retry recovers the
// original TaskSubmitted spec and submits a fresh task.
func TestRetryFailedTaskSubmitsOriginalSpecAsNewTask(t *testing.T) {
	conn := &fakeClientConn{
		taskStatus: &logservepb.GetTaskStatusResponse{TaskId: "task-1", Status: logservepb.TaskStatus_TASK_STATUS_FAILED, Error: "boom"},
		submitTask: &logservepb.SubmitTaskResponse{TaskId: "task-retry", Status: logservepb.TaskStatus_TASK_STATUS_QUEUED},
		readLog: &logservepb.ReadLogResponse{Records: []*logservepb.LogRecord{
			taskSubmittedRecord(t, &logservepb.TaskSpec{
				TaskId:         "task-1",
				TaskName:       "add",
				FunctionName:   "add_fn",
				FunctionSource: "def add(a, b): return a + b",
				ArgsJson:       []byte(`[1,2]`),
				IdempotencyKey: "original-key",
			}),
		}},
	}
	srv := newTestServer(conn, "secret-token", false)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/task-1/retry", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}
	var task TaskDTO
	if err := json.Unmarshal(resp.Body.Bytes(), &task); err != nil {
		t.Fatalf("decode retry response: %v", err)
	}
	if task.TaskID != "task-retry" || task.Status != "QUEUED" {
		t.Fatalf("retry task = %+v, want new queued task", task)
	}
	if len(conn.submitTaskReqs) != 1 {
		t.Fatalf("submit requests = %d, calls %v", len(conn.submitTaskReqs), conn.calls)
	}
	submitted := conn.submitTaskReqs[0]
	if submitted.GetTaskName() != "add" || submitted.GetFunctionName() != "add_fn" || submitted.GetFunctionSource() == "" || string(submitted.GetArgsJson()) != `[1,2]` {
		t.Fatalf("submitted request did not preserve original spec: %+v", submitted)
	}
	if submitted.GetIdempotencyKey() == "" || submitted.GetIdempotencyKey() == "original-key" || !strings.Contains(submitted.GetIdempotencyKey(), "retry:task-1") {
		t.Fatalf("retry idempotency key = %q, want retry key distinct from original", submitted.GetIdempotencyKey())
	}
	if conn.readLogReq.GetStreamId() != "task:task-1" || conn.readLogReq.GetFromSeq() != 1 {
		t.Fatalf("read log request = %+v", conn.readLogReq)
	}
}

// TestResubmitTaskSubmitsOriginalSpecAsNewTask verifies resubmit works from the
// original submitted spec without requiring failed status.
func TestResubmitTaskSubmitsOriginalSpecAsNewTask(t *testing.T) {
	conn := &fakeClientConn{
		taskStatus: &logservepb.GetTaskStatusResponse{TaskId: "task-1", Status: logservepb.TaskStatus_TASK_STATUS_SUCCEEDED},
		submitTask: &logservepb.SubmitTaskResponse{TaskId: "task-resubmitted", Status: logservepb.TaskStatus_TASK_STATUS_QUEUED},
		readLog: &logservepb.ReadLogResponse{Records: []*logservepb.LogRecord{
			taskSubmittedRecord(t, &logservepb.TaskSpec{
				TaskId:       "task-1",
				TaskName:     "registered",
				FunctionName: "module:handler",
				FunctionRef:  "s3://functions/add.py",
				FunctionHash: "sha256:abc",
				ArgsJson:     []byte(`{"x":1}`),
			}),
		}},
	}
	srv := newTestServer(conn, "secret-token", false)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/task-1/resubmit", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}
	if len(conn.submitTaskReqs) != 1 {
		t.Fatalf("submit requests = %d, calls %v", len(conn.submitTaskReqs), conn.calls)
	}
	submitted := conn.submitTaskReqs[0]
	if submitted.GetFunctionRef() != "s3://functions/add.py" || submitted.GetFunctionHash() != "sha256:abc" || !strings.Contains(submitted.GetIdempotencyKey(), "resubmit:task-1") {
		t.Fatalf("resubmit request = %+v", submitted)
	}
}

// TestRetryRejectsNonFailedTask ensures retry is limited to failed tasks.
func TestRetryRejectsNonFailedTask(t *testing.T) {
	conn := &fakeClientConn{taskStatus: &logservepb.GetTaskStatusResponse{TaskId: "task-1", Status: logservepb.TaskStatus_TASK_STATUS_RUNNING}}
	srv := newTestServer(conn, "secret-token", false)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/task-1/retry", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(resp, req)

	if resp.Code != http.StatusConflict {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}
	if len(conn.submitTaskReqs) != 0 {
		t.Fatalf("retry submitted non-failed task: %+v", conn.submitTaskReqs)
	}
}

// TestTaskOperationRejectsDerivedTask verifies retry/resubmit reject workflow,
// actor, LLM, or internally scheduled task specs.
func TestTaskOperationRejectsDerivedTask(t *testing.T) {
	conn := &fakeClientConn{
		taskStatus: &logservepb.GetTaskStatusResponse{TaskId: "task-step", Status: logservepb.TaskStatus_TASK_STATUS_FAILED},
		readLog: &logservepb.ReadLogResponse{Records: []*logservepb.LogRecord{
			taskSubmittedRecord(t, &logservepb.TaskSpec{
				TaskId:         "task-step",
				TaskName:       "wf-step",
				FunctionName:   "step",
				FunctionSource: "def step(): pass",
				WorkflowId:     "wf-1",
				StepId:         "step-1",
			}),
		}},
	}
	srv := newTestServer(conn, "secret-token", false)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/task-step/resubmit", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(resp, req)

	if resp.Code != http.StatusConflict {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}
	if len(conn.submitTaskReqs) != 0 {
		t.Fatalf("derived task operation submitted request: %+v", conn.submitTaskReqs)
	}
}

// TestCancelTaskReturnsUnsupported documents the current unsupported cancellation
// endpoint behavior.
func TestCancelTaskReturnsUnsupported(t *testing.T) {
	conn := &fakeClientConn{}
	srv := newTestServer(conn, "secret-token", false)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/task-1/cancel", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(resp, req)

	if resp.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "UNSUPPORTED_OPERATION") {
		t.Fatalf("cancel response should expose unsupported operation code: %s", resp.Body.String())
	}
	if len(conn.calls) != 0 {
		t.Fatalf("cancel should not call backend while unsupported: %v", conn.calls)
	}
}

// taskSubmittedRecord builds an encoded TaskSubmitted log record for retry and
// resubmit tests.
func taskSubmittedRecord(t *testing.T, spec *logservepb.TaskSpec) *logservepb.LogRecord {
	t.Helper()
	specData, err := proto.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal task spec: %v", err)
	}
	payload, err := eventcodec.Marshal(eventcodec.KindTaskSubmitted, map[string]any{"task_spec": specData})
	if err != nil {
		t.Fatalf("marshal task submitted payload: %v", err)
	}
	return &logservepb.LogRecord{
		StreamId:  "task:" + spec.GetTaskId(),
		Seq:       1,
		EventType: "TaskSubmitted",
		Payload:   payload,
	}
}

// TestListWorkflowsFiltersAndPaginates covers workflow status filtering and shared
// pagination metadata.
func TestListWorkflowsFiltersAndPaginates(t *testing.T) {
	conn := &fakeClientConn{
		dashboard: &logservepb.DashboardSnapshot{Workflows: []*logservepb.DashboardWorkflow{
			{WorkflowId: "wf-1", WorkflowName: "running", Status: logservepb.WorkflowStatus_WORKFLOW_STATUS_RUNNING},
			{WorkflowId: "wf-2", WorkflowName: "done-a", Status: logservepb.WorkflowStatus_WORKFLOW_STATUS_COMPLETED},
			{WorkflowId: "wf-3", WorkflowName: "done-b", Status: logservepb.WorkflowStatus_WORKFLOW_STATUS_COMPLETED},
		}},
	}
	srv := newTestServer(conn, "secret-token", false)

	first := httptest.NewRecorder()
	firstReq := httptest.NewRequest(http.MethodGet, "/api/workflows?status=completed&limit=1", nil)
	firstReq.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(first, firstReq)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, body %s", first.Code, first.Body.String())
	}
	var firstPayload struct {
		Workflows     []WorkflowDTO `json:"workflows"`
		Limit         int           `json:"limit"`
		TotalCount    int           `json:"total_count"`
		NextPageToken string        `json:"next_page_token"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstPayload); err != nil {
		t.Fatalf("decode first response: %v", err)
	}
	if len(firstPayload.Workflows) != 1 || firstPayload.Workflows[0].WorkflowID != "wf-2" || firstPayload.Limit != 1 || firstPayload.TotalCount != 2 || firstPayload.NextPageToken != "1" {
		t.Fatalf("first workflow page = %+v", firstPayload)
	}

	second := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodGet, "/api/workflows?status=completed&limit=1&page_token="+firstPayload.NextPageToken, nil)
	secondReq.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(second, secondReq)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, body %s", second.Code, second.Body.String())
	}
	var secondPayload struct {
		Workflows     []WorkflowDTO `json:"workflows"`
		TotalCount    int           `json:"total_count"`
		NextPageToken string        `json:"next_page_token"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondPayload); err != nil {
		t.Fatalf("decode second response: %v", err)
	}
	if len(secondPayload.Workflows) != 1 || secondPayload.Workflows[0].WorkflowID != "wf-3" || secondPayload.TotalCount != 2 || secondPayload.NextPageToken != "" {
		t.Fatalf("second workflow page = %+v", secondPayload)
	}
}

// TestWorkflowStepsExposeDependenciesForDAGRendering verifies dependency fields
// survive DTO conversion for the workflow graph UI.
func TestWorkflowStepsExposeDependenciesForDAGRendering(t *testing.T) {
	conn := &fakeClientConn{
		dashboard: &logservepb.DashboardSnapshot{
			Workflows: []*logservepb.DashboardWorkflow{{
				WorkflowId:   "wf-1",
				WorkflowName: "rag",
				Status:       logservepb.WorkflowStatus_WORKFLOW_STATUS_RUNNING,
				Steps: []*logservepb.WorkflowStepState{{
					StepId: "search",
					Status: logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SCHEDULED,
					DependsOn: []string{
						"embed",
					},
				}},
			}},
		},
	}
	srv := newTestServer(conn, "secret-token", false)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/workflows", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}
	var payload struct {
		Workflows []WorkflowDTO `json:"workflows"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode workflows response: %v", err)
	}
	if got := payload.Workflows[0].Steps[0].DependsOn; len(got) != 1 || got[0] != "embed" {
		t.Fatalf("depends_on = %v, want [embed]", got)
	}
}

// TestLogExplorerListsStreamsAndReadsRecords covers stream listing, stats, and
// payload representation in log explorer responses.
func TestLogExplorerListsStreamsAndReadsRecords(t *testing.T) {
	conn := &fakeClientConn{
		streams: &logservepb.ListStreamsResponse{StreamIds: []string{"system:functions"}},
		readLog: &logservepb.ReadLogResponse{Records: []*logservepb.LogRecord{{
			StreamId:       "system:functions",
			Seq:            1,
			EventType:      "FunctionRegistered",
			IdempotencyKey: "fn-1",
			Payload:        []byte(`{"function_hash":"abc"}`),
			TimestampMs:    1234,
			Crc32:          99,
		}}},
		stats: &logservepb.GetStreamStatsResponse{Streams: []*logservepb.StreamStats{{
			StreamId:           "system:functions",
			FirstSeq:           1,
			NextSeq:            2,
			TrimmedBeforeSeq:   0,
			CompactableRecords: 0,
			CompactableBytes:   0,
		}}},
	}
	srv := newTestServer(conn, "secret-token", false)

	listResp := httptest.NewRecorder()
	listReq := httptest.NewRequest(http.MethodGet, "/api/logs/streams?prefix=system:", nil)
	listReq.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(listResp, listReq)
	if listResp.Code != http.StatusOK {
		t.Fatalf("list status = %d, body %s", listResp.Code, listResp.Body.String())
	}
	if !strings.Contains(listResp.Body.String(), `"stream_ids":["system:functions"]`) {
		t.Fatalf("list response missing stream id: %s", listResp.Body.String())
	}
	if conn.listStreamsReq.GetPrefix() != "system:" {
		t.Fatalf("list prefix = %q, want system:", conn.listStreamsReq.GetPrefix())
	}

	readResp := httptest.NewRecorder()
	readReq := httptest.NewRequest(http.MethodGet, "/api/logs/streams/system%3Afunctions?from_seq=1&limit=10", nil)
	readReq.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(readResp, readReq)
	if readResp.Code != http.StatusOK {
		t.Fatalf("read status = %d, body %s", readResp.Code, readResp.Body.String())
	}
	body := readResp.Body.String()
	for _, want := range []string{
		`"stream_id":"system:functions"`,
		`"event_type":"FunctionRegistered"`,
		`"payload_json":{"function_hash":"abc"}`,
		`"stats":{"stream_id":"system:functions"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("read response missing %s: %s", want, body)
		}
	}
	if conn.readLogReq.GetStreamId() != "system:functions" || conn.readLogReq.GetFromSeq() != 1 || conn.readLogReq.GetLimit() != 10 {
		t.Fatalf("read request = %+v, want stream system:functions from_seq 1 limit 10", conn.readLogReq)
	}
	if got := conn.statsReqs[len(conn.statsReqs)-1].GetStreamId(); got != "system:functions" {
		t.Fatalf("stats stream_id = %q, want system:functions", got)
	}
}

// TestLogExplorerReturnsPaginationMetadata verifies next_seq and has_more are
// computed from log records plus stream stats.
func TestLogExplorerReturnsPaginationMetadata(t *testing.T) {
	conn := &fakeClientConn{
		readLog: &logservepb.ReadLogResponse{Records: []*logservepb.LogRecord{
			{StreamId: "system:functions", Seq: 1, EventType: "A"},
			{StreamId: "system:functions", Seq: 2, EventType: "B"},
			{StreamId: "system:functions", Seq: 3, EventType: "C"},
		}},
		stats: &logservepb.GetStreamStatsResponse{Streams: []*logservepb.StreamStats{{
			StreamId: "system:functions",
			FirstSeq: 1,
			NextSeq:  4,
		}}},
	}
	srv := newTestServer(conn, "secret-token", false)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/logs/streams/system%3Afunctions?from_seq=1&limit=2", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}
	var payload struct {
		Records []logRecordDTO `json:"records"`
		NextSeq uint64         `json:"next_seq"`
		HasMore bool           `json:"has_more"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode log response: %v", err)
	}
	if len(payload.Records) != 2 || payload.Records[0].Seq != 1 || payload.Records[1].Seq != 2 || payload.NextSeq != 3 || !payload.HasMore {
		t.Fatalf("log page = %+v", payload)
	}
}

// TestLogExplorerRejectsHugeLimit checks the log explorer limit guard.
func TestLogExplorerRejectsHugeLimit(t *testing.T) {
	conn := &fakeClientConn{}
	srv := newTestServer(conn, "secret-token", false)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/logs/streams/system%3Afunctions?limit=1001", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(resp, req)

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}
	if len(conn.calls) != 0 {
		t.Fatalf("invalid request reached backend: %v", conn.calls)
	}
}

// TestLogExplorerDoesNotDoubleDecodeStreamID prevents path values containing
// percent escapes from being decoded twice.
func TestLogExplorerDoesNotDoubleDecodeStreamID(t *testing.T) {
	streamID := "system:%2Fraw"
	conn := &fakeClientConn{
		readLog: &logservepb.ReadLogResponse{Records: []*logservepb.LogRecord{{
			StreamId:  streamID,
			Seq:       1,
			EventType: "RawPercent",
		}}},
		stats: &logservepb.GetStreamStatsResponse{Streams: []*logservepb.StreamStats{{
			StreamId: streamID,
			FirstSeq: 1,
			NextSeq:  2,
		}}},
	}
	srv := newTestServer(conn, "secret-token", false)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/logs/streams/system%3A%252Fraw?from_seq=2&limit=7", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}
	if conn.readLogReq.GetStreamId() != streamID || conn.readLogReq.GetFromSeq() != 2 || conn.readLogReq.GetLimit() != 7 {
		t.Fatalf("read request = %+v, want stream %q from_seq 2 limit 7", conn.readLogReq, streamID)
	}
}

// TestListFunctionsFromRegistryStream verifies function registry entries are read
// from the system:functions log stream.
func TestListFunctionsFromRegistryStream(t *testing.T) {
	conn := &fakeClientConn{readLog: &logservepb.ReadLogResponse{Records: []*logservepb.LogRecord{
		{
			StreamId:    "system:functions",
			Seq:         1,
			EventType:   "TaskSubmitted",
			Payload:     []byte(`{"task_id":"task-1"}`),
			TimestampMs: 1000,
		},
		{
			StreamId:    "system:functions",
			Seq:         2,
			EventType:   "FunctionRegistered",
			Payload:     []byte(`{"function_hash":"sha256:old","source_ref":"s3://functions/old.py","entrypoint":"module:old","language":"python","timestamp_ms":2000}`),
			TimestampMs: 2000,
		},
		{
			StreamId:    "system:functions",
			Seq:         3,
			EventType:   "FunctionRegistered",
			Payload:     []byte(`{"function_hash":"sha256:new","source_ref":"s3://functions/new.py","entrypoint":"module:new","language":"python"}`),
			TimestampMs: 3000,
		},
		{
			StreamId:    "system:functions",
			Seq:         4,
			EventType:   "FunctionRegistered",
			Payload:     []byte(`{"function_hash":"sha256:old","source_ref":"s3://functions/old-v2.py","entrypoint":"module:old","language":"python","timestamp_ms":4000}`),
			TimestampMs: 4000,
		},
	}}}
	srv := newTestServer(conn, "secret-token", false)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/functions", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}
	var payload struct {
		Functions []FunctionDTO `json:"functions"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode functions response: %v", err)
	}
	if len(payload.Functions) != 2 {
		t.Fatalf("functions length = %d, payload %+v", len(payload.Functions), payload.Functions)
	}
	if payload.Functions[0].FunctionHash != "sha256:old" || payload.Functions[0].SourceRef != "s3://functions/old-v2.py" {
		t.Fatalf("first function = %+v, want latest old registration", payload.Functions[0])
	}
	if payload.Functions[1].FunctionHash != "sha256:new" || payload.Functions[1].TimestampMs != 3000 {
		t.Fatalf("second function = %+v, want timestamp fallback from log record", payload.Functions[1])
	}
	if conn.readLogReq.GetStreamId() != "system:functions" || conn.readLogReq.GetFromSeq() != 1 || conn.readLogReq.GetLimit() != maxLogReadLimit {
		t.Fatalf("read registry request = %+v", conn.readLogReq)
	}
}

// TestFunctionRegistryCachesAndTailsRecords verifies incremental tailing from the
// cached next sequence instead of rereading the whole registry stream.
func TestFunctionRegistryCachesAndTailsRecords(t *testing.T) {
	conn := &fakeClientConn{readLog: &logservepb.ReadLogResponse{Records: []*logservepb.LogRecord{{
		StreamId:    "system:functions",
		Seq:         1,
		EventType:   "FunctionRegistered",
		Payload:     []byte(`{"function_hash":"sha256:a","source_ref":"s3://functions/a.py","entrypoint":"module:a","language":"python","timestamp_ms":1000}`),
		TimestampMs: 1000,
	}, {
		StreamId:    "system:functions",
		Seq:         2,
		EventType:   "FunctionRegistered",
		Payload:     []byte(`{"function_hash":"sha256:b","source_ref":"s3://functions/b.py","entrypoint":"module:b","language":"python","timestamp_ms":2000}`),
		TimestampMs: 2000,
	}}}}
	srv := newTestServer(conn, "secret-token", false)

	first := httptest.NewRecorder()
	firstReq := httptest.NewRequest(http.MethodGet, "/api/functions", nil)
	firstReq.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(first, firstReq)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, body %s", first.Code, first.Body.String())
	}

	conn.readLog.Records = append(conn.readLog.Records, &logservepb.LogRecord{
		StreamId:    "system:functions",
		Seq:         3,
		EventType:   "FunctionRegistered",
		Payload:     []byte(`{"function_hash":"sha256:c","source_ref":"s3://functions/c.py","entrypoint":"module:c","language":"python","timestamp_ms":3000}`),
		TimestampMs: 3000,
	})

	second := httptest.NewRecorder()
	secondReq := httptest.NewRequest(http.MethodGet, "/api/functions", nil)
	secondReq.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(second, secondReq)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, body %s", second.Code, second.Body.String())
	}
	if len(conn.readLogReqs) != 2 {
		t.Fatalf("read log request count = %d, requests %+v", len(conn.readLogReqs), conn.readLogReqs)
	}
	if conn.readLogReqs[0].GetFromSeq() != 1 || conn.readLogReqs[1].GetFromSeq() != 3 {
		t.Fatalf("read log from seqs = %d, %d; want 1 then 3", conn.readLogReqs[0].GetFromSeq(), conn.readLogReqs[1].GetFromSeq())
	}
	var payload struct {
		Functions []FunctionDTO `json:"functions"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode second response: %v", err)
	}
	if len(payload.Functions) != 3 || payload.Functions[0].FunctionHash != "sha256:c" {
		t.Fatalf("second functions = %+v, want cached a/b plus tailed c first", payload.Functions)
	}
}

// TestListFunctionsSkipsMalformedRegistryRecord verifies malformed registry events
// are counted and skipped rather than failing the whole listing.
func TestListFunctionsSkipsMalformedRegistryRecord(t *testing.T) {
	conn := &fakeClientConn{readLog: &logservepb.ReadLogResponse{Records: []*logservepb.LogRecord{{
		StreamId:  "system:functions",
		Seq:       1,
		EventType: "FunctionRegistered",
		Payload:   []byte(`{"function_hash":`),
	}, {
		StreamId:    "system:functions",
		Seq:         2,
		EventType:   "FunctionRegistered",
		Payload:     []byte(`{"function_hash":"sha256:valid","source_ref":"s3://functions/valid.py","entrypoint":"module:valid","language":"python"}`),
		TimestampMs: 2000,
	}}}}
	srv := newTestServer(conn, "secret-token", false)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/functions", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}
	var payload struct {
		Functions      []FunctionDTO `json:"functions"`
		InvalidRecords uint64        `json:"invalid_records"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode functions response: %v", err)
	}
	if payload.InvalidRecords != 1 {
		t.Fatalf("invalid_records = %d, want 1", payload.InvalidRecords)
	}
	if len(payload.Functions) != 1 || payload.Functions[0].FunctionHash != "sha256:valid" {
		t.Fatalf("functions = %+v, want valid record only", payload.Functions)
	}
}

// TestGetFunctionByHash verifies lookup of one function registry entry.
func TestGetFunctionByHash(t *testing.T) {
	conn := &fakeClientConn{readLog: &logservepb.ReadLogResponse{Records: []*logservepb.LogRecord{{
		StreamId:    "system:functions",
		Seq:         1,
		EventType:   "FunctionRegistered",
		Payload:     []byte(`{"function_hash":"sha256:abc","source_ref":"s3://functions/abc.py","entrypoint":"module:add","language":"python","timestamp_ms":1234}`),
		TimestampMs: 1234,
	}}}}
	srv := newTestServer(conn, "secret-token", false)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/functions/sha256%3Aabc", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}
	var function FunctionDTO
	if err := json.Unmarshal(resp.Body.Bytes(), &function); err != nil {
		t.Fatalf("decode function response: %v", err)
	}
	if function.FunctionHash != "sha256:abc" || function.SourceRef != "s3://functions/abc.py" || function.Entrypoint != "module:add" || function.Language != "python" {
		t.Fatalf("function response = %+v", function)
	}
}

// TestGetFunctionReturnsNotFound verifies unknown function hashes use the API error
// path.
func TestGetFunctionReturnsNotFound(t *testing.T) {
	conn := &fakeClientConn{readLog: &logservepb.ReadLogResponse{Records: []*logservepb.LogRecord{{
		StreamId:  "system:functions",
		Seq:       1,
		EventType: "FunctionRegistered",
		Payload:   []byte(`{"function_hash":"sha256:abc","source_ref":"s3://functions/abc.py","entrypoint":"module:add","language":"python"}`),
	}}}}
	srv := newTestServer(conn, "secret-token", false)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/functions/sha256%3Amissing", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(resp, req)

	if resp.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}
}

// TestListFunctionsRequiresToken verifies function registry endpoints are not
// publicly accessible.
func TestListFunctionsRequiresToken(t *testing.T) {
	conn := &fakeClientConn{}
	srv := newTestServer(conn, "secret-token", false)

	resp := httptest.NewRecorder()
	srv.Handler().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/functions", nil))

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}
	if len(conn.calls) != 0 {
		t.Fatalf("unauthorized request reached backend: %v", conn.calls)
	}
}

// TestEventsRequiresToken verifies SSE endpoints are protected by authentication.
func TestEventsRequiresToken(t *testing.T) {
	conn := &fakeClientConn{}
	srv := newTestServer(conn, "secret-token", false)

	resp := httptest.NewRecorder()
	srv.Handler().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/events", nil))

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}
	if len(conn.calls) != 0 {
		t.Fatalf("unauthorized request reached backend: %v", conn.calls)
	}
}

// TestDashboardEventsStreamInitialSnapshot verifies dashboard SSE sends an initial
// snapshot event.
func TestDashboardEventsStreamInitialSnapshot(t *testing.T) {
	conn := &fakeClientConn{dashboard: &logservepb.DashboardSnapshot{
		QueueDepth:          3,
		QueueHighWatermark:  10,
		SchedulingPolicy:    logservepb.SchedulingPolicy_SCHEDULING_POLICY_RESOURCE_ONLY,
		RedeliveryTimeoutMs: 30000,
		LogAppendSlowMs:     100,
		Tasks: []*logservepb.DashboardTask{{
			TaskId:   "task-1",
			TaskName: "add",
			Status:   logservepb.TaskStatus_TASK_STATUS_RUNNING,
		}},
	}}
	resp := openSSE(t, newTestServer(conn, "secret-token", false), "/api/events?interval_ms=10")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if contentType := resp.Header.Get("Content-Type"); !strings.Contains(contentType, "text/event-stream") {
		t.Fatalf("content type = %q, want text/event-stream", contentType)
	}
	event, data := readSSEEvent(t, resp)
	if event != "dashboard" {
		t.Fatalf("event = %q, data %s", event, data)
	}
	var payload struct {
		Dashboard DashboardDTO `json:"dashboard"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("decode event payload: %v; data %s", err, data)
	}
	if payload.Dashboard.QueueDepth != 3 || len(payload.Dashboard.Tasks) != 1 || payload.Dashboard.Tasks[0].TaskID != "task-1" {
		t.Fatalf("dashboard event payload = %+v", payload.Dashboard)
	}
}

// TestTaskEventsStreamByTaskID verifies task-specific SSE subscriptions emit task
// payloads.
func TestTaskEventsStreamByTaskID(t *testing.T) {
	conn := &fakeClientConn{
		taskStatus: &logservepb.GetTaskStatusResponse{
			TaskId:     "task-1",
			Status:     logservepb.TaskStatus_TASK_STATUS_SUCCEEDED,
			ResultJson: []byte(`{"ok":true}`),
		},
		dashboard: &logservepb.DashboardSnapshot{Tasks: []*logservepb.DashboardTask{{
			TaskId:     "task-1",
			TaskName:   "console_add",
			Status:     logservepb.TaskStatus_TASK_STATUS_SUCCEEDED,
			WorkflowId: "wf-1",
			StepId:     "step-1",
		}}},
	}
	resp := openSSE(t, newTestServer(conn, "secret-token", false), "/api/events?task_id=task-1&interval_ms=10")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	event, data := readSSEEvent(t, resp)
	if event != "task" {
		t.Fatalf("event = %q, data %s", event, data)
	}
	var payload struct {
		Task TaskDTO `json:"task"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("decode event payload: %v; data %s", err, data)
	}
	if payload.Task.TaskID != "task-1" || payload.Task.TaskName != "console_add" || payload.Task.WorkflowID != "wf-1" {
		t.Fatalf("task event payload = %+v", payload.Task)
	}
}

// TestWorkflowEventsStreamFromWorkflowStream verifies wf: stream shortcuts emit
// workflow summary events by default.
func TestWorkflowEventsStreamFromWorkflowStream(t *testing.T) {
	conn := &fakeClientConn{workflowStatus: &logservepb.GetWorkflowStatusResponse{
		WorkflowId:   "wf-1",
		WorkflowName: "rag",
		Status:       logservepb.WorkflowStatus_WORKFLOW_STATUS_RUNNING,
		Steps: []*logservepb.WorkflowStepState{{
			StepId:   "search",
			TaskName: "search_docs",
			Status:   logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SUCCEEDED,
			TaskId:   "task-search",
		}},
	}}
	resp := openSSE(t, newTestServer(conn, "secret-token", false), "/api/events?stream=wf%3Awf-1&interval_ms=10")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	event, data := readSSEEvent(t, resp)
	if event != "workflow" {
		t.Fatalf("event = %q, data %s", event, data)
	}
	var payload struct {
		Workflow WorkflowDTO `json:"workflow"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("decode event payload: %v; data %s", err, data)
	}
	if payload.Workflow.WorkflowID != "wf-1" || len(payload.Workflow.Steps) != 1 || payload.Workflow.Steps[0].TaskID != "task-search" {
		t.Fatalf("workflow event payload = %+v", payload.Workflow)
	}
}

// TestLogEventsStreamAppendsRecordsFromStream verifies raw log-record SSE advances
// its sequence cursor after each batch.
func TestLogEventsStreamAppendsRecordsFromStream(t *testing.T) {
	conn := &fakeClientConn{readLog: &logservepb.ReadLogResponse{Records: []*logservepb.LogRecord{{
		StreamId:  "system:functions",
		Seq:       1,
		EventType: "OldRecord",
		Payload:   []byte(`{"old":true}`),
	}, {
		StreamId:  "system:functions",
		Seq:       2,
		EventType: "FunctionRegistered",
		Payload:   []byte(`{"function_hash":"abc"}`),
	}}}}
	resp := openSSE(t, newTestServer(conn, "secret-token", false), "/api/events?stream=system%3Afunctions&from_seq=2&limit=10&interval_ms=10")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	event, data := readSSEEvent(t, resp)
	if event != "log_records" {
		t.Fatalf("event = %q, data %s", event, data)
	}
	var payload struct {
		StreamID string         `json:"stream_id"`
		Records  []logRecordDTO `json:"records"`
		NextSeq  uint64         `json:"next_seq"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("decode event payload: %v; data %s", err, data)
	}
	if payload.StreamID != "system:functions" || len(payload.Records) != 1 || payload.Records[0].Seq != 2 || payload.NextSeq != 3 {
		t.Fatalf("log event payload = %+v", payload)
	}
}

// TestWorkflowStreamEventsCanAppendLogRecords verifies records=true forces wf:
// streams into raw log-record SSE mode.
func TestWorkflowStreamEventsCanAppendLogRecords(t *testing.T) {
	conn := &fakeClientConn{readLog: &logservepb.ReadLogResponse{Records: []*logservepb.LogRecord{{
		StreamId:  "wf:wf-1",
		Seq:       2,
		EventType: "StepSucceeded",
		Payload:   []byte(`{"step_id":"search"}`),
	}}}}
	resp := openSSE(t, newTestServer(conn, "secret-token", false), "/api/events?stream=wf%3Awf-1&records=1&from_seq=2&limit=10&interval_ms=10")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	event, data := readSSEEvent(t, resp)
	if event != "log_records" {
		t.Fatalf("event = %q, data %s", event, data)
	}
	var payload struct {
		StreamID string         `json:"stream_id"`
		Records  []logRecordDTO `json:"records"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("decode event payload: %v; data %s", err, data)
	}
	if payload.StreamID != "wf:wf-1" || len(payload.Records) != 1 || payload.Records[0].EventType != "StepSucceeded" {
		t.Fatalf("workflow log event payload = %+v", payload)
	}
}

// openSSE starts an httptest server and opens one authenticated SSE response for
// event-stream assertions.
func openSSE(t *testing.T, srv *Server, target string) *http.Response {
	t.Helper()
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpSrv.URL+target, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// readSSEEvent reads one SSE frame and returns its event name and data payload.
func readSSEEvent(t *testing.T, resp *http.Response) (string, string) {
	t.Helper()
	reader := bufio.NewReader(resp.Body)
	var event string
	var data string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE line: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return event, data
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data:") {
			data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
}

// TestAdminConfigExposesBackpressureFields verifies admin config exposes
// scheduler, backpressure, log, and materializer fields.
func TestAdminConfigExposesBackpressureFields(t *testing.T) {
	conn := &fakeClientConn{dashboard: &logservepb.DashboardSnapshot{
		SchedulingPolicy:      logservepb.SchedulingPolicy_SCHEDULING_POLICY_LOCALITY_AWARE,
		QueueHighWatermark:    1024,
		RedeliveryTimeoutMs:   30000,
		LogAppendSlowMs:       100,
		CompactableLogRecords: 11,
		CompactableLogBytes:   4096,
		MetadataMaterializer: &logservepb.MetadataMaterializerStats{
			Mode:                  "async",
			PendingDeltas:         2,
			EventualLagEstimateMs: 7,
		},
	}}
	srv := newTestServer(conn, "secret-token", false)

	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/config", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	for _, want := range []string{
		`"scheduling_policy":"LOCALITY_AWARE"`,
		`"queue_high_watermark":1024`,
		`"redelivery_timeout_ms":30000`,
		`"log_append_slow_ms":100`,
		`"compactable_log_records":11`,
		`"compactable_log_bytes":4096`,
		`"metadata_materializer":{"mode":"async"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("admin config missing %s: %s", want, body)
		}
	}
}

// TestSetBackpressureRequiresToken verifies the admin backpressure endpoint stays
// authenticated.
func TestSetBackpressureRequiresToken(t *testing.T) {
	conn := &fakeClientConn{}
	srv := newTestServer(conn, "secret-token", false)

	resp := httptest.NewRecorder()
	srv.Handler().ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/admin/backpressure", strings.NewReader(`{"queue_high_watermark":1024,"redelivery_timeout_ms":30000,"log_append_slow_ms":100}`)))

	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}
	if len(conn.calls) != 0 {
		t.Fatalf("unauthorized request reached backend: %v", conn.calls)
	}
}

// TestSetBackpressureRejectsInvalidValues covers validation for zero or negative
// threshold inputs.
func TestSetBackpressureRejectsInvalidValues(t *testing.T) {
	cases := []string{
		`{"queue_high_watermark":0,"redelivery_timeout_ms":30000,"log_append_slow_ms":100}`,
		`{"queue_high_watermark":1024,"redelivery_timeout_ms":0,"log_append_slow_ms":100}`,
		`{"queue_high_watermark":1024,"redelivery_timeout_ms":30000,"log_append_slow_ms":0}`,
		`{"queue_high_watermark":1024,"redelivery_timeout_ms":-1,"log_append_slow_ms":100}`,
	}
	for _, body := range cases {
		conn := &fakeClientConn{}
		srv := newTestServer(conn, "secret-token", false)

		resp := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/admin/backpressure", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret-token")
		srv.Handler().ServeHTTP(resp, req)

		if resp.Code != http.StatusBadRequest {
			t.Fatalf("body %s status = %d, response %s", body, resp.Code, resp.Body.String())
		}
		if conn.setBackpressureReq != nil {
			t.Fatalf("invalid body %s reached backend: %+v", body, conn.setBackpressureReq)
		}
	}
}

// TestSetBackpressureUpdatesAdminConfigImmediately verifies the control-plane
// response reflects updated backpressure settings.
func TestSetBackpressureUpdatesAdminConfigImmediately(t *testing.T) {
	conn := &fakeClientConn{dashboard: &logservepb.DashboardSnapshot{
		SchedulingPolicy:    logservepb.SchedulingPolicy_SCHEDULING_POLICY_RESOURCE_ONLY,
		QueueHighWatermark:  128,
		RedeliveryTimeoutMs: 5000,
		LogAppendSlowMs:     50,
	}}
	srv := newTestServer(conn, "secret-token", false)

	postResp := httptest.NewRecorder()
	postReq := httptest.NewRequest(http.MethodPost, "/api/admin/backpressure", strings.NewReader(`{"queue_high_watermark":2048,"redelivery_timeout_ms":45000,"log_append_slow_ms":120}`))
	postReq.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(postResp, postReq)

	if postResp.Code != http.StatusOK {
		t.Fatalf("post status = %d, body %s", postResp.Code, postResp.Body.String())
	}
	if conn.setBackpressureReq.GetQueueHighWatermark() != 2048 || conn.setBackpressureReq.GetRedeliveryTimeoutMs() != 45000 || conn.setBackpressureReq.GetLogAppendSlowMs() != 120 {
		t.Fatalf("set backpressure request = %+v", conn.setBackpressureReq)
	}

	getResp := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/api/admin/config", nil)
	getReq.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(getResp, getReq)

	if getResp.Code != http.StatusOK {
		t.Fatalf("get status = %d, body %s", getResp.Code, getResp.Body.String())
	}
	body := getResp.Body.String()
	for _, want := range []string{`"queue_high_watermark":2048`, `"redelivery_timeout_ms":45000`, `"log_append_slow_ms":120`} {
		if !strings.Contains(body, want) {
			t.Fatalf("admin config after update missing %s: %s", want, body)
		}
	}
}

// TestDashboardReflectsBackpressureAfterPost verifies dashboard snapshots observe
// backpressure updates after the admin POST.
func TestDashboardReflectsBackpressureAfterPost(t *testing.T) {
	conn := &fakeClientConn{dashboard: &logservepb.DashboardSnapshot{
		SchedulingPolicy: logservepb.SchedulingPolicy_SCHEDULING_POLICY_LOCALITY_AWARE,
	}}
	srv := newTestServer(conn, "secret-token", false)

	postResp := httptest.NewRecorder()
	postReq := httptest.NewRequest(http.MethodPost, "/api/admin/backpressure", strings.NewReader(`{"queue_high_watermark":4096,"redelivery_timeout_ms":60000,"log_append_slow_ms":150}`))
	postReq.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(postResp, postReq)
	if postResp.Code != http.StatusOK {
		t.Fatalf("post status = %d, body %s", postResp.Code, postResp.Body.String())
	}

	dashboardResp := httptest.NewRecorder()
	dashboardReq := httptest.NewRequest(http.MethodGet, "/api/dashboard", nil)
	dashboardReq.Header.Set("Authorization", "Bearer secret-token")
	srv.Handler().ServeHTTP(dashboardResp, dashboardReq)
	if dashboardResp.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, body %s", dashboardResp.Code, dashboardResp.Body.String())
	}
	body := dashboardResp.Body.String()
	for _, want := range []string{`"queue_high_watermark":4096`, `"redelivery_timeout_ms":60000`, `"log_append_slow_ms":150`} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard after update missing %s: %s", want, body)
		}
	}
}

// TestStaticFallbackServesIndexForDeepLinks verifies SPA deep links fall back to
// index.html instead of 404.
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

// newRoleTestServer constructs a Server with explicit viewer/operator/admin role
// tokens for RBAC tests.
func newRoleTestServer(conn *fakeClientConn, tokens map[role]string) *Server {
	if conn == nil {
		conn = &fakeClientConn{}
	}
	s := &Server{
		cfg: Config{
			RoleTokens:           tokens,
			AllowUnauthenticated: false,
			RequestTimeout:       time.Second,
		},
		clients: &Clients{
			Control: logservepb.NewControlServiceClient(conn),
			Log:     logservepb.NewLogServiceClient(conn),
		},
		mux:                   http.NewServeMux(),
		functionRegistryCache: newFunctionRegistryCache(),
	}
	s.registerRoutes()
	return s
}

// decodeAuditRecord decodes an audit AppendLog request into the auditEvent test
// shape.
func decodeAuditRecord(t *testing.T, req *logservepb.AppendLogRequest) auditEvent {
	t.Helper()
	if req.GetStreamId() != auditStreamID {
		t.Fatalf("audit stream = %q", req.GetStreamId())
	}
	var event auditEvent
	if err := json.Unmarshal(req.GetPayload(), &event); err != nil {
		t.Fatalf("decode audit: %v", err)
	}
	return event
}

// TestRoleTokenConfigRequiresBackendToken verifies role-token-only config does not
// satisfy backend gRPC authentication requirements.
func TestRoleTokenConfigRequiresBackendToken(t *testing.T) {
	viewerOnly := Config{RoleTokens: map[role]string{roleViewer: "viewer-token"}}
	normalizeAuthConfig(&viewerOnly)
	if hasConfiguredToken(viewerOnly) {
		t.Fatalf("viewer-only role token must not satisfy backend RPC token config")
	}

	adminToken := Config{RoleTokens: map[role]string{roleAdmin: "admin-token"}}
	normalizeAuthConfig(&adminToken)
	if !hasConfiguredToken(adminToken) || adminToken.APIToken != "admin-token" {
		t.Fatalf("admin role token should backfill backend RPC token: %+v", adminToken)
	}
}

// TestRoleTokenConfigRejectsDuplicateRoleTokens verifies ambiguous role token
// configuration is rejected.
func TestRoleTokenConfigRejectsDuplicateRoleTokens(t *testing.T) {
	cfg := Config{APIToken: "admin-token", RoleTokens: map[role]string{roleViewer: "same-token", roleOperator: "same-token"}}
	normalizeAuthConfig(&cfg)
	if err := validateAuthConfig(cfg); err == nil || !strings.Contains(err.Error(), "duplicate token") {
		t.Fatalf("duplicate role tokens error = %v", err)
	}
}

// TestRoleTokensGateOperatorAndAdminOperations verifies viewer/operator/admin
// tokens enforce route permissions and produce audit records.
func TestRoleTokensGateOperatorAndAdminOperations(t *testing.T) {
	tokens := map[role]string{roleViewer: "viewer-token", roleOperator: "operator-token", roleAdmin: "admin-token"}
	viewerConn := &fakeClientConn{}
	viewerSrv := newRoleTestServer(viewerConn, tokens)

	viewerResp := httptest.NewRecorder()
	viewerReq := httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(`{"task_name":"add","function_name":"add","function_source":"def add(): return 3","args":[],"kwargs":{}}`))
	viewerReq.Header.Set("Authorization", "Bearer viewer-token")
	viewerReq.Header.Set("X-Request-ID", "req-viewer-denied")
	viewerSrv.Handler().ServeHTTP(viewerResp, viewerReq)
	if viewerResp.Code != http.StatusForbidden {
		t.Fatalf("viewer submit status = %d, body %s", viewerResp.Code, viewerResp.Body.String())
	}
	if len(viewerConn.submitTaskReqs) != 0 {
		t.Fatalf("viewer reached submit task: %+v", viewerConn.submitTaskReqs)
	}
	if len(viewerConn.appendLogReqs) != 1 || viewerConn.appendLogReqs[0].GetStreamId() != auditStreamID {
		t.Fatalf("viewer audit records = %+v", viewerConn.appendLogReqs)
	}
	denied := decodeAuditRecord(t, viewerConn.appendLogReqs[0])
	if denied.Subject == "" || denied.Role != string(roleViewer) || denied.Action != "submit_task" || denied.Result != "denied" || denied.RequestID != "req-viewer-denied" {
		t.Fatalf("denied audit = %+v", denied)
	}

	operatorConn := &fakeClientConn{submitTask: &logservepb.SubmitTaskResponse{TaskId: "task-template", Status: logservepb.TaskStatus_TASK_STATUS_QUEUED}}
	operatorSrv := newRoleTestServer(operatorConn, tokens)
	operatorResp := httptest.NewRecorder()
	operatorReq := httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(`{"task_name":"add","function_name":"add","function_source":"def add(): return 3","args":[],"kwargs":{}}`))
	operatorReq.Header.Set("Authorization", "Bearer operator-token")
	operatorReq.Header.Set("X-Request-ID", "req-operator-submit")
	operatorSrv.Handler().ServeHTTP(operatorResp, operatorReq)
	if operatorResp.Code != http.StatusOK {
		t.Fatalf("operator submit status = %d, body %s", operatorResp.Code, operatorResp.Body.String())
	}
	if len(operatorConn.submitTaskReqs) != 1 {
		t.Fatalf("operator submit requests = %d", len(operatorConn.submitTaskReqs))
	}
	accepted := decodeAuditRecord(t, operatorConn.appendLogReqs[0])
	if accepted.Role != string(roleOperator) || accepted.Result != "ok" || accepted.RequestID != "req-operator-submit" {
		t.Fatalf("accepted audit = %+v", accepted)
	}

	adminResp := httptest.NewRecorder()
	adminReq := httptest.NewRequest(http.MethodPost, "/api/admin/backpressure", strings.NewReader(`{"queue_high_watermark":1024,"redelivery_timeout_ms":30000,"log_append_slow_ms":100}`))
	adminReq.Header.Set("Authorization", "Bearer operator-token")
	operatorSrv.Handler().ServeHTTP(adminResp, adminReq)
	if adminResp.Code != http.StatusForbidden {
		t.Fatalf("operator backpressure status = %d, body %s", adminResp.Code, adminResp.Body.String())
	}
}

// TestSessionReportsRoleAndPermissions verifies the session endpoint exposes the
// authenticated role and frontend permission flags.
func TestSessionReportsRoleAndPermissions(t *testing.T) {
	conn := &fakeClientConn{}
	srv := newRoleTestServer(conn, map[role]string{roleViewer: "viewer-token"})
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	req.Header.Set("Authorization", "Bearer viewer-token")
	srv.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	if !strings.Contains(body, `"role":"viewer"`) || !strings.Contains(body, `"read:templates"`) {
		t.Fatalf("session payload = %s", body)
	}
	if len(conn.appendLogReqs) != 1 {
		t.Fatalf("session audit records = %+v", conn.appendLogReqs)
	}
	audit := decodeAuditRecord(t, conn.appendLogReqs[0])
	if audit.Action != "read_session" || audit.Result != "ok" || audit.Role != string(roleViewer) {
		t.Fatalf("session audit = %+v", audit)
	}
}

// TestOperatorCannotRunAdminOnlyTemplate verifies template-level RBAC can be
// stricter than the route-level operator gate.
func TestOperatorCannotRunAdminOnlyTemplate(t *testing.T) {
	conn := &fakeClientConn{}
	srv := newRoleTestServer(conn, map[role]string{roleOperator: "operator-token", roleAdmin: "admin-token"})
	resp := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/templates/mock_llm_request/run", strings.NewReader(`{"idempotency_key":"mock-key"}`))
	req.Header.Set("Authorization", "Bearer operator-token")
	req.Header.Set("X-Request-ID", "req-operator-mock-template")
	srv.Handler().ServeHTTP(resp, req)
	if resp.Code != http.StatusForbidden {
		t.Fatalf("operator mock template status = %d, body %s", resp.Code, resp.Body.String())
	}
	if len(conn.calls) != 0 {
		t.Fatalf("operator reached backend for admin-only template: %v", conn.calls)
	}
	if len(conn.appendLogReqs) != 1 {
		t.Fatalf("audit records = %+v", conn.appendLogReqs)
	}
	audit := decodeAuditRecord(t, conn.appendLogReqs[0])
	if audit.Action != "run_template" || audit.Result != "denied" || audit.Role != string(roleOperator) || audit.RequestID != "req-operator-mock-template" {
		t.Fatalf("admin-only template audit = %+v", audit)
	}
}

// TestTemplatesListAndRunAddTask verifies template listing and successful task
// template execution.
func TestTemplatesListAndRunAddTask(t *testing.T) {
	conn := &fakeClientConn{submitTask: &logservepb.SubmitTaskResponse{TaskId: "task-add", Status: logservepb.TaskStatus_TASK_STATUS_QUEUED}}
	srv := newRoleTestServer(conn, map[role]string{roleOperator: "operator-token"})

	listResp := httptest.NewRecorder()
	listReq := httptest.NewRequest(http.MethodGet, "/api/templates", nil)
	listReq.Header.Set("Authorization", "Bearer operator-token")
	srv.Handler().ServeHTTP(listResp, listReq)
	if listResp.Code != http.StatusOK {
		t.Fatalf("list status = %d, body %s", listResp.Code, listResp.Body.String())
	}
	if body := listResp.Body.String(); !strings.Contains(body, `"id":"add_task"`) || !strings.Contains(body, `"id":"mock_llm_request"`) || !strings.Contains(body, `"expected_result"`) {
		t.Fatalf("template list missing built-ins: %s", body)
	}
	if len(conn.appendLogReqs) != 1 {
		t.Fatalf("list audit records = %+v", conn.appendLogReqs)
	}
	listAudit := decodeAuditRecord(t, conn.appendLogReqs[0])
	if listAudit.Action != "list_templates" || listAudit.Result != "ok" {
		t.Fatalf("template list audit = %+v", listAudit)
	}

	runResp := httptest.NewRecorder()
	runReq := httptest.NewRequest(http.MethodPost, "/api/templates/add_task/run", strings.NewReader(`{"idempotency_key":"template-key"}`))
	runReq.Header.Set("Authorization", "Bearer operator-token")
	srv.Handler().ServeHTTP(runResp, runReq)
	if runResp.Code != http.StatusOK {
		t.Fatalf("run status = %d, body %s", runResp.Code, runResp.Body.String())
	}
	if len(conn.submitTaskReqs) != 1 {
		t.Fatalf("submit task requests = %d", len(conn.submitTaskReqs))
	}
	submitted := conn.submitTaskReqs[0]
	if submitted.GetTaskName() != "add" || submitted.GetFunctionName() != "add" || string(submitted.GetArgsJson()) != `{"args":[1,2],"kwargs":{}}` || submitted.GetIdempotencyKey() != "template-key" {
		t.Fatalf("template submit = %+v", submitted)
	}
	if len(conn.appendLogReqs) != 2 {
		t.Fatalf("audit records = %+v", conn.appendLogReqs)
	}
	runAudit := decodeAuditRecord(t, conn.appendLogReqs[1])
	if runAudit.Action != "run_template" || runAudit.Result != "ok" {
		t.Fatalf("template run audit = %+v", runAudit)
	}
}
