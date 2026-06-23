package control

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	actorpkg "github.com/logserve/logserve/internal/actor"
	"github.com/logserve/logserve/internal/metadata"
	workflowpkg "github.com/logserve/logserve/internal/workflow"
)

type checkpointAcceptanceWorkload struct {
	Tasks      int `json:"tasks"`
	Workflows  int `json:"workflows"`
	Actors     int `json:"actors"`
	LLMStreams int `json:"llm_streams"`
	TailEvents int `json:"tail_events"`
}

type checkpointAcceptanceCheckpointSummary struct {
	ID            string `json:"id"`
	StreamCount   int    `json:"stream_count"`
	TaskCount     int    `json:"task_count"`
	WorkflowCount int    `json:"workflow_count"`
	ActorCount    int    `json:"actor_count"`
	LLMStatsCount int    `json:"llm_stats_count"`
}

type checkpointAcceptanceReplayMetrics struct {
	DurationMS   float64 `json:"duration_ms"`
	ReadLogCalls int     `json:"read_log_calls"`
	RecordsRead  int     `json:"records_read"`
	SeqOneReads  int     `json:"seq_1_reads_for_checkpointed_streams,omitempty"`
}

type checkpointAcceptanceReport struct {
	Verdict          string                                `json:"verdict"`
	GeneratedAtUTC   string                                `json:"generated_at_utc"`
	Workload         checkpointAcceptanceWorkload          `json:"workload"`
	Checkpoint       checkpointAcceptanceCheckpointSummary `json:"checkpoint"`
	FullReplay       checkpointAcceptanceReplayMetrics     `json:"full_replay"`
	CheckpointReplay checkpointAcceptanceReplayMetrics     `json:"checkpoint_replay"`
	Ratios           map[string]float64                    `json:"ratios"`
	Consistency      MetadataCheckpointConsistency         `json:"consistency"`
	Checks           map[string]bool                       `json:"checks"`
	Failures         []string                              `json:"failures,omitempty"`
}

type checkpointAcceptancePendingTail struct {
	TaskIDs         []string
	WorkflowTaskIDs []string
	ActorIDs        []string
	LLMTaskIDs      []string
}

