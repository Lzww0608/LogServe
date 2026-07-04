package control

import (
	"encoding/json"
	"testing"
)

// TestTaskLifecyclePayloadBinaryAndJSONFallback verifies compact lifecycle payloads
// decode correctly and legacy JSON remains replayable.
func TestTaskLifecyclePayloadBinaryAndJSONFallback(t *testing.T) {
	want := taskLifecyclePayload{
		TaskLeaseEpoch: 3,
		WorkerID:       "worker-1",
		ResultJSON:     []byte(`{"value":1}`),
		Error:          "",
		TimestampMs:    42,
	}
	encoded, err := marshalTaskLifecyclePayload(want)
	if err != nil {
		t.Fatal(err)
	}
	got := decodeTaskLifecyclePayload(encoded)
	if got.TaskLeaseEpoch != want.TaskLeaseEpoch || got.WorkerID != want.WorkerID {
		t.Fatalf("decoded = %+v, want %+v", got, want)
	}
	if string(got.ResultJSON) != string(want.ResultJSON) {
		t.Fatalf("result = %s, want %s", got.ResultJSON, want.ResultJSON)
	}

	legacy, err := json.Marshal(map[string]any{
		"task_lease_epoch": want.TaskLeaseEpoch,
		"worker_id":        want.WorkerID,
		"result_json":      json.RawMessage(want.ResultJSON),
		"timestamp_ms":     want.TimestampMs,
	})
	if err != nil {
		t.Fatal(err)
	}
	legacyGot := decodeTaskLifecyclePayload(legacy)
	if legacyGot.TaskLeaseEpoch != want.TaskLeaseEpoch || legacyGot.WorkerID != want.WorkerID {
		t.Fatalf("legacy decoded = %+v", legacyGot)
	}
}
