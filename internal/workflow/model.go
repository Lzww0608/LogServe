package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

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
	ResultJSON             []byte
	ResultRef              string
	Error                  string
	IdempotencyKey         string
	IdempotencyFingerprint string
	CreatedAtMs            int64
	UpdatedAtMs            int64
	CompletedAtMs          int64
	dag                    RuntimeDAG
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

type RuntimeDAG struct {
	steps         []StepState
	byID          map[string]int
	outgoing      [][]int
	remainingDeps []int
	ready         []int
	readyHead     int
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
	ordered, err := topologicalStepDefinitions(def.Steps)
	if err != nil {
		return Definition{}, err
	}
	def.Steps = ordered
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
	def = normalizeDefinition(def)
	dag := buildRuntimeDAG(def, nil)
	return State{
		WorkflowID:   workflowID,
		WorkflowName: def.WorkflowName,
		Status:       logservepb.WorkflowStatus_WORKFLOW_STATUS_RUNNING,
		Definition:   def,
		StepOrder:    dag.stepOrder(),
		CreatedAtMs:  nowMs,
		UpdatedAtMs:  nowMs,
		dag:          dag,
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

func normalizeDefinition(def Definition) Definition {
	if ordered, err := topologicalStepDefinitions(def.Steps); err == nil {
		def.Steps = ordered
	}
	return def
}

func topologicalStepDefinitions(steps []StepDefinition) ([]StepDefinition, error) {
	byID := make(map[string]int, len(steps))
	for i, step := range steps {
		byID[step.StepID] = i
	}
	indegree := make([]int, len(steps))
	outgoing := make([][]int, len(steps))
	for i, step := range steps {
		for _, dep := range step.DependsOn {
			depIdx, ok := byID[dep]
			if !ok {
				return nil, fmt.Errorf("step %q depends on unknown step %q", step.StepID, dep)
			}
			indegree[i]++
			outgoing[depIdx] = append(outgoing[depIdx], i)
		}
	}
	queue := make([]int, 0, len(steps))
	for i := range steps {
		if indegree[i] == 0 {
			queue = append(queue, i)
		}
	}
	ordered := make([]StepDefinition, 0, len(steps))
	for head := 0; head < len(queue); head++ {
		idx := queue[head]
		ordered = append(ordered, steps[idx])
		for _, next := range outgoing[idx] {
			indegree[next]--
			if indegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	if len(ordered) != len(steps) {
		return nil, errors.New("workflow dependency cycle detected")
	}
	return ordered, nil
}

func buildRuntimeDAG(def Definition, existing []StepState) RuntimeDAG {
	def = normalizeDefinition(def)
	existingByID := make(map[string]StepState, len(existing))
	for _, step := range existing {
		if step.StepID != "" {
			existingByID[step.StepID] = step
		}
	}
	dag := RuntimeDAG{
		steps:         make([]StepState, 0, len(def.Steps)),
		byID:          make(map[string]int, len(def.Steps)),
		outgoing:      make([][]int, len(def.Steps)),
		remainingDeps: make([]int, len(def.Steps)),
	}
	for _, stepDef := range def.Steps {
		step, ok := existingByID[stepDef.StepID]
		if !ok {
			step = StepState{
				StepID:   stepDef.StepID,
				TaskName: stepDef.TaskName,
				Status:   logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SCHEDULED,
			}
		}
		step.StepID = stepDef.StepID
		if step.TaskName == "" {
			step.TaskName = stepDef.TaskName
		}
		dag.byID[step.StepID] = len(dag.steps)
		dag.steps = append(dag.steps, cloneStepState(step))
	}
	for i, stepDef := range def.Steps {
		for _, dep := range stepDef.DependsOn {
			depIdx, ok := dag.byID[dep]
			if !ok {
				continue
			}
			dag.outgoing[depIdx] = append(dag.outgoing[depIdx], i)
			if dag.steps[depIdx].Status != logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SUCCEEDED {
				dag.remainingDeps[i]++
			}
		}
	}
	for i := range dag.steps {
		if dag.stepReady(i) {
			dag.ready = append(dag.ready, i)
		}
	}
	return dag
}

func (d RuntimeDAG) clone() RuntimeDAG {
	out := RuntimeDAG{
		steps:         cloneStepStates(d.steps),
		byID:          make(map[string]int, len(d.byID)),
		outgoing:      make([][]int, len(d.outgoing)),
		remainingDeps: append([]int(nil), d.remainingDeps...),
		ready:         append([]int(nil), d.ready...),
		readyHead:     d.readyHead,
	}
	for id, idx := range d.byID {
		out.byID[id] = idx
	}
	for i := range d.outgoing {
		out.outgoing[i] = append([]int(nil), d.outgoing[i]...)
	}
	return out
}

func (d RuntimeDAG) stepOrder() []string {
	out := make([]string, 0, len(d.steps))
	for _, step := range d.steps {
		out = append(out, step.StepID)
	}
	return out
}

func (d RuntimeDAG) stepReady(idx int) bool {
	if idx < 0 || idx >= len(d.steps) || idx >= len(d.remainingDeps) {
		return false
	}
	step := d.steps[idx]
	return d.remainingDeps[idx] == 0 &&
		step.Status == logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SCHEDULED &&
		step.TaskID == ""
}

func (d *RuntimeDAG) popReady() (int, bool) {
	for d.readyHead < len(d.ready) {
		idx := d.ready[d.readyHead]
		d.readyHead++
		if d.stepReady(idx) {
			return idx, true
		}
	}
	d.ready = nil
	d.readyHead = 0
	return 0, false
}

func (s *State) ensureRuntime() {
	if s.dag.byID != nil && len(s.dag.steps) == len(s.StepOrder) {
		return
	}
	s.RebuildRuntime()
}

func (s *State) RebuildRuntime() {
	var steps []StepState
	if s.dag.byID != nil {
		steps = s.StepStatesInOrder()
	}
	s.Definition = normalizeDefinition(s.Definition)
	s.dag = buildRuntimeDAG(s.Definition, steps)
	s.StepOrder = s.dag.stepOrder()
}

func (s State) Step(stepID string) (StepState, bool) {
	if s.dag.byID == nil {
		s.RebuildRuntime()
	}
	idx, ok := s.dag.byID[stepID]
	if !ok || idx < 0 || idx >= len(s.dag.steps) {
		return StepState{}, false
	}
	return cloneStepState(s.dag.steps[idx]), true
}
func (s *State) UpdateStep(stepID string, fn func(*StepState)) bool {
	if fn == nil {
		return false
	}
	s.ensureRuntime()
	idx, ok := s.dag.byID[stepID]
	if !ok || idx < 0 || idx >= len(s.dag.steps) {
		return false
	}
	step := cloneStepState(s.dag.steps[idx])
	fn(&step)
	if step.StepID == "" {
		step.StepID = stepID
	}
	s.dag.steps[idx] = cloneStepState(step)
	s.RebuildRuntime()
	return true
}

func (s State) StepAt(idx int) (StepDefinition, StepState, bool) {
	if s.dag.byID == nil {
		s.RebuildRuntime()
	}
	if idx < 0 || idx >= len(s.dag.steps) || idx >= len(s.Definition.Steps) {
		return StepDefinition{}, StepState{}, false
	}
	return s.Definition.Steps[idx], cloneStepState(s.dag.steps[idx]), true
}

func (s State) StepStatesInOrder() []StepState {
	if s.dag.byID == nil {
		s.RebuildRuntime()
	}
	return cloneStepStates(s.dag.steps)
}

func (s *State) PopReadyStep() (StepDefinition, StepState, bool) {
	s.ensureRuntime()
	for {
		idx, ok := s.dag.popReady()
		if !ok {
			return StepDefinition{}, StepState{}, false
		}
		if idx >= len(s.Definition.Steps) {
			continue
		}
		return s.Definition.Steps[idx], cloneStepState(s.dag.steps[idx]), true
	}
}

func (s *State) SetStepScheduled(stepID, taskID string, attempt uint32, inputHash string, resolvedArgsJSON []byte, nowMs int64) bool {
	s.ensureRuntime()
	idx, ok := s.dag.byID[stepID]
	if !ok {
		return false
	}
	step := s.dag.steps[idx]
	step.Status = logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SCHEDULED
	step.TaskID = taskID
	step.Attempts = attempt
	step.LastInputHash = inputHash
	step.LastScheduledAtMs = nowMs
	step.Error = ""
	if resolvedArgsJSON != nil {
		step.ResolvedArgsJSON = append([]byte(nil), resolvedArgsJSON...)
	}
	s.dag.steps[idx] = step
	s.UpdatedAtMs = nowMs
	return true
}

func (s *State) SetStepStarted(stepID, taskID string, nowMs int64) bool {
	s.ensureRuntime()
	idx, ok := s.dag.byID[stepID]
	if !ok {
		return false
	}
	step := s.dag.steps[idx]
	if step.Status == logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SUCCEEDED {
		return true
	}
	step.Status = logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_STARTED
	step.TaskID = taskID
	step.StartedAtMs = nowMs
	s.dag.steps[idx] = step
	s.UpdatedAtMs = nowMs
	return true
}

func (s *State) SetStepSucceeded(stepID, taskID string, resultJSON []byte, resultRef string, nowMs, latencyMs int64) bool {
	s.ensureRuntime()
	idx, ok := s.dag.byID[stepID]
	if !ok {
		return false
	}
	step := s.dag.steps[idx]
	wasSucceeded := step.Status == logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SUCCEEDED
	step.Status = logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SUCCEEDED
	step.TaskID = taskID
	step.ResultJSON = append([]byte(nil), resultJSON...)
	step.ResultRef = resultRef
	step.Error = ""
	step.CompletedAtMs = nowMs
	step.LatencyMs = latencyMs
	s.dag.steps[idx] = step
	s.UpdatedAtMs = nowMs
	if wasSucceeded {
		return true
	}
	for _, next := range s.dag.outgoing[idx] {
		if next < 0 || next >= len(s.dag.remainingDeps) {
			continue
		}
		if s.dag.remainingDeps[next] > 0 {
			s.dag.remainingDeps[next]--
		}
		if s.dag.stepReady(next) {
			s.dag.ready = append(s.dag.ready, next)
		}
	}
	return true
}

func (s *State) SetStepFailed(stepID, taskID, stepErr string, retry bool, nowMs, latencyMs int64) bool {
	s.ensureRuntime()
	idx, ok := s.dag.byID[stepID]
	if !ok {
		return false
	}
	step := s.dag.steps[idx]
	step.Status = logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_FAILED
	step.TaskID = taskID
	step.Error = stepErr
	step.CompletedAtMs = nowMs
	step.LatencyMs = latencyMs
	if retry {
		step.Status = logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SCHEDULED
		step.TaskID = ""
	}
	s.dag.steps[idx] = step
	s.UpdatedAtMs = nowMs
	if retry && s.dag.stepReady(idx) {
		s.dag.ready = append(s.dag.ready, idx)
	}
	return true
}

func (s State) AllStepsSucceeded() bool {
	if s.dag.byID == nil {
		s.RebuildRuntime()
	}
	for _, step := range s.dag.steps {
		if step.Status != logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SUCCEEDED {
			return false
		}
	}
	return true
}

func CloneState(state State) State {
	if state.dag.byID == nil {
		state.RebuildRuntime()
	}
	state.ResultJSON = append([]byte(nil), state.ResultJSON...)
	state.StepOrder = append([]string(nil), state.StepOrder...)
	state.Definition = CloneDefinition(state.Definition)
	state.dag = state.dag.clone()
	return state
}

func CloneDefinition(def Definition) Definition {
	def.ArgsJSON = append([]byte(nil), def.ArgsJSON...)
	def.FunctionSource = string([]byte(def.FunctionSource))
	def.Steps = append([]StepDefinition(nil), def.Steps...)
	for i := range def.Steps {
		def.Steps[i].ArgsJSON = append([]byte(nil), def.Steps[i].ArgsJSON...)
		def.Steps[i].DependsOn = append([]string(nil), def.Steps[i].DependsOn...)
		def.Steps[i].FunctionSource = string([]byte(def.Steps[i].FunctionSource))
	}
	return def
}

func cloneStepStates(source []StepState) []StepState {
	out := make([]StepState, len(source))
	for i, step := range source {
		out[i] = cloneStepState(step)
	}
	return out
}

func cloneStepState(step StepState) StepState {
	step.ResultJSON = append([]byte(nil), step.ResultJSON...)
	step.ResolvedArgsJSON = append([]byte(nil), step.ResolvedArgsJSON...)
	return step
}

type stateJSON struct {
	WorkflowID             string                    `json:"WorkflowID,omitempty"`
	WorkflowName           string                    `json:"WorkflowName,omitempty"`
	Status                 logservepb.WorkflowStatus `json:"Status,omitempty"`
	Definition             Definition                `json:"Definition"`
	StepOrder              []string                  `json:"StepOrder,omitempty"`
	Steps                  json.RawMessage           `json:"Steps,omitempty"`
	ResultJSON             []byte                    `json:"ResultJSON,omitempty"`
	ResultRef              string                    `json:"ResultRef,omitempty"`
	Error                  string                    `json:"Error,omitempty"`
	IdempotencyKey         string                    `json:"IdempotencyKey,omitempty"`
	IdempotencyFingerprint string                    `json:"IdempotencyFingerprint,omitempty"`
	CreatedAtMs            int64                     `json:"CreatedAtMs,omitempty"`
	UpdatedAtMs            int64                     `json:"UpdatedAtMs,omitempty"`
	CompletedAtMs          int64                     `json:"CompletedAtMs,omitempty"`
}

func (s State) MarshalJSON() ([]byte, error) {
	if s.dag.byID == nil {
		s.RebuildRuntime()
	}
	steps, err := json.Marshal(s.StepStatesInOrder())
	if err != nil {
		return nil, err
	}
	return json.Marshal(stateJSON{
		WorkflowID:             s.WorkflowID,
		WorkflowName:           s.WorkflowName,
		Status:                 s.Status,
		Definition:             s.Definition,
		StepOrder:              append([]string(nil), s.StepOrder...),
		Steps:                  steps,
		ResultJSON:             append([]byte(nil), s.ResultJSON...),
		ResultRef:              s.ResultRef,
		Error:                  s.Error,
		IdempotencyKey:         s.IdempotencyKey,
		IdempotencyFingerprint: s.IdempotencyFingerprint,
		CreatedAtMs:            s.CreatedAtMs,
		UpdatedAtMs:            s.UpdatedAtMs,
		CompletedAtMs:          s.CompletedAtMs,
	})
}

func (s *State) UnmarshalJSON(data []byte) error {
	var raw stateJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	steps, err := decodeStateSteps(raw.Steps)
	if err != nil {
		return err
	}
	def := normalizeDefinition(raw.Definition)
	*s = State{
		WorkflowID:             raw.WorkflowID,
		WorkflowName:           raw.WorkflowName,
		Status:                 raw.Status,
		Definition:             def,
		StepOrder:              append([]string(nil), raw.StepOrder...),
		ResultJSON:             append([]byte(nil), raw.ResultJSON...),
		ResultRef:              raw.ResultRef,
		Error:                  raw.Error,
		IdempotencyKey:         raw.IdempotencyKey,
		IdempotencyFingerprint: raw.IdempotencyFingerprint,
		CreatedAtMs:            raw.CreatedAtMs,
		UpdatedAtMs:            raw.UpdatedAtMs,
		CompletedAtMs:          raw.CompletedAtMs,
	}
	if len(steps) == 0 && len(def.Steps) > 0 {
		steps = buildRuntimeDAG(def, nil).steps
	}
	s.dag = buildRuntimeDAG(def, steps)
	s.StepOrder = s.dag.stepOrder()
	return nil
}

func decodeStateSteps(raw json.RawMessage) ([]StepState, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var steps []StepState
	if raw[0] == '[' {
		if err := json.Unmarshal(raw, &steps); err != nil {
			return nil, err
		}
		return steps, nil
	}
	legacy := make(map[string]StepState)
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(legacy))
	for id := range legacy {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	steps = make([]StepState, 0, len(ids))
	for _, id := range ids {
		step := legacy[id]
		if step.StepID == "" {
			step.StepID = id
		}
		steps = append(steps, step)
	}
	return steps, nil
}
