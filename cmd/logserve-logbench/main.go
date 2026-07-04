// Command logserve-logbench runs a local logstore microbenchmark across fsync
// policies and emits a JSON report for append, read, and recovery timing.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/logserve/logserve/internal/logstore"
)

// report is the top-level JSON document. It records workload parameters once
// and then stores per-policy measurements for easy side-by-side comparison.
type report struct {
	GeneratedAt  string         `json:"generated_at"`
	Records      int            `json:"records"`
	Streams      int            `json:"streams"`
	PayloadBytes int            `json:"payload_bytes"`
	Policies     []policyReport `json:"policies"`
}

// policyReport captures measurements for one fsync policy using an isolated
// temporary logstore directory.
type policyReport struct {
	Policy           string  `json:"policy"`
	DataDir          string  `json:"data_dir"`
	SegmentSizeBytes int64   `json:"segment_size_bytes"`
	SegmentCount     int     `json:"segment_count"`
	AppendDurationMs int64   `json:"append_duration_ms"`
	ReadDurationMs   int64   `json:"read_duration_ms"`
	RecoverMs        int64   `json:"recover_ms"`
	AppendRecordsSec float64 `json:"append_records_sec"`
	ReadRecordsSec   float64 `json:"read_records_sec"`
	ReadRecords      int     `json:"read_records"`
}

// main validates benchmark flags, runs each requested fsync policy, and emits
// the combined report to stdout or the requested JSON file.
func main() {
	records := flag.Int("records", 20000, "records to append per policy")
	streams := flag.Int("streams", 16, "number of streams")
	payloadBytes := flag.Int("payload-bytes", 256, "payload bytes per record")
	segmentSizeBytes := flag.Int64("segment-size-bytes", 1<<20, "segment bytes before rolling")
	policies := flag.String("policies", "always,batch,interval", "comma-separated fsync policies")
	fsyncIntervalMs := flag.Int64("fsync-interval-ms", 100, "fsync interval for interval policy")
	out := flag.String("out", "", "optional JSON output path")
	keepData := flag.Bool("keep-data", false, "keep benchmark data directories")
	flag.Parse()

	if *records <= 0 {
		fatalf("records must be positive")
	}
	if *streams <= 0 {
		fatalf("streams must be positive")
	}
	if *payloadBytes < 0 {
		fatalf("payload-bytes must be non-negative")
	}

	policyNames := splitPolicies(*policies)
	if len(policyNames) == 0 {
		fatalf("at least one fsync policy is required")
	}

	result := report{
		GeneratedAt:  time.Now().Format(time.RFC3339),
		Records:      *records,
		Streams:      *streams,
		PayloadBytes: *payloadBytes,
		Policies:     make([]policyReport, 0, len(policyNames)),
	}

	for _, policy := range policyNames {
		fmt.Fprintf(os.Stderr, "running policy=%s records=%d streams=%d\n", policy, *records, *streams)
		policyResult, err := runPolicy(policy, *records, *streams, *payloadBytes, *segmentSizeBytes, time.Duration(*fsyncIntervalMs)*time.Millisecond, *keepData)
		if err != nil {
			fatalf("policy %s failed: %v", policy, err)
		}
		result.Policies = append(result.Policies, policyResult)
	}

	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fatalf("marshal report: %v", err)
	}
	if *out != "" {
		if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
			fatalf("create output dir: %v", err)
		}
		if err := os.WriteFile(*out, data, 0o644); err != nil {
			fatalf("write report: %v", err)
		}
		fmt.Fprintf(os.Stderr, "wrote %s\n", *out)
		return
	}
	fmt.Println(string(data))
}

// runPolicy measures a single fsync policy by appending records, reading every
// stream back, closing cleanly, and reopening the store to time recovery.
func runPolicy(policy string, records, streams, payloadBytes int, segmentSizeBytes int64, fsyncInterval time.Duration, keepData bool) (policyReport, error) {
	dir, err := os.MkdirTemp("", "logserve-logbench-"+policy+"-")
	if err != nil {
		return policyReport{}, err
	}
	// Each policy gets a fresh directory so segment counts and recovery time are
	// not affected by a previous policy run. --keep-data preserves that directory
	// for manual inspection after the benchmark exits.
	if !keepData {
		defer os.RemoveAll(dir)
	}

	opts := logstore.DefaultOptions()
	opts.SegmentSizeBytes = segmentSizeBytes
	opts.FsyncPolicy = logstore.FsyncPolicy(policy)
	opts.FsyncInterval = fsyncInterval

	store, err := logstore.OpenWithOptions(dir, opts)
	if err != nil {
		return policyReport{}, err
	}

	payload := bytes.Repeat([]byte("x"), payloadBytes)
	appendStart := time.Now()
	for i := 0; i < records; i++ {
		streamID := fmt.Sprintf("bench:%04d", i%streams)
		_, _, err := store.Append(logstore.AppendRequest{
			StreamID:       streamID,
			EventType:      "BenchRecord",
			IdempotencyKey: fmt.Sprintf("record-%d", i),
			Payload:        payload,
		})
		if err != nil {
			_ = store.Close()
			return policyReport{}, err
		}
	}
	appendDuration := time.Since(appendStart)

	// The read pass walks each stream from sequence 1 until the store returns an
	// empty batch, matching the public Read API rather than inspecting segments.
	readStart := time.Now()
	readRecords := 0
	for i := 0; i < streams; i++ {
		streamID := fmt.Sprintf("bench:%04d", i)
		fromSeq := uint64(1)
		for {
			batch, err := store.Read(streamID, fromSeq, 1024)
			if err != nil {
				_ = store.Close()
				return policyReport{}, err
			}
			if len(batch) == 0 {
				break
			}
			readRecords += len(batch)
			fromSeq = batch[len(batch)-1].Seq + 1
		}
	}
	readDuration := time.Since(readStart)

	if err := store.Close(); err != nil {
		return policyReport{}, err
	}

	// Recovery is approximated by reopening after a clean close, which exercises
	// segment discovery and metadata reconstruction for the generated workload.
	recoverStart := time.Now()
	recovered, err := logstore.OpenWithOptions(dir, opts)
	if err != nil {
		return policyReport{}, err
	}
	recoverDuration := time.Since(recoverStart)
	if err := recovered.Close(); err != nil {
		return policyReport{}, err
	}

	segmentCount, err := countSegments(dir)
	if err != nil {
		return policyReport{}, err
	}

	return policyReport{
		Policy:           policy,
		DataDir:          dir,
		SegmentSizeBytes: segmentSizeBytes,
		SegmentCount:     segmentCount,
		AppendDurationMs: appendDuration.Milliseconds(),
		ReadDurationMs:   readDuration.Milliseconds(),
		RecoverMs:        recoverDuration.Milliseconds(),
		AppendRecordsSec: rate(records, appendDuration),
		ReadRecordsSec:   rate(readRecords, readDuration),
		ReadRecords:      readRecords,
	}, nil
}

// splitPolicies accepts human-friendly comma-separated input and drops empty
// entries so trailing commas do not create invalid policy names.
func splitPolicies(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// countSegments reports how many rolled segment log files were produced for
// a benchmark run.
func countSegments(dir string) (int, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "segment-*.log"))
	if err != nil {
		return 0, err
	}
	return len(paths), nil
}

// rate avoids division by zero for extremely small or failed timing windows.
func rate(records int, duration time.Duration) float64 {
	if duration <= 0 {
		return 0
	}
	return float64(records) / duration.Seconds()
}

// fatalf prints a benchmark error to stderr and exits with a non-zero status.
func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
