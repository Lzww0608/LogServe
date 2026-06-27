package webapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"

	"github.com/logserve/logserve/gen/logservepb"
)

const functionRegistryStreamID = "system:functions"

type FunctionDTO struct {
	FunctionHash string `json:"function_hash"`
	SourceRef    string `json:"source_ref"`
	Entrypoint   string `json:"entrypoint"`
	Language     string `json:"language"`
	TimestampMs  int64  `json:"timestamp_ms,omitempty"`
}

type functionRegistrySnapshot struct {
	Functions      []FunctionDTO
	InvalidRecords uint64
}

type functionRegistryCache struct {
	mu             sync.Mutex
	byHash         map[string]FunctionDTO
	nextSeq        uint64
	invalidRecords uint64
}

func newFunctionRegistryCache() *functionRegistryCache {
	return &functionRegistryCache{byHash: make(map[string]FunctionDTO), nextSeq: 1}
}

func (s *Server) handleListFunctions(w http.ResponseWriter, r *http.Request) {
	snapshot, err := s.functionRegistry(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	payload := map[string]any{"functions": snapshot.Functions}
	if snapshot.InvalidRecords > 0 {
		payload["invalid_records"] = snapshot.InvalidRecords
	}
	writeJSON(w, payload)
}

func (s *Server) handleGetFunction(w http.ResponseWriter, r *http.Request) {
	functionHash := r.PathValue("function_hash")
	if functionHash == "" {
		writeErr(w, fmt.Errorf("%w: function_hash is required", errInvalidInput))
		return
	}
	snapshot, err := s.functionRegistry(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	for _, function := range snapshot.Functions {
		if function.FunctionHash == functionHash {
			writeJSON(w, function)
			return
		}
	}
	writeErr(w, fmt.Errorf("function %s not found", functionHash))
}

func (s *Server) functionRegistry(r *http.Request) (functionRegistrySnapshot, error) {
	ctx, cancel := requestContext(r, s.cfg.RequestTimeout)
	defer cancel()
	cache := s.functionRegistryCache
	if cache == nil {
		cache = newFunctionRegistryCache()
		s.functionRegistryCache = cache
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.byHash == nil {
		cache.byHash = make(map[string]FunctionDTO)
	}
	if cache.nextSeq == 0 {
		cache.nextSeq = 1
	}

	fromSeq := cache.nextSeq
	for {
		records, err := s.clients.Log.ReadLog(ctx, &logservepb.ReadLogRequest{
			StreamId: functionRegistryStreamID,
			FromSeq:  fromSeq,
			Limit:    maxLogReadLimit,
		})
		if err != nil {
			return functionRegistrySnapshot{}, err
		}
		batch := records.GetRecords()
		if len(batch) == 0 {
			break
		}
		for _, record := range batch {
			if record.GetEventType() != "FunctionRegistered" {
				continue
			}
			var function FunctionDTO
			if err := json.Unmarshal(record.GetPayload(), &function); err != nil {
				cache.invalidRecords++
				continue
			}
			if function.FunctionHash == "" {
				cache.invalidRecords++
				continue
			}
			if function.TimestampMs == 0 {
				function.TimestampMs = record.GetTimestampMs()
			}
			cache.byHash[function.FunctionHash] = function
		}
		nextSeq := batch[len(batch)-1].GetSeq() + 1
		if nextSeq <= fromSeq {
			break
		}
		fromSeq = nextSeq
		cache.nextSeq = fromSeq
		if len(batch) < int(maxLogReadLimit) {
			break
		}
	}

	functions := make([]FunctionDTO, 0, len(cache.byHash))
	for _, function := range cache.byHash {
		functions = append(functions, function)
	}
	sort.Slice(functions, func(i, j int) bool {
		if functions[i].TimestampMs == functions[j].TimestampMs {
			return functions[i].FunctionHash < functions[j].FunctionHash
		}
		return functions[i].TimestampMs > functions[j].TimestampMs
	})
	return functionRegistrySnapshot{Functions: functions, InvalidRecords: cache.invalidRecords}, nil
}
