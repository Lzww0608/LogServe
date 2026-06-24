package workflow

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/logserve/logserve/gen/logservepb"
)

type Definition struct {
	WorkflowName   string           `json:"workflow_name"`
	FunctionSource string           `json:"function_source"`
	FunctionRef    string           `json:"function_ref,omitempty"`
	FunctionHash   string           `json:"function_hash,omitempty"`
	ArgsJSON       json.RawMessage  `json:"args_json"`
	Steps          []StepDefinition `json:"steps"`
	ResultStepID   string           `json:"result_step_id"`
	MaxAttempts    int              `json:"max_attempts"`
	TimeoutMs      int64            `json:"timeout_ms"`
}

type StepDefinition struct {
	StepID          string          `json:"step_id"`
	TaskName        string          `json:"task_name"`
	FunctionName    string          `json:"function_name"`
	FunctionSource  string          `json:"function_source"`
	FunctionRef     string          `json:"function_ref,omitempty"`
	FunctionHash    string          `json:"function_hash,omitempty"`
	ArgsJSON        json.RawMessage `json:"args_json"`
	DependsOn       []string        `json:"depends_on"`
	MaxAttempts     int             `json:"max_attempts"`
	TimeoutMs       int64           `json:"timeout_ms"`
	LLMModelName    string          `json:"llm_model_name,omitempty"`
	LLMModelVersion string          `json:"llm_model_version,omitempty"`
	LLMAdapter      string          `json:"llm_adapter,omitempty"`
	LLMMaxTokens    uint32          `json:"llm_max_tokens,omitempty"`
}

type State struct {
	WorkflowID             string
	WorkflowName           string
	Status                 logservepb.WorkflowStatus
	Definition             Definition
	StepOrder              []string
	Steps                  map[string]StepState
	ResultJSON             []byte
	ResultRef              string
	Error                  string
	IdempotencyKey         string
	IdempotencyFingerprint string
	CreatedAtMs            int64
	UpdatedAtMs            int64
	CompletedAtMs          int64
}

type StepState struct {
	StepID            string
	TaskName          string
	Status            logservepb.WorkflowStepStatus
	Attempts          uint32
	TaskID            string
	ResultJSON        []byte
	ResultRef         string
	Error             string
	StartedAtMs       int64
	CompletedAtMs     int64
	LatencyMs         int64
	LastInputHash     string
	ResolvedArgsJSON  []byte
	LastScheduledAtMs int64
}

func ParseDefinition(data []byte) (Definition, error) {
	var def Definition
	if err := json.Unmarshal(data, &def); err != nil {
		return Definition{}, err
	}
	if def.MaxAttempts <= 0 {
		def.MaxAttempts = 3
	}
	if def.TimeoutMs <= 0 {
		def.TimeoutMs = 30_000
	}
	for i := range def.Steps {
		if def.Steps[i].MaxAttempts <= 0 {
			def.Steps[i].MaxAttempts = def.MaxAttempts
		}
		if def.Steps[i].TimeoutMs <= 0 {
			def.Steps[i].TimeoutMs = def.TimeoutMs
		}
	}
	if def.ResultStepID == "" && len(def.Steps) > 0 {
		def.ResultStepID = def.Steps[len(def.Steps)-1].StepID
	}
	if err := ValidateDefinition(def); err != nil {
		return Definition{}, err
	}
	return def, nil
}

func ValidateDefinition(def Definition) error {
	if len(def.Steps) == 0 {
		return errors.New("workflow must contain at least one step")
	}
	steps := make(map[string]StepDefinition, len(def.Steps))
	for i, step := range def.Steps {
		if step.StepID == "" {
			return fmt.Errorf("workflow step %d has empty step_id", i)
		}
		if _, exists := steps[step.StepID]; exists {
			return fmt.Errorf("duplicate workflow step_id %q", step.StepID)
		}
		steps[step.StepID] = step
	}
	if def.ResultStepID == "" {
		return errors.New("result_step_id is required")
	}
	if _, ok := steps[def.ResultStepID]; !ok {
		return fmt.Errorf("result_step_id %q does not match any step", def.ResultStepID)
	}
	for _, step := range def.Steps {
		for _, dep := range step.DependsOn {
			if dep == "" {
				return fmt.Errorf("step %q has empty dependency", step.StepID)
			}
			if _, ok := steps[dep]; !ok {
				return fmt.Errorf("step %q depends on unknown step %q", step.StepID, dep)
			}
		}
	}
	visiting := map[string]bool{}
	visited := map[string]bool{}
	var visit func(string) error
	visit = func(stepID string) error {
		if visiting[stepID] {
			return fmt.Errorf("workflow dependency cycle includes step %q", stepID)
		}
		if visited[stepID] {
			return nil
		}
		visiting[stepID] = true
		for _, dep := range steps[stepID].DependsOn {
			if err := visit(dep); err != nil {
				return err
			}
		}
		visiting[stepID] = false
		visited[stepID] = true
		return nil
	}
	for _, step := range def.Steps {
		if err := visit(step.StepID); err != nil {
			return err
		}
	}
	return nil
}
func NewState(workflowID string, def Definition, nowMs int64) State {
	steps := make(map[string]StepState, len(def.Steps))
	order := make([]string, 0, len(def.Steps))
	for _, step := range def.Steps {
		order = append(order, step.StepID)
		steps[step.StepID] = StepState{
			StepID:   step.StepID,
			TaskName: step.TaskName,
			Status:   logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SCHEDULED,
		}
	}
	return State{
		WorkflowID:   workflowID,
		WorkflowName: def.WorkflowName,
		Status:       logservepb.WorkflowStatus_WORKFLOW_STATUS_RUNNING,
		Definition:   def,
		StepOrder:    order,
		Steps:        steps,
		CreatedAtMs:  nowMs,
		UpdatedAtMs:  nowMs,
	}
}

func StepDefinitionByID(def Definition, stepID string) (StepDefinition, bool) {
	for _, step := range def.Steps {
		if step.StepID == stepID {
			return step, true
		}
	}
	return StepDefinition{}, false
}

func StepMaxAttempts(def Definition, stepID string) int {
	step, ok := StepDefinitionByID(def, stepID)
	if !ok || step.MaxAttempts <= 0 {
		return def.MaxAttempts
	}
	return step.MaxAttempts
}
