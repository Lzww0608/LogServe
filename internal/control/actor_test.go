package control

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/actor"
	"github.com/logserve/logserve/internal/metadata"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type recordingLogClient struct {
	mu      sync.Mutex
	records []*logservepb.AppendLogRequest
}

func (c *recordingLogClient) AppendLog(_ context.Context, req *logservepb.AppendLogRequest, _ ...grpc.CallOption) (*logservepb.AppendLogResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	clone := proto.Clone(req).(*logservepb.AppendLogRequest)
	c.records = append(c.records, clone)
	return &logservepb.AppendLogResponse{Seq: uint64(len(c.records)), TimestampMs: actor.NowMs()}, nil
}

func (c *recordingLogClient) ReadLog(context.Context, *logservepb.ReadLogRequest, ...grpc.CallOption) (*logservepb.ReadLogResponse, error) {
	return &logservepb.ReadLogResponse{}, nil
}

func (c *recordingLogClient) ListStreams(context.Context, *logservepb.ListStreamsRequest, ...grpc.CallOption) (*logservepb.ListStreamsResponse, error) {
	return &logservepb.ListStreamsResponse{}, nil
}

func (c *recordingLogClient) TrimStream(context.Context, *logservepb.TrimStreamRequest, ...grpc.CallOption) (*logservepb.TrimStreamResponse, error) {
	return &logservepb.TrimStreamResponse{}, nil
}

func (c *recordingLogClient) GetStreamStats(context.Context, *logservepb.GetStreamStatsRequest, ...grpc.CallOption) (*logservepb.GetStreamStatsResponse, error) {
	return &logservepb.GetStreamStatsResponse{}, nil
}

func (c *recordingLogClient) countActorEvents(actorID, eventType string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, rec := range c.records {
		if rec.GetStreamId() == actorStream(actorID) && rec.GetEventType() == eventType {
			count++
		}
	}
	return count
}

func TestActorEpochFencingRejectsStaleCompletion(t *testing.T) {
	meta := metadata.NewMemoryStore()
	state := actor.NewState("actor-fence", "Counter", "class Counter: pass", []byte(`{"args":[],"kwargs":{}}`), 10, actor.NowMs())
	state.OwnerWorkerID = "new-worker"
	state.Epoch = 2
	if _, duplicate := meta.CreateActor(state, ""); duplicate {
		t.Fatal("unexpected duplicate actor")
	}
	service := &Service{meta: meta}

	err := service.completeActorCall(context.Background(), metadata.Task{
		TaskID:      "actor-call-stale",
		ActorID:     "actor-fence",
		ActorCallID: "actor-call-stale",
		ActorEpoch:  1,
	}, &logservepb.CompleteTaskRequest{
		TaskId:         "actor-call-stale",
		WorkerId:       "old-worker",
		Status:         logservepb.TaskStatus_TASK_STATUS_SUCCEEDED,
		ResultJson:     []byte(`1`),
		ActorStateJson: []byte(`{"value":1}`),
		ActorEpoch:     1,
	})
	if err == nil {
		t.Fatal("expected stale completion to be rejected")
	}
	if !strings.Contains(err.Error(), "stale actor completion rejected") {
		t.Fatalf("unexpected error: %v", err)
	}
	current, ok := meta.GetActor("actor-fence")
	if !ok {
		t.Fatal("actor missing")
	}
	if current.CommandCount != 0 || len(current.StateJSON) != 0 {
		t.Fatalf("stale completion mutated actor: command_count=%d state=%s", current.CommandCount, current.StateJSON)
	}
}

