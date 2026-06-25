package workflow

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/logserve/logserve/gen/logservepb"
)

func buildLinearDAGState(steps int) State {
	defSteps := make([]StepDefinition, 0, steps)
	for i := 0; i < steps; i++ {
		stepID := fmt.Sprintf("step-%d", i)
		def := StepDefinition{
			StepID:         stepID,
			TaskName:       stepID,
			FunctionName:   "noop",
			FunctionSource: "def noop():\n    return 1\n",
		}
		if i > 0 {
			def.DependsOn = []string{fmt.Sprintf("step-%d", i-1)}
		}
		defSteps = append(defSteps, def)
	}
	raw, err := json.Marshal(Definition{
		WorkflowName: "wf-bench",
		Steps:        defSteps,
	})
	if err != nil {
		panic(err)
	}
	def, err := ParseDefinition(raw)
	if err != nil {
		panic(err)
	}
	return NewState("wf-bench", def, 0)
}

func buildFanOutDAGState(width int) State {
	defSteps := make([]StepDefinition, 0, width+1)
	defSteps = append(defSteps, StepDefinition{
		StepID:         "root",
		TaskName:       "root",
		FunctionName:   "noop",
		FunctionSource: "def noop():\n    return 1\n",
	})
	for i := 0; i < width; i++ {
		defSteps = append(defSteps, StepDefinition{
			StepID:         fmt.Sprintf("leaf-%d", i),
			TaskName:       fmt.Sprintf("leaf-%d", i),
			FunctionName:   "noop",
			FunctionSource: "def noop():\n    return 1\n",
			DependsOn:      []string{"root"},
		})
	}
	raw, err := json.Marshal(Definition{
		WorkflowName: "wf-fanout",
		Steps:        defSteps,
	})
	if err != nil {
		panic(err)
	}
	def, err := ParseDefinition(raw)
	if err != nil {
		panic(err)
	}
	return NewState("wf-fanout", def, 0)
}

func runReadyQueueSchedule(state *State) int {
	scheduled := 0
	for {
		stepDef, _, ok := state.PopReadyStep()
		if !ok {
			break
		}
		taskID := fmt.Sprintf("task-%s", stepDef.StepID)
		state.SetStepScheduled(stepDef.StepID, taskID, 1, "hash", nil, int64(scheduled))
		state.SetStepSucceeded(stepDef.StepID, taskID, []byte(`{"ok":true}`), "", int64(scheduled), 1)
		scheduled++
	}
	return scheduled
}

func runNaiveScanSchedule(state *State) int {
	scheduled := 0
	for scheduled < len(state.Definition.Steps) {
		var pickedDef StepDefinition
		var pickedState StepState
		found := false
		for _, stepDef := range state.Definition.Steps {
			step, ok := state.Step(stepDef.StepID)
			if !ok {
				continue
			}
			if step.Status != logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SCHEDULED || step.TaskID != "" {
				continue
			}
			if !DependenciesSucceeded(stepDef, *state) {
				continue
			}
			pickedDef = stepDef
			pickedState = step
			found = true
			break
		}
		if !found {
			break
		}
		taskID := fmt.Sprintf("task-%s", pickedDef.StepID)
		state.SetStepScheduled(pickedDef.StepID, taskID, 1, "hash", nil, int64(scheduled))
		state.SetStepSucceeded(pickedDef.StepID, taskID, []byte(`{"ok":true}`), "", int64(scheduled), 1)
		_ = pickedState
		scheduled++
	}
	return scheduled
}

func BenchmarkScheduleLinearDAGReadyQueue(b *testing.B) {
	for _, steps := range []int{32, 128, 512, 1024} {
		b.Run(fmt.Sprintf("steps=%d", steps), func(b *testing.B) {
			seed := buildLinearDAGState(steps)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				state := CloneState(seed)
				if got := runReadyQueueSchedule(&state); got != steps {
					b.Fatalf("scheduled %d, want %d", got, steps)
				}
			}
		})
	}
}

func BenchmarkScheduleLinearDAGNaiveScan(b *testing.B) {
	for _, steps := range []int{32, 128, 512, 1024} {
		b.Run(fmt.Sprintf("steps=%d", steps), func(b *testing.B) {
			seed := buildLinearDAGState(steps)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				state := CloneState(seed)
				if got := runNaiveScanSchedule(&state); got != steps {
					b.Fatalf("scheduled %d, want %d", got, steps)
				}
			}
		})
	}
}

func BenchmarkScheduleFanOutDAGReadyQueue(b *testing.B) {
	for _, width := range []int{32, 128, 512} {
		b.Run(fmt.Sprintf("leaves=%d", width), func(b *testing.B) {
			seed := buildFanOutDAGState(width)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				state := CloneState(seed)
				if got := runReadyQueueSchedule(&state); got != width+1 {
					b.Fatalf("scheduled %d, want %d", got, width+1)
				}
			}
		})
	}
}

func BenchmarkScheduleFanOutDAGNaiveScan(b *testing.B) {
	for _, width := range []int{32, 128, 512} {
		b.Run(fmt.Sprintf("leaves=%d", width), func(b *testing.B) {
			seed := buildFanOutDAGState(width)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				state := CloneState(seed)
				if got := runNaiveScanSchedule(&state); got != width+1 {
					b.Fatalf("scheduled %d, want %d", got, width+1)
				}
			}
		})
	}
}