func TestMetadataCheckpointAcceptanceReport(t *testing.T) {
	outPath := os.Getenv("LOGSERVE_CHECKPOINT_ACCEPTANCE_OUT")
	if outPath == "" {
		t.Skip("set LOGSERVE_CHECKPOINT_ACCEPTANCE_OUT to emit the Ubuntu checkpoint acceptance report")
	}

	ctx := context.Background()
	workload := checkpointAcceptanceWorkloadFromEnv()
	logClient := newCountingReplayableLogClient()
	source := NewServiceWithResultStore(metadata.NewMemoryStore(), logClient, nil, 0)
	source.schedulerV2 = false

	pending, err := buildCheckpointAcceptanceHistory(t, ctx, source, logClient, workload)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := source.CreateMetadataCheckpoint(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	tailEvents, err := appendCheckpointAcceptanceTail(t, ctx, source, logClient, pending)
	if err != nil {
		t.Fatal(err)
	}
	workload.TailEvents = tailEvents

	full, fullMetrics, err := runFullMetadataBootstrapForAcceptance(ctx, logClient)
	if err != nil {
		t.Fatal(err)
	}
	fast, fastMetrics, err := runCheckpointMetadataBootstrapForAcceptance(ctx, logClient, cp.Streams)
	if err != nil {
		t.Fatal(err)
	}
	consistent, failures := checkpointAcceptanceServicesConsistent(full, fast)
	checkpointConsistency, err := source.CheckMetadataCheckpointConsistency(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !checkpointConsistency.Consistent {
		failures = append(failures, checkpointConsistency.FailureKeys...)
	}

	corruptFallbackOK := checkpointAcceptanceCorruptFallback(ctx)
	retentionOK := checkpointAcceptanceRetention(ctx)
	checks := map[string]bool{
		"checkpoint_created":              cp.ID != "" && len(cp.Streams) > 0,
		"checkpoint_replay_consistent":    consistent && checkpointConsistency.Consistent,
		"checkpoint_read_records_reduced": fastMetrics.RecordsRead < fullMetrics.RecordsRead,
		"checkpoint_tail_only_reads":      fastMetrics.SeqOneReads == 0,
		"corrupt_checkpoint_fallback":     corruptFallbackOK,
		"checkpoint_retention":            retentionOK,
	}
	for name, ok := range checks {
		if !ok {
			failures = append(failures, name)
		}
	}
	sort.Strings(failures)
	failures = compactStrings(failures)
	verdict := "PASS"
	if len(failures) > 0 {
		verdict = "FAIL"
	}

	report := checkpointAcceptanceReport{
		Verdict:        verdict,
		GeneratedAtUTC: time.Now().UTC().Format(time.RFC3339),
		Workload:       workload,
		Checkpoint: checkpointAcceptanceCheckpointSummary{
			ID:            cp.ID,
			StreamCount:   len(cp.Streams),
			TaskCount:     len(cp.Tasks),
			WorkflowCount: len(cp.Workflows),
			ActorCount:    len(cp.Actors),
			LLMStatsCount: len(cp.LLMStats),
		},
		FullReplay:       fullMetrics,
		CheckpointReplay: fastMetrics,
		Ratios: map[string]float64{
			"checkpoint_records_over_full":    ratioFloat(float64(fastMetrics.RecordsRead), float64(fullMetrics.RecordsRead)),
			"checkpoint_read_calls_over_full": ratioFloat(float64(fastMetrics.ReadLogCalls), float64(fullMetrics.ReadLogCalls)),
			"checkpoint_duration_over_full":   ratioFloat(fastMetrics.DurationMS, fullMetrics.DurationMS),
		},
		Consistency: checkpointConsistency,
		Checks:      checks,
		Failures:    failures,
	}
	if err := writeCheckpointAcceptanceReport(outPath, report); err != nil {
		t.Fatal(err)
	}
	if verdict != "PASS" {
		t.Fatalf("checkpoint acceptance failed: %v", failures)
	}
}

func checkpointAcceptanceWorkloadFromEnv() checkpointAcceptanceWorkload {
	return checkpointAcceptanceWorkload{
		Tasks:      envIntMin("LOGSERVE_CHECKPOINT_ACCEPTANCE_TASKS", 120, 4),
		Workflows:  envIntMin("LOGSERVE_CHECKPOINT_ACCEPTANCE_WORKFLOWS", 12, 1),
		Actors:     envIntMin("LOGSERVE_CHECKPOINT_ACCEPTANCE_ACTORS", 12, 1),
		LLMStreams: envIntMin("LOGSERVE_CHECKPOINT_ACCEPTANCE_LLM_STREAMS", 40, 1),
	}
}

func envIntMin(name string, fallback, minValue int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil || value < minValue {
		return fallback
	}
	return value
}

func buildCheckpointAcceptanceHistory(t *testing.T, ctx context.Context, service *Service, logClient *countingReplayableLogClient, workload checkpointAcceptanceWorkload) (checkpointAcceptancePendingTail, error) {
	t.Helper()
	if _, err := service.RegisterWorker(ctx, &logservepb.RegisterWorkerRequest{WorkerId: "worker-1", Capacity: 8}); err != nil {
		return checkpointAcceptancePendingTail{}, err
	}
	if _, err := service.RegisterWorker(ctx, &logservepb.RegisterWorkerRequest{WorkerId: "worker-2", Capacity: 8}); err != nil {
		return checkpointAcceptancePendingTail{}, err
	}

	var pending checkpointAcceptancePendingTail
	taskTailCount := tailCount(workload.Tasks)
	for i := 0; i < workload.Tasks; i++ {
		resp, err := service.SubmitTask(ctx, &logservepb.SubmitTaskRequest{
			TaskName:       "checkpoint_acceptance_task",
			FunctionName:   "run",
			FunctionSource: "def run():\n    return 'ok'\n",
			ArgsJson:       []byte(`{"args":[],"kwargs":{}}`),
			IdempotencyKey: fmt.Sprintf("checkpoint-acceptance-task-%04d", i),
		})
		if err != nil {
			return checkpointAcceptancePendingTail{}, err
		}
		if i >= workload.Tasks-taskTailCount {
			pending.TaskIDs = append(pending.TaskIDs, resp.GetTaskId())
			continue
		}
		if err := completeAcceptanceTask(ctx, service, resp.GetTaskId(), workerFor(i), []byte(fmt.Sprintf(`"task-%d"`, i))); err != nil {
			return checkpointAcceptancePendingTail{}, err
		}
	}

	workflowTailCount := tailCount(workload.Workflows)
	for i := 0; i < workload.Workflows; i++ {
		resp, err := service.SubmitWorkflow(ctx, &logservepb.SubmitWorkflowRequest{
			WorkflowName:   "checkpoint_acceptance_workflow",
			DefinitionJson: minimalWorkflowDefinition(t),
			IdempotencyKey: fmt.Sprintf("checkpoint-acceptance-workflow-%04d", i),
		})
		if err != nil {
			return checkpointAcceptancePendingTail{}, err
		}
		state, ok := service.meta.GetWorkflow(resp.GetWorkflowId())
		if !ok {
			return checkpointAcceptancePendingTail{}, fmt.Errorf("workflow %s missing", resp.GetWorkflowId())
		}
		taskID := state.Steps["finish"].TaskID
		if i >= workload.Workflows-workflowTailCount {
			pending.WorkflowTaskIDs = append(pending.WorkflowTaskIDs, taskID)
			continue
		}
		if err := completeAcceptanceTask(ctx, service, taskID, workerFor(i), []byte(fmt.Sprintf(`"workflow-%d"`, i))); err != nil {
			return checkpointAcceptancePendingTail{}, err
		}
	}

	actorTailCount := tailCount(workload.Actors)
	for i := 0; i < workload.Actors; i++ {
		resp, err := service.CreateActor(ctx, &logservepb.CreateActorRequest{
			ClassName:      "Counter",
			ClassSource:    "class Counter:\n    pass\n",
			InitArgsJson:   []byte(`{"args":[],"kwargs":{}}`),
			IdempotencyKey: fmt.Sprintf("checkpoint-acceptance-actor-%04d", i),
		})
		if err != nil {
			return checkpointAcceptancePendingTail{}, err
		}
		if i >= workload.Actors-actorTailCount {
			pending.ActorIDs = append(pending.ActorIDs, resp.GetActorId())
			continue
		}
		applyAcceptanceActorCommand(t, service, logClient, resp.GetActorId(), 1)
	}

	llmTailCount := tailCount(workload.LLMStreams)
	for i := 0; i < workload.LLMStreams; i++ {
		taskID := fmt.Sprintf("checkpoint-acceptance-llm-%04d", i)
		appendLLMCompleted(t, logClient, taskID, llmEventPayload{
			TaskID:         taskID,
			ModelName:      "model-A",
			ModelVersion:   "v1",
			WorkerID:       workerFor(i),
			CacheHit:       i%2 == 0,
			ModelLoadMs:    int64(5 + i%7),
			TotalLatencyMs: int64(50 + i%11),
			TimestampMs:    int64(1000 + i),
		})
		if i >= workload.LLMStreams-llmTailCount {
			pending.LLMTaskIDs = append(pending.LLMTaskIDs, taskID)
		}
	}
	if err := service.bootstrapLLMStats(ctx); err != nil {
		return checkpointAcceptancePendingTail{}, err
	}
	return pending, nil
}

func appendCheckpointAcceptanceTail(t *testing.T, ctx context.Context, service *Service, logClient *countingReplayableLogClient, pending checkpointAcceptancePendingTail) (int, error) {
	t.Helper()
	tailEvents := 0
	for i, taskID := range pending.TaskIDs {
		if err := completeAcceptanceTask(ctx, service, taskID, workerFor(i), []byte(fmt.Sprintf(`"tail-task-%d"`, i))); err != nil {
			return tailEvents, err
		}
		tailEvents += 2
	}
	for i, taskID := range pending.WorkflowTaskIDs {
		if err := completeAcceptanceTask(ctx, service, taskID, workerFor(i), []byte(fmt.Sprintf(`"tail-workflow-%d"`, i))); err != nil {
			return tailEvents, err
		}
		tailEvents += 5
	}
	for _, actorID := range pending.ActorIDs {
		applyAcceptanceActorCommand(t, service, logClient, actorID, 1)
		tailEvents++
	}
	for i, taskID := range pending.LLMTaskIDs {
		appendLLMCompleted(t, logClient, taskID, llmEventPayload{
			TaskID:         taskID,
			ModelName:      "model-A",
			ModelVersion:   "v1",
			WorkerID:       workerFor(i),
			CacheHit:       i%2 == 0,
			ModelLoadMs:    int64(10 + i),
			TotalLatencyMs: int64(70 + i),
			TimestampMs:    int64(9000 + i),
		})
		tailEvents++
	}
	return tailEvents, nil
}

func completeAcceptanceTask(ctx context.Context, service *Service, taskID, workerID string, result []byte) error {
	leased, err := service.meta.LeaseTask(taskID, workerID)
	if err != nil {
		return err
	}
	if _, err := service.StartTask(ctx, &logservepb.StartTaskRequest{
		TaskId:         taskID,
		WorkerId:       workerID,
		TaskLeaseEpoch: leased.TaskLeaseEpoch,
	}); err != nil {
		return err
	}
	_, err = service.CompleteTask(ctx, &logservepb.CompleteTaskRequest{
		TaskId:         taskID,
		WorkerId:       workerID,
		Status:         logservepb.TaskStatus_TASK_STATUS_SUCCEEDED,
		ResultJson:     result,
		TaskLeaseEpoch: leased.TaskLeaseEpoch,
		ActorEpoch:     leased.ActorEpoch,
		ActorStateJson: result,
	})
	return err
}

func applyAcceptanceActorCommand(t *testing.T, service *Service, logClient *countingReplayableLogClient, actorID string, commandSeq uint64) {
	t.Helper()
	state, ok := service.meta.GetActor(actorID)
	if !ok {
		t.Fatalf("actor %s missing", actorID)
	}
	if state.OwnerWorkerID == "" {
		state.OwnerWorkerID = "worker-1"
	}
	if state.Epoch == 0 {
		state.Epoch = 1
	}
	stateJSON := []byte(fmt.Sprintf(`{"n":%d}`, commandSeq))
	appendActorEvent(t, logClient, actorID, "ActorCommandApplied", actorpkg.EventPayload{
		ActorID:      actorID,
		CallID:       fmt.Sprintf("%s-call-%d", actorID, commandSeq),
		CommandSeq:   commandSeq,
		CommandCount: commandSeq,
		WorkerID:     state.OwnerWorkerID,
		Epoch:        state.Epoch,
		StateJSON:    stateJSON,
		TimestampMs:  int64(7000 + commandSeq),
	})
	if _, err := service.meta.UpdateActor(actorID, func(current *actorpkg.State) error {
		current.OwnerWorkerID = state.OwnerWorkerID
		current.Epoch = state.Epoch
		current.CommandCount = commandSeq
		current.SubmittedCommandCount = commandSeq
		current.StateJSON = actorpkg.NormalizeJSON(stateJSON)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func runFullMetadataBootstrapForAcceptance(ctx context.Context, logClient *countingReplayableLogClient) (*Service, checkpointAcceptanceReplayMetrics, error) {
	logClient.resetReadCounts()
	metaStore := metadata.NewMemoryStore()
	service := NewServiceWithResultStore(metaStore, logClient, nil, 0)
	service.schedulerV2 = false
	start := time.Now()
	for _, step := range []func(context.Context) error{
		service.bootstrapModels,
		service.bootstrapWorkers,
		service.bootstrapScheduler,
		service.bootstrapBackpressure,
		service.bootstrapTasks,
		func(ctx context.Context) error { return service.bootstrapWorkflowsWithScheduling(ctx, false) },
		service.bootstrapActors,
		service.bootstrapLLMStats,
	} {
		if err := step(ctx); err != nil {
			return nil, checkpointAcceptanceReplayMetrics{}, err
		}
	}
	return service, checkpointAcceptanceReplayMetrics{
		DurationMS:   millisSince(start),
		ReadLogCalls: logClient.totalReadCalls(),
		RecordsRead:  logClient.totalRecordsRead(),
	}, nil
}

func runCheckpointMetadataBootstrapForAcceptance(ctx context.Context, logClient *countingReplayableLogClient, streams map[string]MetadataCheckpointStream) (*Service, checkpointAcceptanceReplayMetrics, error) {
	logClient.resetReadCounts()
	metaStore := metadata.NewMemoryStore()
	service := NewServiceWithResultStore(metaStore, logClient, nil, 0)
	service.schedulerV2 = false
	start := time.Now()
	if err := service.BootstrapFromLog(ctx); err != nil {
		return nil, checkpointAcceptanceReplayMetrics{}, err
	}
	return service, checkpointAcceptanceReplayMetrics{
		DurationMS:   millisSince(start),
		ReadLogCalls: logClient.totalReadCalls(),
		RecordsRead:  logClient.totalRecordsRead(),
		SeqOneReads:  logClient.readCountForStreamsFromSeq(streams, 1),
	}, nil
}

func checkpointAcceptanceServicesConsistent(full, fast *Service) (bool, []string) {
	var failures []string
	if len(full.meta.ListTasks()) != len(fast.meta.ListTasks()) {
		failures = append(failures, "task_count")
	}
	for _, task := range full.meta.ListTasks() {
		other, ok := fast.meta.GetTask(task.TaskID)
		if !ok || !metadataTasksConsistent(task, other) {
			failures = append(failures, "task:"+task.TaskID)
		}
	}
	if len(full.meta.ListWorkflows()) != len(fast.meta.ListWorkflows()) {
		failures = append(failures, "workflow_count")
	}
	for _, state := range full.meta.ListWorkflows() {
		other, ok := fast.meta.GetWorkflow(state.WorkflowID)
		if !ok || !workflowpkg.Consistent(state, other) {
			failures = append(failures, "wf:"+state.WorkflowID)
		}
	}
	if len(full.meta.ListActors()) != len(fast.meta.ListActors()) {
		failures = append(failures, "actor_count")
	}
	for _, state := range full.meta.ListActors() {
		other, ok := fast.meta.GetActor(state.ActorID)
		if !ok || !actorpkg.Consistent(state, other) {
			failures = append(failures, "actor:"+state.ActorID)
		}
	}
	sort.Strings(failures)
	failures = compactStrings(failures)
	return len(failures) == 0, failures
}

func checkpointAcceptanceCorruptFallback(ctx context.Context) bool {
	logClient := newCountingReplayableLogClient()
	service := NewServiceWithResultStore(metadata.NewMemoryStore(), logClient, nil, 0)
	resp, err := service.SubmitTask(ctx, &logservepb.SubmitTaskRequest{
		TaskName:       "corrupt_checkpoint_acceptance",
		FunctionName:   "run",
		FunctionSource: "def run():\n    return 'ok'\n",
		ArgsJson:       []byte(`{"args":[],"kwargs":{}}`),
		IdempotencyKey: "corrupt-checkpoint-acceptance",
	})
	if err != nil {
		return false
	}
	if _, err := logClient.AppendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       metadataCheckpointStream,
		EventType:      metadataCheckpointEvent,
		IdempotencyKey: "corrupt-checkpoint-acceptance",
		Payload:        []byte(`{"checkpoint_id":`),
	}); err != nil {
		return false
	}
	restarted := NewServiceWithResultStore(metadata.NewMemoryStore(), logClient, nil, 0)
	if err := restarted.BootstrapFromLog(ctx); err != nil {
		return false
	}
	_, ok := restarted.meta.GetTask(resp.GetTaskId())
	return ok
}

func checkpointAcceptanceRetention(ctx context.Context) bool {
	logClient := newCountingReplayableLogClient()
	service := NewServiceWithResultStore(metadata.NewMemoryStore(), logClient, nil, 0)
	for i := 0; i < 4; i++ {
		if _, err := service.CreateMetadataCheckpoint(ctx, 2); err != nil {
			return false
		}
	}
	resp, err := logClient.ReadLog(ctx, &logservepb.ReadLogRequest{StreamId: metadataCheckpointStream, FromSeq: 1, Limit: 10})
	if err != nil {
		return false
	}
	return len(resp.GetRecords()) == 2 && resp.GetRecords()[0].GetSeq() == 3 && resp.GetRecords()[1].GetSeq() == 4
}

func writeCheckpointAcceptanceReport(path string, report checkpointAcceptanceReport) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func tailCount(total int) int {
	if total <= 0 {
		return 0
	}
	count := total / 5
	if count < 1 {
		return 1
	}
	return count
}

func workerFor(i int) string {
	if i%2 == 0 {
		return "worker-1"
	}
	return "worker-2"
}

func millisSince(start time.Time) float64 {
	ms := float64(time.Since(start).Microseconds()) / 1000
	return math.Round(ms*1000) / 1000
}

func ratioFloat(num, den float64) float64 {
	if den == 0 {
		return 0
	}
	return math.Round((num/den)*10000) / 10000
}

func compactStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := values[:0]
	var last string
	for _, value := range values {
		if value == "" || value == last {
			continue
		}
		out = append(out, value)
		last = value
	}
	return out
}