func TestActorCommandSeqRejectsOutOfOrderCompletion(t *testing.T) {
	meta := metadata.NewMemoryStore()
	state := actor.NewState("actor-seq", "Counter", "class Counter: pass", []byte(`{"args":[],"kwargs":{}}`), 10, actor.NowMs())
	state.OwnerWorkerID = "worker-1"
	state.Epoch = 1
	state.CommandCount = 1
	if _, duplicate := meta.CreateActor(state, ""); duplicate {
		t.Fatal("unexpected duplicate actor")
	}
	service := &Service{meta: meta}

	err := service.completeActorCall(context.Background(), metadata.Task{
		TaskID:          "actor-call-out-of-order",
		ActorID:         "actor-seq",
		ActorCallID:     "actor-call-out-of-order",
		ActorEpoch:      1,
		ActorCommandSeq: 3,
	}, &logservepb.CompleteTaskRequest{
		TaskId:         "actor-call-out-of-order",
		WorkerId:       "worker-1",
		Status:         logservepb.TaskStatus_TASK_STATUS_SUCCEEDED,
		ResultJson:     []byte(`3`),
		ActorStateJson: []byte(`{"value":3}`),
		ActorEpoch:     1,
	})
	if err == nil {
		t.Fatal("expected out-of-order actor command completion to be rejected")
	}
	if !strings.Contains(err.Error(), "out-of-order actor command rejected") {
		t.Fatalf("unexpected error: %v", err)
	}
	current, ok := meta.GetActor("actor-seq")
	if !ok {
		t.Fatal("actor missing")
	}
	if current.CommandCount != 1 || len(current.StateJSON) != 0 {
		t.Fatalf("out-of-order completion mutated actor: command_count=%d state=%s", current.CommandCount, current.StateJSON)
	}
}

func TestActorCommandSeqAdvancesAfterTimedOutSubmittedCommand(t *testing.T) {
	meta := metadata.NewMemoryStore()
	meta.UpsertWorker(metadata.Worker{
		WorkerID:      "worker-1",
		Labels:        map[string]string{},
		CachedModels:  map[string]bool{},
		Capacity:      1,
		LastHeartbeat: actor.NowMs(),
	})
	state := actor.NewState("actor-timeout-seq", "Counter", "class Counter: pass", []byte(`{"args":[],"kwargs":{}}`), 10, actor.NowMs())
	state.OwnerWorkerID = "worker-1"
	state.Epoch = 1
	if _, duplicate := meta.CreateActor(state, ""); duplicate {
		t.Fatal("unexpected duplicate actor")
	}
	logClient := &recordingLogClient{}
	service := NewServiceWithResultStore(meta, logClient, nil, 0)

	for i := 0; i < 2; i++ {
		_, err := service.CallActor(context.Background(), &logservepb.CallActorRequest{
			ActorId:    state.ActorID,
			MethodName: "inc",
			ArgsJson:   []byte(`{"args":[],"kwargs":{}}`),
			TimeoutMs:  30,
		})
		if err == nil || !strings.Contains(err.Error(), "actor call timed out") {
			t.Fatalf("call %d error = %v, want actor call timed out", i+1, err)
		}
	}

	var seqs []uint64
	for _, rec := range logClient.records {
		if rec.GetStreamId() != actorStream(state.ActorID) || rec.GetEventType() != "ActorCommandSubmitted" {
			continue
		}
		var payload actor.EventPayload
		if err := actor.UnmarshalEventPayload(rec.GetPayload(), &payload); err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, payload.CommandSeq)
	}
	if len(seqs) != 2 || seqs[0] != 1 || seqs[1] != 2 {
		t.Fatalf("submitted command_seq values = %v, want [1 2]", seqs)
	}
}

func TestActorCallDoesNotBlockMailboxWhileWaitingForResult(t *testing.T) {
	meta := metadata.NewMemoryStore()
	meta.UpsertWorker(metadata.Worker{
		WorkerID:      "worker-1",
		Labels:        map[string]string{},
		CachedModels:  map[string]bool{},
		Capacity:      1,
		LastHeartbeat: actor.NowMs(),
	})
	state := actor.NewState("actor-async-mailbox", "Counter", "class Counter: pass", []byte(`{"args":[],"kwargs":{}}`), 10, actor.NowMs())
	state.OwnerWorkerID = "worker-1"
	state.Epoch = 1
	if _, duplicate := meta.CreateActor(state, ""); duplicate {
		t.Fatal("unexpected duplicate actor")
	}
	logClient := &recordingLogClient{}
	service := NewServiceWithResultStore(meta, logClient, nil, 0)

	firstDone := make(chan error, 1)
	go func() {
		_, err := service.CallActor(context.Background(), &logservepb.CallActorRequest{
			ActorId:    state.ActorID,
			MethodName: "inc",
			ArgsJson:   []byte(`{"args":[],"kwargs":{}}`),
			TimeoutMs:  500,
		})
		firstDone <- err
	}()
	waitForActorEventCountOrDone(t, logClient, state.ActorID, "ActorCommandSubmitted", 1, firstDone)

	start := time.Now()
	_, err := service.CallActor(context.Background(), &logservepb.CallActorRequest{
		ActorId:    state.ActorID,
		MethodName: "inc",
		ArgsJson:   []byte(`{"args":[],"kwargs":{}}`),
		TimeoutMs:  40,
	})
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "actor call timed out") {
		t.Fatalf("second call error = %v, want actor call timed out", err)
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("second call waited %s; mailbox submission is blocked by an earlier in-flight call", elapsed)
	}
	if got := logClient.countActorEvents(state.ActorID, "ActorCommandSubmitted"); got != 2 {
		t.Fatalf("ActorCommandSubmitted events = %d, want 2", got)
	}
	<-firstDone
}

