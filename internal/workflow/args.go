package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/logserve/logserve/gen/logservepb"
)

const refKey = "__step_ref__"

type ResultLoader interface {
	LoadResult(ref string) ([]byte, error)
}

func ResolveArgs(step StepDefinition, state State, loader ResultLoader) ([]byte, string, error) {
	var value any
	if len(step.ArgsJSON) == 0 {
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

func ResolveCachedArgs(step StepDefinition, stepState StepState, state State, loader ResultLoader) ([]byte, string, error) {
	if stepState.LastInputHash != "" && len(stepState.ResolvedArgsJSON) > 0 {
		return append([]byte(nil), stepState.ResolvedArgsJSON...), stepState.LastInputHash, nil
	}
	return ResolveArgs(step, state, loader)
}
func DependenciesSucceeded(step StepDefinition, state State) bool {
	for _, dep := range step.DependsOn {
		depState, ok := state.Steps[dep]
		if !ok || depState.Status != logservepb.WorkflowStepStatus_WORKFLOW_STEP_STATUS_SUCCEEDED {
			return false
		}
	}
	return true
}

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
		if ref, ok := typed[refKey]; ok && len(typed) == 1 {
			stepID, ok := ref.(string)
			if !ok {
				return nil, errors.New("step ref must be a string")
			}
			step, ok := state.Steps[stepID]
			if !ok {
				return nil, errors.New("referenced step not found")
			}
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
