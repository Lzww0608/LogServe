// Package observability provides lightweight process diagnostics shared by
// LogServe command entrypoints and internal services.
package observability

// This file implements the structured standard-library logging helper used by
// background loops and command entrypoints.

import (
	"encoding/json"
	"log"
	"time"
)

// Info emits an info-level structured event through the standard logger.
//
// The fields map is optional. When provided, it is enriched in place with
// timestamp, level, and event fields before being encoded as one JSON log line.
// Callers that need to reuse the original map should pass a copy.
func Info(event string, fields map[string]any) {
	write("info", event, fields)
}

// Error emits an error-level structured event through the standard logger.
//
// err is expected to be non-nil because its message is copied into fields. The
// fields map is optional but, when provided, is mutated to include the "error"
// field plus the common log metadata added by write. Passing a nil error would
// panic through err.Error, so call sites should only use Error for real failures.
func Error(event string, err error, fields map[string]any) {
	// Keep nil-field callers cheap while preserving the documented side effect:
	// the map passed by non-nil callers receives the error and common log fields.
	if fields == nil {
		fields = map[string]any{}
	}
	fields["error"] = err.Error()
	write("error", event, fields)
}

// write serializes a structured event as a single JSON log record.
//
// It deliberately uses the standard library logger so binaries can share one
// small logging path without configuring a process-wide logging framework. The
// supplied fields map is mutated in place; callers that need to reuse a map
// without the added metadata should pass a copy.
func write(level, event string, fields map[string]any) {
	if fields == nil {
		// Allocate lazily so the public helpers can accept nil without forcing
		// every caller to construct an empty map for common no-field events.
		fields = map[string]any{}
	}
	// Use UTC/RFC3339Nano so logs from separate processes compare lexically and
	// do not depend on the host timezone.
	fields["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	fields["level"] = level
	fields["event"] = event
	data, err := json.Marshal(fields)
	if err != nil {
		// Fall back to a hand-written JSON-shaped line so encode failures in
		// user-supplied fields do not hide the observability failure itself. The %q
		// formatting keeps the error string valid as a JSON string literal.
		log.Printf(`{"level":"error","event":"structured_log_encode_failed","error":%q}`, err.Error())
		return
	}
	log.Print(string(data))
}
