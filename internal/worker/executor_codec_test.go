package worker

// This file protects the msgpack executor wire format used between the Go
// worker and the Python subprocess.

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/vmihailenco/msgpack/v5"
)

// TestExecutorMsgpackRequestKeepsJSONPayloadAsBytes verifies the msgpack request path preserves raw JSON args bytes.
func TestExecutorMsgpackRequestKeepsJSONPayloadAsBytes(t *testing.T) {
	args := json.RawMessage(`{"args":[1,2],"kwargs":{}}`)
	data, err := marshalExecutorRequestMsgpack(executorRequest{
		FunctionName: "add",
		FunctionHash: "sha256:abc",
		ArgsJSON:     args,
	})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := msgpack.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	got, ok := fields["args_json"].([]byte)
	if !ok {
		t.Fatalf("args_json type = %T, want []byte", fields["args_json"])
	}
	if !bytes.Equal(got, args) {
		t.Fatalf("args_json = %q, want %q", got, args)
	}
}

// TestExecutorMsgpackResponseUsesRawJSONBytes verifies result_json and state_json stay raw JSON after response decoding.
func TestExecutorMsgpackResponseUsesRawJSONBytes(t *testing.T) {
	data, err := msgpack.Marshal(map[string]any{
		"ok":          true,
		"result_json": []byte(`{"value":3}`),
		"state_json":  []byte(`{"count":2}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := unmarshalExecutorResponseMsgpack(data)
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK || string(resp.Result) != `{"value":3}` || string(resp.State) != `{"count":2}` {
		t.Fatalf("response = %+v", resp)
	}
}
