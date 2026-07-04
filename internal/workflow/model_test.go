package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/logrecord"
)

// TestParseDefinitionRejectsInvalidDAG verifies validation rejects missing result steps, unknown dependencies, cycles, and duplicate IDs.
func TestParseDefinitionRejectsInvalidDAG(t *testing.T) {
	tests := []struct {
		name       string
		definition string
		want       string
	}{
		{
			name: "invalid result step",
			definition: `{
				"result_step_id":"missing",
				"steps":[{"step_id":"a","task_name":"a","function_name":"a","function_source":"def a():\n    return 1\n"}]
			}`,
			want: "result_step_id",
		},
		{
			name: "unknown dependency",
			definition: `{
				"steps":[{"step_id":"a","depends_on":["missing"],"task_name":"a","function_name":"a","function_source":"def a():\n    return 1\n"}]
			}`,
			want: "unknown step",
		},
		{
			name: "cycle",
			definition: `{
				"steps":[
					{"step_id":"a","depends_on":["b"],"task_name":"a","function_name":"a","function_source":"def a():\n    return 1\n"},
					{"step_id":"b","depends_on":["a"],"task_name":"b","function_name":"b","function_source":"def b():\n    return 1\n"}
				]
			}`,
			want: "cycle",
		},
		{
			name: "duplicate step id",
			definition: `{
				"steps":[
					{"step_id":"a","task_name":"a","function_name":"a","function_source":"def a():\n    return 1\n"},
					{"step_id":"a","task_name":"b","function_name":"b","function_source":"def b():\n    return 1\n"}
				]
			}`,
			want: "duplicate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseDefinition([]byte(tt.definition))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ParseDefinition error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

// TestParseDefinitionDefaultsResultStepToLastStep verifies legacy definitions without result_step_id use the last step.
func TestParseDefinitionDefaultsResultStepToLastStep(t *testing.T) {
	def, err := ParseDefinition([]byte(`{
		"steps":[
			{"step_id":"first","task_name":"first","function_name":"first","function_source":"def first():\n    return 1\n"},
			{"step_id":"last","depends_on":["first"],"task_name":"last","function_name":"last","function_source":"def last():\n    return 2\n"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if def.ResultStepID != "last" {
		t.Fatalf("ResultStepID = %q, want last", def.ResultStepID)
	}
}

// TestWorkflowEventPayloadBinaryAndJSONFallback verifies binary workflow events and legacy JSON payloads decode to the same fields.
func TestWorkflowEventPayloadBinaryAndJSONFallback(t *testing.T) {
	payload := EventPayload{
		WorkflowID:     "wf-1",
		StepID:         "step-1",
		TaskID:         "task-1",
		Attempt:        2,
		InputHash:      "hash",
		ResultJSON:     json.RawMessage(`{"ok":true}`),
		TimestampMs:    123,
		LatencyMs:      7,
		IdempotencyKey: "idem",
	}
	data, err := MarshalEventPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	var decoded EventPayload
	if err := UnmarshalEventPayload(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.WorkflowID != payload.WorkflowID || decoded.StepID != payload.StepID || decoded.InputHash != payload.InputHash || string(decoded.ResultJSON) != string(payload.ResultJSON) || decoded.Attempt != payload.Attempt {
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
	if decoded.WorkflowID != payload.WorkflowID || decoded.StepID != payload.StepID || decoded.InputHash != payload.InputHash {
		t.Fatalf("legacy decoded payload = %+v, want %+v", decoded, payload)
	}
}

// TestReplayRawEachConsumesRawWorkflowRecords verifies raw replay applies workflow start and scheduling records without protobuf conversion.
func TestReplayRawEachConsumesRawWorkflowRecords(t *testing.T) {
	definition := json.RawMessage(`{"workflow_name":"wf","steps":[{"step_id":"step-1","task_name":"task","function_name":"fn","function_source":"def fn():\n    return 1\n"}],"result_step_id":"step-1"}`)
	started, err := MarshalEventPayload(EventPayload{DefinitionJSON: definition, TimestampMs: 10})
	if err != nil {
		t.Fatal(err)
	}
	scheduled, err := MarshalEventPayload(EventPayload{StepID: "step-1", TaskID: "task-1", Attempt: 1, InputHash: "hash", TimestampMs: 11})
	if err != nil {
		t.Fatal(err)
	}
	state, err := ReplayRawEach("wf-1", func(emit func(logrecord.RawRecord) error) error {
		if err := emit(logrecord.RawRecord{EventType: "WorkflowStarted", Payload: started, TimestampMs: 10}); err != nil {
			return err
		}
		return emit(logrecord.RawRecord{EventType: "StepScheduled", Payload: scheduled, TimestampMs: 11})
	})
	if err != nil {
		t.Fatal(err)
	}
	step, ok := state.Step("step-1")
	if !ok {
		t.Fatal("step missing")
	}
	if step.TaskID != "task-1" || step.LastInputHash != "hash" {
		t.Fatalf("step after raw replay = %+v", step)
	}
}

// TestReplayRawEachRestoresResolvedArgsCache verifies replayed StepScheduled events preserve the cached resolved args payload.
func TestReplayRawEachRestoresResolvedArgsCache(t *testing.T) {
	definition := json.RawMessage(`{"workflow_name":"wf","steps":[{"step_id":"step-1","task_name":"task","function_name":"fn","function_source":"def fn():\n    return 1\n"}],"result_step_id":"step-1"}`)
	started, err := MarshalEventPayload(EventPayload{DefinitionJSON: definition, TimestampMs: 10})
	if err != nil {
		t.Fatal(err)
	}
	resolved := []byte(`{"args":[1],"kwargs":{}}`)
	scheduled, err := MarshalEventPayload(EventPayload{
		StepID:           "step-1",
		TaskID:           "task-1",
		Attempt:          1,
		InputHash:        "hash",
		ResolvedArgsJSON: resolved,
		TimestampMs:      11,
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err := ReplayRawEach("wf-1", func(emit func(logrecord.RawRecord) error) error {
		if err := emit(logrecord.RawRecord{EventType: "WorkflowStarted", Payload: started, TimestampMs: 10}); err != nil {
			return err
		}
		return emit(logrecord.RawRecord{EventType: "StepScheduled", Payload: scheduled, TimestampMs: 11})
	})
	if err != nil {
		t.Fatal(err)
	}
	step, ok := state.Step("step-1")
	if !ok {
		t.Fatal("step missing")
	}
	if !bytes.Equal(step.ResolvedArgsJSON, resolved) {
		t.Fatalf("resolved args = %q, want %q", step.ResolvedArgsJSON, resolved)
	}
	args, hash, err := ResolveCachedArgs(
		StepDefinition{StepID: "step-1"},
		step,
		state,
		failingResultLoader{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if hash != "hash" || string(args) != string(resolved) {
		t.Fatalf("cached resolve = %q hash=%q", args, hash)
	}
}

// errFailingLoader marks tests where cached args must avoid dereferencing result refs.
var errFailingLoader = errors.New("result loader should not be called")

// failingResultLoader fails any unexpected attempt to load a referenced result during cached resolution tests.
type failingResultLoader struct{}

// LoadResult implements ResultLoader and always fails so cache-only paths are observable.
func (failingResultLoader) LoadResult(string) ([]byte, error) {
	return nil, errFailingLoader
}

// TestResolveCachedArgsUsesStepCache verifies cached resolved args bypass recursive reference resolution.
func TestResolveCachedArgsUsesStepCache(t *testing.T) {
	args, hash, err := ResolveCachedArgs(
		StepDefinition{StepID: "step-1", ArgsJSON: json.RawMessage(`{"args":[{"__step_ref__":"dep"}],"kwargs":{}}`)},
		StepState{LastInputHash: "cached-hash", ResolvedArgsJSON: []byte(`{"args":[1],"kwargs":{}}`)},
		State{},
		failingResultLoader{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if hash != "cached-hash" || string(args) != `{"args":[1],"kwargs":{}}` {
		t.Fatalf("cached args = %q hash=%q", args, hash)
	}
}

// TestRuntimeDAGReadyQueueUsesTopologicalOrder verifies topological ordering and downstream readiness after dependency success.
func TestRuntimeDAGReadyQueueUsesTopologicalOrder(t *testing.T) {
	def, err := ParseDefinition([]byte(`{
		"result_step_id":"b",
		"steps":[
			{"step_id":"b","depends_on":["a"],"task_name":"b","function_name":"b","function_source":"def b():\n    return 2\n"},
			{"step_id":"a","task_name":"a","function_name":"a","function_source":"def a():\n    return 1\n"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{def.Steps[0].StepID, def.Steps[1].StepID}; got[0] != "a" || got[1] != "b" {
		t.Fatalf("topological step order = %v, want [a b]", got)
	}

	state := NewState("wf-ready", def, 1)
	stepDef, _, ok := state.PopReadyStep()
	if !ok || stepDef.StepID != "a" {
		t.Fatalf("first ready step = %q/%v, want a/true", stepDef.StepID, ok)
	}
	if nextDef, _, ok := state.PopReadyStep(); ok {
		t.Fatalf("unexpected ready step before dependency success: %s", nextDef.StepID)
	}
	state.SetStepSucceeded("a", "task-a", []byte(`1`), "", 2, 1)
	stepDef, _, ok = state.PopReadyStep()
	if !ok || stepDef.StepID != "b" {
		t.Fatalf("ready step after dependency success = %q/%v, want b/true", stepDef.StepID, ok)
	}
}

// TestStateJSONAcceptsLegacyStepMapAndRebuildsRuntime verifies old map-shaped state JSON still restores a schedulable runtime DAG.
func TestStateJSONAcceptsLegacyStepMapAndRebuildsRuntime(t *testing.T) {
	def, err := ParseDefinition([]byte(`{
		"result_step_id":"b",
		"steps":[
			{"step_id":"a","task_name":"a","function_name":"a","function_source":"def a():\n    return 1\n"},
			{"step_id":"b","depends_on":["a"],"task_name":"b","function_name":"b","function_source":"def b():\n    return 2\n"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	legacy := map[string]any{
		"WorkflowID":   "wf-legacy",
		"WorkflowName": "legacy",
		"Status":       logservepb.WorkflowStatus_WORKFLOW_STATUS_RUNNING,
		"Definition":   def,
		"StepOrder":    []string{"a", "b"},
		"Steps": map[string]StepState{
			"a": {StepID: "a", TaskName: "a", Status: logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SUCCEEDED, ResultJSON: []byte(`1`)},
			"b": {StepID: "b", TaskName: "b", Status: logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SCHEDULED},
		},
		"CreatedAtMs": int64(1),
		"UpdatedAtMs": int64(2),
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	stepDef, step, ok := state.PopReadyStep()
	if !ok || stepDef.StepID != "b" || step.StepID != "b" {
		t.Fatalf("ready step from legacy JSON = def:%q step:%q ok:%v, want b/b/true", stepDef.StepID, step.StepID, ok)
	}
	if _, ok := state.Step("a"); !ok {
		t.Fatal("legacy step a missing after runtime rebuild")
	}
}
