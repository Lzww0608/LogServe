package webapi

// This file contains shared JSON decoding, JSON response, request timeout, and
// raw-JSON helper code for handlers.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"google.golang.org/grpc/metadata"
)

var errInvalidInput = errors.New("invalid input")

const (
	maxSourceBytes = 256 * 1024
	maxJSONBytes   = 64 * 1024
)

// decodeJSON reads one bounded JSON value, rejects unknown fields, and rejects
// trailing values so request bodies have a single unambiguous shape.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, 2*maxSourceBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: %v", errInvalidInput, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("%w: request body must contain exactly one JSON value", errInvalidInput)
	}
	return nil
}

// writeJSON serializes a JSON response with HTML escaping disabled so RawMessage
// fields remain readable to the console.
func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value)
}

// requestContext derives a bounded backend RPC context from the HTTP request and
// forwards the request ID as outgoing gRPC metadata.
func requestContext(r *http.Request, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx := r.Context()
	if id := requestID(r); id != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-request-id", id)
	}
	return context.WithTimeout(ctx, timeout)
}

// defaultRaw returns a copied raw JSON value or a copied fallback when the input
// is empty or whitespace.
func defaultRaw(value json.RawMessage, fallback []byte) json.RawMessage {
	value = bytes.TrimSpace(value)
	if len(value) == 0 {
		return append(json.RawMessage(nil), fallback...)
	}
	return append(json.RawMessage(nil), value...)
}

// validateRawJSON checks optional raw JSON fields for size and syntax without
// normalizing their semantic content.
func validateRawJSON(name string, value json.RawMessage, maxBytes int) error {
	value = bytes.TrimSpace(value)
	if len(value) == 0 {
		return nil
	}
	if len(value) > maxBytes {
		return fmt.Errorf("%w: %s exceeds %d bytes", errInvalidInput, name, maxBytes)
	}
	if !json.Valid(value) {
		return fmt.Errorf("%w: %s must be valid JSON", errInvalidInput, name)
	}
	return nil
}

// envelopeArgs builds the control-plane {args, kwargs} envelope used by task and
// actor calls, defaulting omitted fields to [] and {}.
func envelopeArgs(args, kwargs json.RawMessage) ([]byte, error) {
	args = defaultRaw(args, []byte("[]"))
	kwargs = defaultRaw(kwargs, []byte("{}"))
	if err := validateRawJSON("args", args, maxJSONBytes); err != nil {
		return nil, err
	}
	if err := validateRawJSON("kwargs", kwargs, maxJSONBytes); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]json.RawMessage{
		"args":   args,
		"kwargs": kwargs,
	})
}

// jsonOrNil returns valid JSON as-is, encodes non-JSON bytes as a JSON string,
// and leaves empty payloads omitted.
func jsonOrNil(data []byte) json.RawMessage {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil
	}
	if json.Valid(data) {
		return append(json.RawMessage(nil), data...)
	}
	encoded, _ := json.Marshal(string(data))
	return encoded
}
