package control

import (
	"encoding/json"

	"github.com/logserve/logserve/internal/eventcodec"
)

const taskLifecycleEventKind = eventcodec.Kind(4)

func marshalTaskLifecyclePayload(payload taskLifecyclePayload) ([]byte, error) {
	fields := map[string]any{}
	if payload.TaskLeaseEpoch > 0 {
		fields["task_lease_epoch"] = payload.TaskLeaseEpoch
	}
	if payload.WorkerID != "" {
		fields["worker_id"] = payload.WorkerID
	}
	if len(payload.ResultJSON) > 0 {
		fields["result_json"] = []byte(payload.ResultJSON)
	}
	if payload.Error != "" {
		fields["error"] = payload.Error
	}
	if payload.TimestampMs != 0 {
		fields["timestamp_ms"] = payload.TimestampMs
	}
	if len(fields) == 0 {
		return json.Marshal(payload)
	}
	return eventcodec.Marshal(taskLifecycleEventKind, fields)
}

func decodeTaskLifecyclePayload(data []byte) taskLifecyclePayload {
	if len(data) == 0 {
		return taskLifecyclePayload{}
	}
	var fields map[string]any
	encoded, err := eventcodec.Unmarshal(taskLifecycleEventKind, data, &fields)
	if err != nil {
		return taskLifecyclePayload{}
	}
	if encoded {
		return taskLifecyclePayload{
			TaskLeaseEpoch: eventcodec.Uint64Value(fields["task_lease_epoch"]),
			WorkerID:       eventcodec.StringValue(fields["worker_id"]),
			ResultJSON:     eventcodec.BytesValue(fields["result_json"]),
			Error:          eventcodec.StringValue(fields["error"]),
			TimestampMs:    eventcodec.Int64Value(fields["timestamp_ms"]),
		}
	}
	var payload taskLifecyclePayload
	_ = json.Unmarshal(data, &payload)
	return payload
}
