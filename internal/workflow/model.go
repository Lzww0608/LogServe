// Package workflow models LogServe workflow definitions, runtime state, argument resolution, and replay from the workflow event log.
package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/logserve/logserve/gen/logservepb"
)

// Definition is the durable workflow specification submitted to the control plane.
// Defaults are applied by ParseDefinition before the definition is stored or scheduled.
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

// StepDefinition describes one executable node in a workflow DAG.
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

// State is the replayable runtime snapshot for a workflow execution.
// The dag field is an in-memory acceleration structure rebuilt from Definition and StepState data.
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

// StepState records scheduler and execution progress for one workflow step.
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

// RuntimeDAG keeps topological step state plus dependency counters and a ready queue for efficient scheduling.
type RuntimeDAG struct {
	steps         []StepState
	byID          map[string]int
	outgoing      [][]int
	remainingDeps []int
	ready         []int
	readyHead     int
}

// ParseDefinition decodes a JSON workflow definition, applies defaults, validates the DAG, and returns steps in topological order.
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

	// Historical definitions may omit result_step_id; the last submitted step becomes the result step.
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

// ValidateDefinition checks required step IDs, result step membership, dependency references, and cycles.
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

	// DFS keeps separate visiting and visited sets so back-edges are reported as dependency cycles.
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

// NewState creates an initial running workflow state with a rebuilt runtime DAG and timestamps.
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

// StepDefinitionByID finds the durable definition for a step ID.
func StepDefinitionByID(def Definition, stepID string) (StepDefinition, bool) {
	for _, step := range def.Steps {
		if step.StepID == stepID {
			return step, true
		}
	}
	return StepDefinition{}, false
}

// StepMaxAttempts returns the step-specific retry limit, falling back to the workflow default.
func StepMaxAttempts(def Definition, stepID string) int {
	step, ok := StepDefinitionByID(def, stepID)
	if !ok || step.MaxAttempts <= 0 {
		return def.MaxAttempts
	}
	return step.MaxAttempts
}

// normalizeDefinition best-effort sorts steps topologically without rejecting already-validated callers.
func normalizeDefinition(def Definition) Definition {
	if ordered, err := topologicalStepDefinitions(def.Steps); err == nil {
		def.Steps = ordered
	}
	return def
}

// topologicalStepDefinitions orders steps with Kahn's algorithm and reports unknown dependencies or cycles.
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

	// The queue preserves the original order among simultaneously-ready steps, making scheduling deterministic.
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

// buildRuntimeDAG merges durable step definitions with existing step state and initializes dependency counters plus the ready queue.
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

			// Only unfinished dependencies count; replayed successful steps unlock their downstream nodes immediately.
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

// clone returns a deep copy of the runtime DAG so callers can mutate cloned State values independently.
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

// stepOrder returns the topological step ID order represented by the runtime DAG.
func (d RuntimeDAG) stepOrder() []string {
	out := make([]string, 0, len(d.steps))
	for _, step := range d.steps {
		out = append(out, step.StepID)
	}
	return out
}

// stepReady reports whether a DAG index is schedulable: all dependencies are done, the step is scheduled, and no task is in flight.
func (d RuntimeDAG) stepReady(idx int) bool {
	if idx < 0 || idx >= len(d.steps) || idx >= len(d.remainingDeps) {
		return false
	}
	step := d.steps[idx]
	return d.remainingDeps[idx] == 0 &&
		step.Status == logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SCHEDULED &&
		step.TaskID == ""
}

// popReady returns the next still-ready DAG index while skipping stale queue entries.
func (d *RuntimeDAG) popReady() (int, bool) {
	for d.readyHead < len(d.ready) {
		idx := d.ready[d.readyHead]
		d.readyHead++
		if d.stepReady(idx) {
			return idx, true
		}
	}

	// Compact the queue after it is fully drained so old stale indices are not retained.
	d.ready = nil
	d.readyHead = 0
	return 0, false
}

// ensureRuntime lazily rebuilds the in-memory DAG when a State was decoded or cloned without runtime fields.
func (s *State) ensureRuntime() {
	if s.dag.byID != nil && len(s.dag.steps) == len(s.StepOrder) {
		return
	}
	s.RebuildRuntime()
}

