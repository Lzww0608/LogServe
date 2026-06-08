package control

import (
	"context"
	"strings"
	"testing"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/actor"
	"github.com/logserve/logserve/internal/metadata"
)

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
