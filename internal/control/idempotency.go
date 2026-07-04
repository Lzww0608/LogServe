package control

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/workflow"
)

// taskSpecFingerprint returns the stable identity used to validate repeated task
// submissions with the same idempotency key.
func taskSpecFingerprint(spec *logservepb.TaskSpec) (string, error) {
	args, err := jsonValueForFingerprint(spec.GetArgsJson())
	if err != nil {
		return "", err
	}
	return stableFingerprint(map[string]any{
		"task_name":          spec.GetTaskName(),
		"function_name":      spec.GetFunctionName(),
		"function_hash":      taskFunctionIdentity(spec),
		"args_json":          args,
		"workflow_id":        spec.GetWorkflowId(),
		"step_id":            spec.GetStepId(),
		"actor_id":           spec.GetActorId(),
		"actor_class_name":   spec.GetActorClassName(),
		"actor_class_source": spec.GetActorClassSource(),
		"actor_method":       spec.GetActorMethod(),
		"llm_model_name":     spec.GetLlmModelName(),
		"llm_model_version":  spec.GetLlmModelVersion(),
		"llm_adapter":        spec.GetLlmAdapter(),
		"llm_max_tokens":     spec.GetLlmMaxTokens(),
		"timeout_ms":         spec.GetTimeoutMs(),
	})
}

// workflowFingerprint returns the stable identity for a workflow submission.
func workflowFingerprint(workflowName string, def workflow.Definition) (string, error) {
	definition, err := json.Marshal(def)
	if err != nil {
		return "", err
	}
	definitionValue, err := jsonValueForFingerprint(definition)
	if err != nil {
		return "", err
	}
	return stableFingerprint(map[string]any{
		"workflow_name":   workflowName,
		"definition_json": definitionValue,
	})
}

// actorCreateFingerprint returns the stable identity for actor creation requests.
func actorCreateFingerprint(req *logservepb.CreateActorRequest, initArgs []byte, snapshotEvery uint32) (string, error) {
	initArgsValue, err := jsonValueForFingerprint(initArgs)
	if err != nil {
		return "", err
	}
	return stableFingerprint(map[string]any{
		"class_name":     req.GetClassName(),
		"class_source":   req.GetClassSource(),
		"init_args_json": initArgsValue,
		"snapshot_every": snapshotEvery,
	})
}

// ensureIdempotencyFingerprint rejects reuse of an idempotency key with different
// semantic request content.
func ensureIdempotencyFingerprint(kind, key, existing, requested string) error {
	if key == "" || existing == "" || existing == requested {
		return nil
	}
	return fmt.Errorf("idempotency conflict for %s key %q", kind, key)
}

// stableFingerprint hashes JSON-marshaled structured values for idempotency checks.
func stableFingerprint(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// jsonValueForFingerprint compacts and unmarshals JSON so whitespace and object
// formatting do not change an idempotency fingerprint.
func jsonValueForFingerprint(data []byte) (any, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		return nil, err
	}
	var value any
	if err := json.Unmarshal(compact.Bytes(), &value); err != nil {
		return nil, err
	}
	return value, nil
}
