package integration

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
)

func TestActorCounterRecoverySnapshotAndReplay(t *testing.T) {
	env := startWorkflowEnv(t)
	defer env.stop()

	firstCtx, firstCancel := context.WithCancel(context.Background())
	defer firstCancel()
	done := make(chan struct{})
	go func() {
		runWorkerForTest(firstCtx, t, env, "actor-worker-1", 100)
		close(done)
	}()

	created := createCounterActor(t, env.controlClient, 20)
	for i := 1; i <= 100; i++ {
		resp := callActor(t, env.controlClient, created.GetActorId(), "inc", nil)
		if string(resp.GetResultJson()) != strconv.Itoa(i) {
			t.Fatalf("inc result = %s, want %d", resp.GetResultJson(), i)
		}
	}
	<-done

	status, err := env.controlClient.GetActorStatus(context.Background(), &logservepb.GetActorStatusRequest{ActorId: created.GetActorId()})
	if err != nil {
		t.Fatal(err)
	}
	if status.GetCommandCount() != 100 {
		t.Fatalf("command_count = %d, want 100", status.GetCommandCount())
	}
	if status.GetSnapshotRef() == "" || status.GetSnapshotCommandCount() != 100 {
		t.Fatalf("snapshot not created: ref=%q count=%d", status.GetSnapshotRef(), status.GetSnapshotCommandCount())
	}

	secondCtx, secondCancel := context.WithCancel(context.Background())
	defer secondCancel()
	go runWorkerForTest(secondCtx, t, env, "actor-worker-2", 0)
	time.Sleep(900 * time.Millisecond)

	getResp := callActor(t, env.controlClient, created.GetActorId(), "get", nil)
	if string(getResp.GetResultJson()) != "100" {
		t.Fatalf("get after recovery = %s, want 100", getResp.GetResultJson())
	}
	if getResp.GetEpoch() < 2 {
		t.Fatalf("epoch after recovery = %d, want >= 2", getResp.GetEpoch())
	}

	replayed, err := env.controlClient.ReplayActor(context.Background(), &logservepb.ReplayActorRequest{ActorId: created.GetActorId()})
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.GetConsistentWithMetadata() {
		t.Fatal("actor replay is not consistent with metadata")
	}
	if replayed.GetSnapshotReplayCommands() >= replayed.GetFullReplayCommands() {
		t.Fatalf("snapshot replay did not reduce work: snapshot=%d full=%d", replayed.GetSnapshotReplayCommands(), replayed.GetFullReplayCommands())
	}

	records, err := env.logClient.ReadLog(context.Background(), &logservepb.ReadLogRequest{
		StreamId: "actor:" + created.GetActorId(),
		FromSeq:  1,
		Limit:    1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := countWorkflowEvent(records.GetRecords(), "ActorCreated"); got != 1 {
		t.Fatalf("ActorCreated events = %d, want 1", got)
	}
	if got := countWorkflowEvent(records.GetRecords(), "ActorCommandApplied"); got != 101 {
		t.Fatalf("ActorCommandApplied events = %d, want 101", got)
	}
	if got := countWorkflowEvent(records.GetRecords(), "ActorCommandSubmitted"); got != 101 {
		t.Fatalf("ActorCommandSubmitted events = %d, want 101", got)
	}
	assertActorCommandSubmittedBeforeApplied(t, records.GetRecords())
	if got := countWorkflowEvent(records.GetRecords(), "ActorSnapshotCreated"); got == 0 {
		t.Fatal("ActorSnapshotCreated event missing")
	}
}

func TestActorConcurrentMailboxSerializes1000Increments(t *testing.T) {
	env := startWorkflowEnv(t)
	defer env.stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runWorkerForTest(ctx, t, env, "actor-concurrency-worker", 0)

	created := createCounterActor(t, env.controlClient, 200)
	var wg sync.WaitGroup
	errs := make(chan error, 1000)
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := env.controlClient.CallActor(context.Background(), &logservepb.CallActorRequest{
				ActorId:    created.GetActorId(),
				MethodName: "inc",
				ArgsJson:   actorArgs(t),
				TimeoutMs:  180_000,
			})
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	resp := callActor(t, env.controlClient, created.GetActorId(), "get", nil)
	if string(resp.GetResultJson()) != "1000" {
		t.Fatalf("final actor value = %s, want 1000", resp.GetResultJson())
	}
}

func createCounterActor(t *testing.T, client logservepb.ControlServiceClient, snapshotEvery uint32) *logservepb.CreateActorResponse {
	t.Helper()
	resp, err := client.CreateActor(context.Background(), &logservepb.CreateActorRequest{
		ClassName:      "Counter",
		ClassSource:    counterSource(),
		InitArgsJson:   actorArgs(t),
		SnapshotEvery:  snapshotEvery,
		IdempotencyKey: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func callActor(t *testing.T, client logservepb.ControlServiceClient, actorID, method string, args []any) *logservepb.CallActorResponse {
	t.Helper()
	resp, err := client.CallActor(context.Background(), &logservepb.CallActorRequest{
		ActorId:    actorID,
		MethodName: method,
		ArgsJson:   actorArgs(t, args...),
		TimeoutMs:  60_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetStatus() != logservepb.TaskStatus_TASK_STATUS_SUCCEEDED {
		t.Fatalf("actor call failed: %s", resp.GetError())
	}
	return resp
}

func actorArgs(t *testing.T, args ...any) []byte {
	t.Helper()
	if args == nil {
		args = []any{}
	}
	data, err := json.Marshal(map[string]any{"args": args, "kwargs": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func counterSource() string {
	return `
from logserve import actor

@actor
class Counter:
    def __init__(self):
        self.value = 0

    def inc(self):
        self.value += 1
        return self.value

    def get(self):
        return self.value
`
}

func assertActorCommandSubmittedBeforeApplied(t *testing.T, records []*logservepb.LogRecord) {
	t.Helper()
	submittedAt := map[uint64]int{}
	for i, rec := range records {
		if rec.GetEventType() != "ActorCommandSubmitted" && rec.GetEventType() != "ActorCommandApplied" {
			continue
		}
		var payload struct {
			CommandSeq uint64 `json:"command_seq"`
			CallID     string `json:"call_id"`
		}
		if err := json.Unmarshal(rec.GetPayload(), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.CommandSeq == 0 {
			t.Fatalf("%s missing command_seq in payload %s", rec.GetEventType(), string(rec.GetPayload()))
		}
		switch rec.GetEventType() {
		case "ActorCommandSubmitted":
			if _, exists := submittedAt[payload.CommandSeq]; exists {
				t.Fatalf("duplicate ActorCommandSubmitted for command_seq=%d", payload.CommandSeq)
			}
			submittedAt[payload.CommandSeq] = i
		case "ActorCommandApplied":
			submittedIndex, exists := submittedAt[payload.CommandSeq]
			if !exists {
				t.Fatalf("ActorCommandApplied command_seq=%d has no submitted event", payload.CommandSeq)
			}
			if submittedIndex > i {
				t.Fatalf("ActorCommandApplied command_seq=%d appears before submitted", payload.CommandSeq)
			}
		}
	}
}