func TestPollTaskSkipsFutureActorCommandUntilMailboxReady(t *testing.T) {
	meta := metadata.NewMemoryStore()
	meta.UpsertWorker(metadata.Worker{WorkerID: "worker-1", Labels: map[string]string{}, CachedModels: map[string]bool{}, Capacity: 1, LastHeartbeat: actor.NowMs()})
	state := actor.NewState("actor-skip-future", "Counter", "class Counter: pass", []byte(`{"args":[],"kwargs":{}}`), 10, actor.NowMs())
	state.OwnerWorkerID = "worker-1"
	state.Epoch = 1
	state.CommandCount = 0
	if _, duplicate := meta.CreateActor(state, ""); duplicate {
		t.Fatal("unexpected duplicate actor")
	}
	service := NewServiceWithResultStore(meta, acceptingLogClient{}, nil, 0)
	_, _, err := service.enqueueTaskWithMetadata(context.Background(), &logservepb.TaskSpec{
		TaskId:            "actor-call-future",
		TaskName:          "actor:inc",
		FunctionName:      "inc",
		ArgsJson:          []byte(`{"args":[],"kwargs":{}}`),
		IdempotencyKey:    "actor-future-idem",
		TargetWorkerId:    "worker-1",
		ActorId:           state.ActorID,
		ActorCallId:       "actor-call-future",
		ActorClassName:    state.ClassName,
		ActorClassSource:  state.ClassSource,
		ActorMethod:       "inc",
		ActorInitArgsJson: state.InitArgsJSON,
		ActorEpoch:        state.Epoch,
	}, func(task *metadata.Task) {
		task.ActorCommandSeq = 2
	})
	if err != nil {
		t.Fatal(err)
	}

	poll, err := service.PollTask(context.Background(), &logservepb.PollTaskRequest{WorkerId: "worker-1"})
	if err != nil {
		t.Fatal(err)
	}
	if poll.GetHasTask() {
		t.Fatalf("future actor command was dispatched before command_seq=1 completed: %s", poll.GetTask().GetTaskId())
	}
}

