package workflow

import (
	"strings"
	"testing"
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
