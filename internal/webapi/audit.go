package webapi

// This file records request IDs, HTTP statuses, and best-effort audit
// events for frontend API operations.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/logserve/logserve/gen/logservepb"
)

const auditStreamID = "system:audit"

var fallbackRequestIDCounter uint64

// requestIDContextKey stores the normalized request ID on API request contexts.
type requestIDContextKey struct{}

// statusRecorder captures the first HTTP status written by a handler for audit
// reporting.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

// flushingStatusRecorder restores http.Flusher on top of statusRecorder for SSE
// and other streaming responses.
type flushingStatusRecorder struct {
	*statusRecorder
}

// WriteHeader records the first status code before forwarding it to the wrapped
// ResponseWriter.
func (r *statusRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

// Write records an implicit 200 status when a handler writes a body without an
// explicit header.
func (r *statusRecorder) Write(data []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(data)
}

// Flush forwards streaming flushes through the recorder wrapper when supported.
func (r *flushingStatusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// auditEvent is the JSON payload appended to the system:audit log stream after
// frontend API operations complete.
type auditEvent struct {
	Subject    string `json:"subject"`
	Role       string `json:"role"`
	Action     string `json:"action"`
	Target     string `json:"target"`
	RequestID  string `json:"request_id"`
	Timestamp  int64  `json:"timestamp_ms"`
	Result     string `json:"result"`
	StatusCode int    `json:"status_code"`
	DurationMs int64  `json:"duration_ms"`
	Method     string `json:"method"`
	Path       string `json:"path"`
}

// auditFrontendOperation appends a best-effort audit record to the log service.
// Audit failures are intentionally ignored so user-facing operations do not fail
// because the audit append path is unavailable.
func (s *Server) auditFrontendOperation(r *http.Request, principal authPrincipal, action string, statusCode int, started time.Time) {
	if action == "" || s.clients == nil || s.clients.Log == nil {
		return
	}
	requestID := requestID(r)
	payload, err := json.Marshal(auditEvent{
		Subject:    principal.Subject,
		Role:       string(principal.Role),
		Action:     action,
		Target:     r.URL.Path,
		RequestID:  requestID,
		Timestamp:  time.Now().UnixMilli(),
		Result:     auditResult(statusCode),
		StatusCode: statusCode,
		DurationMs: time.Since(started).Milliseconds(),
		Method:     r.Method,
		Path:       r.URL.Path,
	})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = s.clients.Log.AppendLog(ctx, &logservepb.AppendLogRequest{
		StreamId:       auditStreamID,
		EventType:      "FrontendOperationCompleted",
		IdempotencyKey: auditIdempotencyKey(requestID, action, r),
		Payload:        payload,
	})
}

// ensureRequestID returns a request carrying the stable request ID used for
// response headers, outgoing gRPC metadata, and audit records.
func ensureRequestID(r *http.Request) (*http.Request, string) {
	id := requestID(r)
	return r.WithContext(context.WithValue(r.Context(), requestIDContextKey{}, id)), id
}

// requestID reads the context value first, then X-Request-ID, and finally creates
// a local fallback ID.
func requestID(r *http.Request) string {
	if value, ok := r.Context().Value(requestIDContextKey{}).(string); ok && value != "" {
		return value
	}
	if value := r.Header.Get("X-Request-ID"); value != "" {
		return value
	}
	return newRequestID()
}

// newRequestID creates a random request ID and falls back to timestamp+counter
// only if the OS random source fails.
func newRequestID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err == nil {
		return "req-" + hex.EncodeToString(buf[:])
	}
	return fmt.Sprintf("req-%d-%d", time.Now().UnixNano(), atomic.AddUint64(&fallbackRequestIDCounter, 1))
}

// auditIdempotencyKey makes repeated audit writes for the same frontend request
// collapse in the log store idempotency layer.
func auditIdempotencyKey(requestID, action string, r *http.Request) string {
	return fmt.Sprintf("%s:%s:%s:%s", requestID, r.Method, action, r.URL.EscapedPath())
}

// auditResult maps HTTP status codes into coarse audit outcomes.
func auditResult(statusCode int) string {
	switch {
	case statusCode >= 200 && statusCode < 300:
		return "ok"
	case statusCode == http.StatusForbidden:
		return "denied"
	case statusCode >= 400 && statusCode < 500:
		return "rejected"
	case statusCode >= 500:
		return "error"
	default:
		return "unknown"
	}
}