func TestPollTaskInjectsLatestActorStateForReadyCommand(t *testing.T) {
	meta := metadata.NewMemoryStore()
	meta.UpsertWorker(metadata.Worker{WorkerID: "worker-1", Labels: map[string]string{}, CachedModels: map[string]bool{}, Capacity: 1, LastHeartbeat: actor.NowMs()})
	state := actor.NewState("actor-latest-state", "Counter", "class Counter: pass", []byte(`{"args":[],"kwargs":{}}`), 10, actor.NowMs())
	state.OwnerWorkerID = "worker-1"
	state.Epoch = 1
	state.StateJSON = []byte(`{"value":41}`)
	if _, duplicate := meta.CreateActor(state, ""); duplicate {
		t.Fatal("unexpected duplicate actor")
	}
	service := NewServiceWithResultStore(meta, acceptingLogClient{}, nil, 0)
	_, _, err := service.enqueueTaskWithMetadata(context.Background(), &logservepb.TaskSpec{
		TaskId:            "actor-call-ready",
		TaskName:          "actor:inc",
		FunctionName:      "inc",
		ArgsJson:          []byte(`{"args":[],"kwargs":{}}`),
		IdempotencyKey:    "actor-ready-idem",
		TargetWorkerId:    "worker-1",
		ActorId:           state.ActorID,
		ActorCallId:       "actor-call-ready",
		ActorClassName:    state.ClassName,
		ActorClassSource:  state.ClassSource,
		ActorMethod:       "inc",
		ActorStateJson:    []byte(`{"value":0}`),
		ActorInitArgsJson: state.InitArgsJSON,
		ActorEpoch:        state.Epoch,
	}, func(task *metadata.Task) {
		task.ActorCommandSeq = 1
	})
	if err != nil {
		t.Fatal(err)
	}

	poll, err := service.PollTask(context.Background(), &logservepb.PollTaskRequest{WorkerId: "worker-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !poll.GetHasTask() {
		t.Fatal("ready actor command was not dispatched")
	}
	if string(poll.GetTask().GetActorStateJson()) != `{"value":41}` {
		t.Fatalf("actor state json = %s, want latest state", poll.GetTask().GetActorStateJson())
	}
}

func TestPollTaskDispatchesQueuedActorCommandToRecoveredOwner(t *testing.T) {
	meta := metadata.NewMemoryStore()
	meta.UpsertWorker(metadata.Worker{WorkerID: "new-worker", Labels: map[string]string{}, CachedModels: map[string]bool{}, Capacity: 1, LastHeartbeat: actor.NowMs()})
	state := actor.NewState("actor-recovered-mailbox", "Counter", "class Counter: pass", []byte(`{"args":[],"kwargs":{}}`), 10, actor.NowMs())
	state.OwnerWorkerID = "new-worker"
	state.Epoch = 2
	state.StateJSON = []byte(`{"value":7}`)
	if _, duplicate := meta.CreateActor(state, ""); duplicate {
		t.Fatal("unexpected duplicate actor")
	}
	service := NewServiceWithResultStore(meta, acceptingLogClient{}, nil, 0)
	_, _, err := service.enqueueTaskWithMetadata(context.Background(), &logservepb.TaskSpec{
		TaskId:            "actor-call-recovered",
		TaskName:          "actor:inc",
		FunctionName:      "inc",
		ArgsJson:          []byte(`{"args":[],"kwargs":{}}`),
		IdempotencyKey:    "actor-recovered-idem",
		TargetWorkerId:    "old-worker",
		ActorId:           state.ActorID,
		ActorCallId:       "actor-call-recovered",
		ActorClassName:    state.ClassName,
		ActorClassSource:  state.ClassSource,
		ActorMethod:       "inc",
		ActorStateJson:    []byte(`{"value":0}`),
		ActorInitArgsJson: state.InitArgsJSON,
		ActorEpoch:        1,
	}, func(task *metadata.Task) {
		task.ActorCommandSeq = 1
		task.ActorEpoch = 1
		task.TargetWorkerID = "old-worker"
	})
	if err != nil {
		t.Fatal(err)
	}

	poll, err := service.PollTask(context.Background(), &logservepb.PollTaskRequest{WorkerId: "new-worker"})
	if err != nil {
		t.Fatal(err)
	}
	if !poll.GetHasTask() {
		t.Fatal("recovered actor owner did not receive queued command")
	}
	if poll.GetTask().GetTargetWorkerId() != "new-worker" || poll.GetTask().GetActorEpoch() != 2 {
		t.Fatalf("leased task owner/epoch = %s/%d, want new-worker/2", poll.GetTask().GetTargetWorkerId(), poll.GetTask().GetActorEpoch())
	}
	if string(poll.GetTask().GetActorStateJson()) != `{"value":7}` {
		t.Fatalf("actor state json = %s, want recovered state", poll.GetTask().GetActorStateJson())
	}
}

func waitForActorEventCount(t *testing.T, client *recordingLogClient, actorID, eventType string, want int) {
	t.Helper()
	waitForActorEventCountOrDone(t, client, actorID, eventType, want, nil)
}

func waitForActorEventCountOrDone(t *testing.T, client *recordingLogClient, actorID, eventType string, want int, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if client.countActorEvents(actorID, eventType) >= want {
			return
		}
		if done != nil {
			select {
			case err := <-done:
				t.Fatalf("actor call finished before %s was logged: %v", eventType, err)
			case <-time.After(10 * time.Millisecond):
			}
			continue
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d %s events on %s; got %d", want, eventType, actorID, client.countActorEvents(actorID, eventType))
}
