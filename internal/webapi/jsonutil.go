package webapi

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

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value)
}

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

func defaultRaw(value json.RawMessage, fallback []byte) json.RawMessage {
	value = bytes.TrimSpace(value)
	if len(value) == 0 {
		return append(json.RawMessage(nil), fallback...)
	}
	return append(json.RawMessage(nil), value...)
}

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
