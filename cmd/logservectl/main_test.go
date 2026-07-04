package main

import (
	"encoding/json"
	"testing"
)

// TestLLMOutputIncludesCheckpointCacheMetrics verifies the CLI JSON shape
// includes checkpoint-cache fields used by downstream tooling.
func TestLLMOutputIncludesCheckpointCacheMetrics(t *testing.T) {
	data, err := json.Marshal(llmOutput{
		TaskID:             "task-1",
		CheckpointFetchMs:  7,
		CacheUsedBytes:     1024,
		CacheCapacityBytes: 4096,
		EvictionCount:      2,
	})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"checkpoint_fetch_ms", "cache_used_bytes", "cache_capacity_bytes", "eviction_count"} {
		if _, ok := out[key]; !ok {
			t.Fatalf("llm output missing %s: %s", key, data)
		}
	}
}
