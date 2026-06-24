package workflow

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/logserve/logserve/internal/logrecord"
)

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
	step := state.Steps["step-1"]
	if step.TaskID != "task-1" || step.LastInputHash != "hash" {
		t.Fatalf("step after raw replay = %+v", step)
	}
}

var errFailingLoader = errors.New("result loader should not be called")

type failingResultLoader struct{}

func (failingResultLoader) LoadResult(string) ([]byte, error) {
	return nil, errFailingLoader
}

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
