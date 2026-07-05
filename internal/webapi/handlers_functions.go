package webapi

// This file exposes the function registry stream through list/detail HTTP
// endpoints and maintains a small tailing cache for system:functions records.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"

	"github.com/logserve/logserve/gen/logservepb"
)

// functionRegistryStreamID is the append-only log stream for function metadata.
const functionRegistryStreamID = "system:functions"

// FunctionDTO describes a registered function source as discovered from the
// system:functions log stream.
type FunctionDTO struct {
	FunctionHash string `json:"function_hash"`
	SourceRef    string `json:"source_ref"`
	Entrypoint   string `json:"entrypoint"`
	Language     string `json:"language"`
	TimestampMs  int64  `json:"timestamp_ms,omitempty"`
}

// functionRegistrySnapshot holds the sorted function list plus malformed record
// count for diagnostics.
type functionRegistrySnapshot struct {
	Functions      []FunctionDTO
	InvalidRecords uint64
}

// functionRegistryCache tails the function registry stream incrementally and
// stores the latest record for each function hash.
type functionRegistryCache struct {
	// mu protects all cache fields while a request tails the registry stream.
	mu sync.Mutex
	// byHash stores the latest valid registry event for each function hash.
	byHash map[string]FunctionDTO
	// nextSeq is the next unread system:functions sequence for incremental tailing.
	nextSeq uint64
	// invalidRecords counts malformed registry records skipped since process start.
	invalidRecords uint64
}

// newFunctionRegistryCache initializes stream tailing from sequence 1.
func newFunctionRegistryCache() *functionRegistryCache {
	return &functionRegistryCache{byHash: make(map[string]FunctionDTO), nextSeq: 1}
}

// handleListFunctions returns cached function registry entries and includes an
// invalid-record counter when malformed log events were skipped.
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

// handleGetFunction returns one function registry entry by hash.
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

// functionRegistry tails system:functions from the cached next sequence, ignores
// malformed FunctionRegistered records, and returns entries sorted newest first.
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

	// Hold the cache lock across tail reads so concurrent requests cannot race the
	// nextSeq cursor and duplicate work or skip records.
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
			// system:functions may grow additional event types; only registration events
			// update the cache, while malformed registrations are counted below.
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
		// Defensive stop: a non-increasing sequence would otherwise make the tail loop
		// repeat forever on corrupt or unusual log-service output.
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
