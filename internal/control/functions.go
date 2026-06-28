package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
	"github.com/logserve/logserve/internal/logrecord"
	"github.com/logserve/logserve/internal/objectstore"
	"github.com/logserve/logserve/internal/workflow"
)

const functionRegistryStream = "system:functions"

type functionRegisteredPayload struct {
	FunctionHash string `json:"function_hash"`
	SourceRef    string `json:"source_ref"`
	Language     string `json:"language"`
	Entrypoint   string `json:"entrypoint"`
	TimestampMs  int64  `json:"timestamp_ms,omitempty"`
}

func (s *Service) normalizeTaskFunction(ctx context.Context, spec *logservepb.TaskSpec) error {
	if spec == nil || !requiresPythonFunction(spec) {
		return nil
	}
	ref, hash, err := s.ensureFunctionRegistered(ctx, spec.GetFunctionName(), spec.GetFunctionSource(), spec.GetFunctionRef(), spec.GetFunctionHash())
	if err != nil {
		return err
	}
	spec.FunctionRef = ref
	spec.FunctionHash = hash
	spec.FunctionSource = ""
	return nil
}

func (s *Service) normalizeWorkflowDefinition(ctx context.Context, def *workflow.Definition) error {
	if def == nil {
		return nil
	}
	if strings.TrimSpace(def.FunctionSource) != "" || strings.TrimSpace(def.FunctionHash) != "" || strings.TrimSpace(def.FunctionRef) != "" {
		ref, hash, err := s.ensureFunctionRegistered(ctx, def.WorkflowName, def.FunctionSource, def.FunctionRef, def.FunctionHash)
		if err != nil {
			return err
		}
		def.FunctionRef = ref
		def.FunctionHash = hash
		def.FunctionSource = ""
	}
	for i := range def.Steps {
		step := &def.Steps[i]
		if isWorkflowLLMStep(*step) {
			if err := s.normalizeWorkflowLLMStep(step); err != nil {
				return fmt.Errorf("workflow step %q: %w", step.StepID, err)
			}
			continue
		}
		ref, hash, err := s.ensureFunctionRegistered(ctx, step.FunctionName, step.FunctionSource, step.FunctionRef, step.FunctionHash)
		if err != nil {
			return fmt.Errorf("workflow step %q: %w", step.StepID, err)
		}
		step.FunctionRef = ref
		step.FunctionHash = hash
		step.FunctionSource = ""
	}
	return nil
}

func isWorkflowLLMStep(step workflow.StepDefinition) bool {
	return strings.TrimSpace(step.LLMModelName) != "" || strings.TrimSpace(step.FunctionName) == "__logserve_llm__"
}

func (s *Service) normalizeWorkflowLLMStep(step *workflow.StepDefinition) error {
	step.LLMModelName = strings.TrimSpace(step.LLMModelName)
	if step.LLMModelName == "" {
		return errors.New("llm_model_name is required")
	}
	version := strings.TrimSpace(step.LLMModelVersion)
	if version == "" {
		version = "v1"
	}
	model, ok := s.meta.GetModel(step.LLMModelName, version)
	if !ok {
		return fmt.Errorf("model %s:%s is not registered", step.LLMModelName, version)
	}
	adapter := strings.TrimSpace(step.LLMAdapter)
	if adapter == "" {
		adapter = model.GetAdapter()
	}
	if adapter == "" {
		adapter = "mock"
	}
	step.FunctionName = "__logserve_llm__"
	step.FunctionSource = ""
	step.FunctionRef = ""
	step.FunctionHash = ""
	step.LLMModelVersion = version
	step.LLMAdapter = adapter
	if step.LLMMaxTokens == 0 {
		step.LLMMaxTokens = 64
	}
	return nil
}

func (s *Service) ensureFunctionRegistered(ctx context.Context, entrypoint, source, requestedRef, requestedHash string) (string, string, error) {
	requestedRef = strings.TrimSpace(requestedRef)
	requestedHash = strings.TrimSpace(requestedHash)
	if source == "" && requestedRef == "" && requestedHash == "" {
		return "", "", errors.New("function_source or function_hash is required")
	}

	hash := requestedHash
	if source != "" {
		computed := hashFunctionSource(source)
		if hash != "" && hash != computed {
			return "", "", fmt.Errorf("function_hash %q does not match source hash %q", hash, computed)
		}
		hash = computed
	}
	if hash == "" {
		return "", "", errors.New("function_hash is required when function_source is omitted")
	}

	s.functionsMu.Lock()
	defer s.functionsMu.Unlock()
	if registered, ok := s.functions[hash]; ok {
		return registered.SourceRef, hash, nil
	}

	ref := requestedRef
	if ref == "" {
		if source == "" {
			return "", "", fmt.Errorf("function %s is not registered and source was omitted", hash)
		}
		if s.functionStore == nil {
			return "", "", errors.New("function registry object store is not configured")
		}
		storedRef, err := objectstore.PutBytes(ctx, s.functionStore, "functions", []byte(source))
		if err != nil {
			return "", "", err
		}
		ref = storedRef
	}

	payload := functionRegisteredPayload{
		FunctionHash: hash,
		SourceRef:    ref,
		Language:     "python",
		Entrypoint:   functionEntrypoint(entrypoint),
		TimestampMs:  time.Now().UnixMilli(),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", "", err
	}
	if _, err := s.appendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       functionRegistryStream,
		EventType:      "FunctionRegistered",
		IdempotencyKey: hash + ":registered",
		Payload:        data,
	}); err != nil {
		return "", "", err
	}
	s.functions[hash] = payload
	return ref, hash, nil
}

func (s *Service) bootstrapFunctions(ctx context.Context) error {
	return s.forEachRawLogRecord(ctx, functionRegistryStream, 1, func(rec logrecord.RawRecord) error {
		if rec.EventType != "FunctionRegistered" {
			return nil
		}
		var payload functionRegisteredPayload
		if err := json.Unmarshal(rec.Payload, &payload); err != nil {
			return err
		}
		if payload.FunctionHash == "" || payload.SourceRef == "" {
			return nil
		}
		s.functionsMu.Lock()
		s.functions[payload.FunctionHash] = payload
		s.functionsMu.Unlock()
		return nil
	})
}

func functionEntrypoint(name string) string {
	name = strings.TrimSpace(name)
	if name == "" || strings.Contains(name, ":") {
		return name
	}
	return "module:" + name
}
func requiresPythonFunction(spec *logservepb.TaskSpec) bool {
	return spec.GetLlmModelName() == "" && spec.GetActorId() == ""
}

func hashFunctionSource(source string) string {
	sum := sha256.Sum256([]byte(source))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func taskFunctionIdentity(spec *logservepb.TaskSpec) string {
	if spec == nil {
		return ""
	}
	if spec.GetFunctionHash() != "" {
		return spec.GetFunctionHash()
	}
	if spec.GetFunctionSource() != "" {
		return hashFunctionSource(spec.GetFunctionSource())
	}
	return ""
}
