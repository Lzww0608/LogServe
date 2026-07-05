package integration

// This file exercises actor execution through the integration stack, including
// snapshot recovery, log compaction, mailbox serialization, and replay checks.

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	actorpkg "github.com/logserve/logserve/internal/actor"
)

// TestActorCounterRecoverySnapshotAndReplay drives a counter actor through enough
// commands to create a snapshot, restarts execution on another worker, and checks
// replay plus compacted log ordering.
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
	waitForWorkerRegistered(t, env.controlClient, "actor-worker-1")

	// snapshotEvery is lower than the command count so the test can assert both
	// snapshot creation and reduced replay work.
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
	waitForWorkerRegistered(t, env.controlClient, "actor-worker-2")
	// Allow the replacement worker to observe and recover the idle actor before the
	// next call checks epoch advancement.
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
	// After trimming, only the compacted tail and snapshot event should remain in
	// the actor stream; creation is represented by the snapshot state.
	if got := countWorkflowEvent(records.GetRecords(), "ActorCreated"); got != 0 {
		t.Fatalf("ActorCreated events after trim = %d, want 0", got)
	}
	if got := countWorkflowEvent(records.GetRecords(), "ActorCommandApplied"); got != 1 {
		t.Fatalf("tail ActorCommandApplied events after trim = %d, want 1", got)
	}
	if got := countWorkflowEvent(records.GetRecords(), "ActorCommandSubmitted"); got != 1 {
		t.Fatalf("tail ActorCommandSubmitted events after trim = %d, want 1", got)
	}
	assertActorCommandSubmittedBeforeApplied(t, records.GetRecords())
	if got := countWorkflowEvent(records.GetRecords(), "ActorSnapshotCreated"); got == 0 {
		t.Fatal("ActorSnapshotCreated event missing")
	}
	stats, err := env.logClient.GetStreamStats(context.Background(), &logservepb.GetStreamStatsRequest{
		StreamId: "actor:" + created.GetActorId(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.GetStreams()) != 1 {
		t.Fatalf("stream stats = %d, want 1", len(stats.GetStreams()))
	}
	if stats.GetStreams()[0].GetCompactableRecords() == 0 || stats.GetStreams()[0].GetCompactableBytes() == 0 {
		t.Fatalf("actor stream compactable stats missing: %+v", stats.GetStreams()[0])
	}
}

// TestActorConcurrentMailboxSerializes1000Increments submits many concurrent
// calls to one actor and verifies the mailbox serializes all increments exactly
// once.
func TestActorConcurrentMailboxSerializes1000Increments(t *testing.T) {
	env := startWorkflowEnv(t)
	defer env.stop()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runWorkerForTest(ctx, t, env, "actor-concurrency-worker", 0)
	waitForWorkerRegistered(t, env.controlClient, "actor-concurrency-worker")

	created := createCounterActor(t, env.controlClient, 200)
	var wg sync.WaitGroup
	errs := make(chan error, 1000)
	// The goroutines intentionally share one actor ID; correctness depends on the
	// actor mailbox, not client-side ordering.
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

// createCounterActor registers the embedded Python Counter class and returns the
// actor id used by recovery and concurrency tests.
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

// callActor invokes one actor method and fails the test if the control plane or
// actor runtime reports an unsuccessful task status.
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

// actorArgs encodes the args/kwargs envelope expected by the Python actor runtime.
// A nil variadic slice is normalized to an empty args array for stable JSON shape.
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

// counterSource returns the minimal Python actor class used by the actor runtime
// tests.
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

// assertActorCommandSubmittedBeforeApplied verifies each applied actor command has
// a prior submitted event with the same command sequence.
func assertActorCommandSubmittedBeforeApplied(t *testing.T, records []*logservepb.LogRecord) {
	t.Helper()
	// Store the first submitted index per command sequence so ordering can be
	// checked even after log compaction leaves only a tail of command events.
	submittedAt := map[uint64]int{}
	for i, rec := range records {
		if rec.GetEventType() != "ActorCommandSubmitted" && rec.GetEventType() != "ActorCommandApplied" {
			continue
		}
		var payload actorpkg.EventPayload
		if err := actorpkg.UnmarshalEventPayload(rec.GetPayload(), &payload); err != nil {
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