// RebuildRuntime reconstructs the runtime DAG from durable definition and current step states.
func (s *State) RebuildRuntime() {
	var steps []StepState

	// Preserve current step progress when rebuilding after status or task-ID transitions.
	if s.dag.byID != nil {
		steps = s.StepStatesInOrder()
	}
	s.Definition = normalizeDefinition(s.Definition)
	s.dag = buildRuntimeDAG(s.Definition, steps)
	s.StepOrder = s.dag.stepOrder()
}

// Step returns a defensive copy of the current state for a step ID.
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

// UpdateStep applies a mutation callback to one step and rebuilds scheduling metadata when readiness can change.
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
	oldStatus := step.Status
	oldTaskID := step.TaskID
	fn(&step)
	if step.StepID == "" {
		step.StepID = stepID
	}
	s.dag.steps[idx] = cloneStepState(step)

	// Status and task assignment affect ready-queue membership, so they require a DAG rebuild.
	if step.Status != oldStatus || step.TaskID != oldTaskID {
		s.RebuildRuntime()
	}
	return true
}

// StepAt returns the definition and state at a topological index.
func (s State) StepAt(idx int) (StepDefinition, StepState, bool) {
	if s.dag.byID == nil {
		s.RebuildRuntime()
	}
	if idx < 0 || idx >= len(s.dag.steps) || idx >= len(s.Definition.Steps) {
		return StepDefinition{}, StepState{}, false
	}
	return s.Definition.Steps[idx], cloneStepState(s.dag.steps[idx]), true
}

// StepStatesInOrder returns defensive copies of all step states in topological order.
func (s State) StepStatesInOrder() []StepState {
	if s.dag.byID == nil {
		s.RebuildRuntime()
	}
	return cloneStepStates(s.dag.steps)
}

// PopReadyStep returns the next ready step definition and state without marking it in-flight.
func (s *State) PopReadyStep() (StepDefinition, StepState, bool) {
	s.ensureRuntime()
	for {
		idx, ok := s.dag.popReady()
		if !ok {
			return StepDefinition{}, StepState{}, false
		}

		// A stale runtime index can appear after decode/rebuild edge cases; skip it instead of panicking.
		if idx >= len(s.Definition.Steps) {
			continue
		}
		return s.Definition.Steps[idx], cloneStepState(s.dag.steps[idx]), true
	}
}

// SetStepScheduled records that a workflow step has been assigned to a task attempt.
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

// SetStepStarted records the first worker start timestamp for a step task.
func (s *State) SetStepStarted(stepID, taskID string, nowMs int64) bool {
	s.ensureRuntime()
	idx, ok := s.dag.byID[stepID]
	if !ok {
		return false
	}
	step := s.dag.steps[idx]

	// Replayed or duplicate start events must not move an already-succeeded step backward.
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

// SetStepSucceeded stores a step result and unlocks downstream ready steps when this is the first success.
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

	// Duplicate success events are idempotent and must not decrement downstream dependency counters twice.
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

// SetStepFailed records a failed step and optionally requeues it for retry when dependencies are still satisfied.
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

// AllStepsSucceeded reports whether every step in the runtime DAG reached the succeeded status.
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

// CloneState returns a deep copy of workflow state, including JSON payload slices and runtime DAG metadata.
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

// CloneDefinition returns a deep copy of a workflow definition and all mutable slices.
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

// cloneStepStates deep-copies a slice of step states.
func cloneStepStates(source []StepState) []StepState {
	out := make([]StepState, len(source))
	for i, step := range source {
		out[i] = cloneStepState(step)
	}
	return out
}

// cloneStepState deep-copies JSON payload slices within one step state.
func cloneStepState(step StepState) StepState {
	step.ResultJSON = append([]byte(nil), step.ResultJSON...)
	step.ResolvedArgsJSON = append([]byte(nil), step.ResolvedArgsJSON...)
	return step
}

// stateJSON is the durable JSON shape for State without the in-memory runtime DAG.
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

// MarshalJSON serializes State using ordered step states and omits the derived runtime DAG.
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

// UnmarshalJSON restores State from the durable JSON shape and rebuilds the runtime DAG.
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

	// Very old snapshots may omit Steps; rebuild scheduled step states from the definition.
	if len(steps) == 0 && len(def.Steps) > 0 {
		steps = buildRuntimeDAG(def, nil).steps
	}
	s.dag = buildRuntimeDAG(def, steps)
	s.StepOrder = s.dag.stepOrder()
	return nil
}

// decodeStateSteps accepts the current ordered slice format and the legacy map keyed by step ID.
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

	// Sort legacy map keys to keep decode deterministic before the runtime DAG reapplies topological order.
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
