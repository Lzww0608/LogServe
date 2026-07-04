// Tests in package actor cover replay compatibility and payload encoding
// behavior that the control plane depends on for actor recovery.
package actor

import (
	"encoding/json"
	"testing"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/eventcodec"
)

// mapResultLoader is a test snapshot loader backed by in-memory byte slices.
type mapResultLoader map[string][]byte

// LoadResult returns a defensive copy so tests exercise replay ownership of
// loaded snapshot bytes.
func (l mapResultLoader) LoadResult(ref string) ([]byte, error) {
	return append([]byte(nil), l[ref]...), nil
}

// TestReplayRestoresOwnerAndEpochFromSnapshotAndTailCommands verifies snapshot
// hydration followed by tail-command replay preserves owner, epoch, and state.
func TestReplayRestoresOwnerAndEpochFromSnapshotAndTailCommands(t *testing.T) {
	records := []*logservepb.LogRecord{
		actorRecord(t, "ActorSnapshotCreated", EventPayload{
			ActorID:              "actor-1",
			ClassName:            "Counter",
			ClassSource:          "class Counter: pass",
			InitArgsJSON:         json.RawMessage(`{"args":[],"kwargs":{}}`),
			WorkerID:             "worker-a",
			Epoch:                3,
			SnapshotRef:          "snap-20",
			SnapshotEvery:        5,
			SnapshotCommandCount: 20,
			TimestampMs:          100,
		}),
		actorRecord(t, "ActorCommandSubmitted", EventPayload{
			ActorID:     "actor-1",
			CommandSeq:  21,
			TimestampMs: 101,
		}),
		actorRecord(t, "ActorCommandApplied", EventPayload{
			ActorID:      "actor-1",
			CommandSeq:   21,
			CommandCount: 21,
			StateJSON:    json.RawMessage(`{"value":21}`),
			WorkerID:     "worker-a",
			Epoch:        3,
			TimestampMs:  102,
		}),
	}

	replayed, err := Replay("actor-1", records, mapResultLoader{"snap-20": []byte(`{"value":20}`)})
	if err != nil {
		t.Fatal(err)
	}
	want := State{
		ActorID:               "actor-1",
		ClassName:             "Counter",
		Status:                logservepb.ActorStatus_ACTOR_STATUS_ACTIVE,
		OwnerWorkerID:         "worker-a",
		Epoch:                 3,
		CommandCount:          21,
		SubmittedCommandCount: 21,
		SnapshotRef:           "snap-20",
		SnapshotCommandCount:  20,
		StateJSON:             []byte(`{"value":21}`),
	}
	if !Consistent(replayed.State, want) {
		t.Fatalf("replayed state = %+v, want owner/epoch and command state %+v", replayed.State, want)
	}
}

// actorRecord builds a protobuf log record with the legacy JSON payload form
// used by compatibility tests.
func actorRecord(t *testing.T, eventType string, payload EventPayload) *logservepb.LogRecord {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return &logservepb.LogRecord{
		StreamId:    "actor:actor-1",
		EventType:   eventType,
		Payload:     data,
		TimestampMs: payload.TimestampMs,
	}
}

// TestActorEventPayloadBinaryAndJSONFallback verifies new binary payloads and
// legacy JSON payloads both decode into the same actor event fields.
func TestActorEventPayloadBinaryAndJSONFallback(t *testing.T) {
	payload := EventPayload{
		ActorID:              "actor-1",
		ClassName:            "Counter",
		ClassSource:          "class Counter: pass",
		WorkerID:             "worker-1",
		Epoch:                2,
		CommandSeq:           3,
		StateJSON:            json.RawMessage(`{"value":3}`),
		SnapshotRef:          "snap-3",
		SnapshotCommandCount: 3,
		TimestampMs:          123,
	}
	data, err := MarshalEventPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	var decoded EventPayload
	if err := UnmarshalEventPayload(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ActorID != payload.ActorID || decoded.WorkerID != payload.WorkerID || decoded.CommandSeq != payload.CommandSeq || string(decoded.StateJSON) != string(payload.StateJSON) {
		t.Fatalf("decoded payload = %+v, want %+v", decoded, payload)
	}

	legacy, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	decoded = EventPayload{}
	if err := UnmarshalEventPayload(legacy, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ActorID != payload.ActorID || decoded.WorkerID != payload.WorkerID || decoded.CommandSeq != payload.CommandSeq {
		t.Fatalf("legacy decoded payload = %+v, want %+v", decoded, payload)
	}
}

// TestActorEventPayloadRecordsByteSizes verifies eventcodec payload maps retain
// byte-size metadata for large JSON fields.
func TestActorEventPayloadRecordsByteSizes(t *testing.T) {
	fields := eventPayloadMap(EventPayload{
		ArgsJSON:   json.RawMessage(`{"args":[1]}`),
		ResultJSON: json.RawMessage(`{"ok":true}`),
		StateJSON:  json.RawMessage(`{"value":3}`),
	})
	if got := eventcodec.Int64Value(fields["args_json_bytes"]); got != int64(len(`{"args":[1]}`)) {
		t.Fatalf("args_json_bytes = %d, want %d", got, len(`{"args":[1]}`))
	}
	if got := eventcodec.Int64Value(fields["result_json_bytes"]); got != int64(len(`{"ok":true}`)) {
		t.Fatalf("result_json_bytes = %d, want %d", got, len(`{"ok":true}`))
	}
	if got := eventcodec.Int64Value(fields["state_json_bytes"]); got != int64(len(`{"value":3}`)) {
		t.Fatalf("state_json_bytes = %d, want %d", got, len(`{"value":3}`))
	}
}
