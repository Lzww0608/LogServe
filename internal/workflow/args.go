package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/logserve/logserve/gen/logservepb"
)

// refKey is the sentinel object key that replaces a JSON value with another step's result.
const refKey = "__step_ref__"

// ResultLoader loads a large step result by reference when the workflow state stores ResultRef instead of inline ResultJSON.
type ResultLoader interface {
	LoadResult(ref string) ([]byte, error)
}

// ResolveArgs resolves step argument JSON by recursively replacing step references and returns canonical JSON plus its hash.
func ResolveArgs(step StepDefinition, state State, loader ResultLoader) ([]byte, string, error) {
	var value any
	if len(step.ArgsJSON) == 0 {

		// Empty args use the same envelope shape as submitted tasks so the input hash is stable.
		value = map[string]any{"args": []any{}, "kwargs": map[string]any{}}
	} else if err := json.Unmarshal(step.ArgsJSON, &value); err != nil {
		return nil, "", err
	}
	resolved, err := resolveValue(value, state, loader)
	if err != nil {
		return nil, "", err
	}
	data, err := json.Marshal(resolved)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}

// ResolveCachedArgs reuses the resolved argument snapshot from scheduling replay when available.
func ResolveCachedArgs(step StepDefinition, stepState StepState, state State, loader ResultLoader) ([]byte, string, error) {
	if stepState.LastInputHash != "" && len(stepState.ResolvedArgsJSON) > 0 {
		return append([]byte(nil), stepState.ResolvedArgsJSON...), stepState.LastInputHash, nil
	}
	return ResolveArgs(step, state, loader)
}

// DependenciesSucceeded reports whether every declared dependency has reached succeeded state.
func DependenciesSucceeded(step StepDefinition, state State) bool {
	for _, dep := range step.DependsOn {
		depState, ok := state.Step(dep)
		if !ok || depState.Status != logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SUCCEEDED {
			return false
		}
	}
	return true
}

// resolveValue recursively walks arbitrary JSON values and expands sole-key step reference objects.
func resolveValue(value any, state State, loader ResultLoader) (any, error) {
	switch typed := value.(type) {
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			resolved, err := resolveValue(item, state, loader)
			if err != nil {
				return nil, err
			}
			out = append(out, resolved)
		}
		return out, nil
	case map[string]any:

		// The sentinel is only special when it is the entire object; mixed objects keep normal JSON semantics.
		if ref, ok := typed[refKey]; ok && len(typed) == 1 {
			stepID, ok := ref.(string)
			if !ok {
				return nil, errors.New("step ref must be a string")
			}
			step, ok := state.Step(stepID)
			if !ok {
				return nil, errors.New("referenced step not found")
			}

			// Large results may live outside workflow state, so dereference them lazily during argument resolution.
			if len(step.ResultJSON) == 0 && step.ResultRef != "" {
				if loader == nil {
					return nil, errors.New("result loader is required")
				}
				data, err := loader.LoadResult(step.ResultRef)
				if err != nil {
					return nil, err
				}
				return decodeJSON(data)
			}
			return decodeJSON(step.ResultJSON)
		}
		out := make(map[string]any, len(typed))
		for k, v := range typed {
			resolved, err := resolveValue(v, state, loader)
			if err != nil {
				return nil, err
			}
			out[k] = resolved
		}
		return out, nil
	default:
		return typed, nil
	}
}

// decodeJSON converts raw result JSON into a generic value for embedding into resolved args.
func decodeJSON(data []byte) (any, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}
