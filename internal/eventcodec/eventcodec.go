// Package eventcodec defines the compact event payload envelope shared by task,
// workflow, and actor lifecycle replay code. New records use a small msgpack
// envelope, while callers can still detect legacy JSON payloads and choose a
// fallback decoder outside this package.
package eventcodec

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/vmihailenco/msgpack/v5"
)

// Kind identifies the logical payload family inside the eventcodec envelope.
// The kind byte prevents one replay path from accidentally decoding another
// path's msgpack map.
type Kind byte

// Known kind values keep payload families isolated; each byte selects the only
// replay path that should decode the msgpack body.
const (
	KindTaskSubmitted Kind = 1 // Task submission payload stored in the shared log.
	KindWorkflowEvent Kind = 2 // Workflow replay record stored in the shared log.
	KindActorEvent    Kind = 3 // Actor lifecycle or command replay record stored in the shared log.
)

// magic marks the versioned LogServe event envelope. Payloads without this
// prefix are treated as legacy records by callers.
var magic = []byte{'L', 'S', 'E', 1}

// Marshal encodes value as msgpack and prefixes it with the eventcodec magic
// plus the non-zero payload kind. It returns the full log payload and fails for
// a zero kind or for msgpack encoding errors; it performs no I/O by itself.
func Marshal(kind Kind, value any) ([]byte, error) {
	if kind == 0 {
		return nil, errors.New("event kind is required")
	}
	body, err := msgpack.Marshal(value)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(magic)+1+len(body))
	copy(out, magic)
	out[len(magic)] = byte(kind)
	copy(out[len(magic)+1:], body)
	return out, nil
}

// Unmarshal decodes a matching eventcodec payload into value. The returned bool
// reports whether data used this envelope; false with nil error is the explicit
// signal for callers to try their legacy JSON decoder. A kind mismatch or
// msgpack decode failure is reported as an error because the payload already
// claimed to be an eventcodec record.
func Unmarshal(kind Kind, data []byte, value any) (bool, error) {
	body, ok, err := Body(kind, data)
	if err != nil || !ok {
		return ok, err
	}
	if err := msgpack.Unmarshal(body, value); err != nil {
		return true, err
	}
	return true, nil
}

// Body strips the eventcodec envelope without decoding the msgpack body. It
// returns ok=false for data without the magic prefix, and returns an error when
// the envelope is present but the kind byte does not match the expected kind.
// Callers use the returned body when they need to defer or customize msgpack
// decoding.
func Body(kind Kind, data []byte) ([]byte, bool, error) {
	if len(data) < len(magic)+1 || !bytes.Equal(data[:len(magic)], magic) {
		// Missing magic is a compatibility signal, not corrupt data.
		return nil, false, nil
	}
	if data[len(magic)] != byte(kind) {
		return nil, true, fmt.Errorf("event payload kind %d does not match %d", data[len(magic)], kind)
	}
	return data[len(magic)+1:], true, nil
}

// StringValue recovers a string field from a msgpack-decoded map, accepting both
// string and []byte representations. Unknown or nil values collapse to the empty
// string so replay code can tolerate absent optional fields.
func StringValue(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	if b, ok := value.([]byte); ok {
		return string(b)
	}
	return ""
}

// BytesValue recovers a byte field from a msgpack-decoded map, accepting both
// []byte and string representations. Unsupported values return nil instead of an
// empty slice so callers can distinguish "not present" from a present empty
// payload when they need to.
func BytesValue(value any) []byte {
	switch v := value.(type) {
	case nil:
		return nil
	case []byte:
		return v
	case string:
		return []byte(v)
	default:
		return nil
	}
}

// Uint64Value recovers an unsigned integer field from a msgpack-decoded map.
// Signed inputs must be positive so negative values cannot wrap into large
// uint64 values. Unsupported or non-positive signed values return zero, matching
// the package's permissive recovery helpers.
func Uint64Value(value any) uint64 {
	switch v := value.(type) {
	case uint64:
		return v
	case uint32:
		return uint64(v)
	case uint16:
		return uint64(v)
	case uint8:
		return uint64(v)
	case uint:
		return uint64(v)
	case int64:
		// Only positive signed values are accepted for unsigned fields.
		if v > 0 {
			return uint64(v)
		}
	case int32:
		if v > 0 {
			return uint64(v)
		}
	case int16:
		if v > 0 {
			return uint64(v)
		}
	case int8:
		if v > 0 {
			return uint64(v)
		}
	case int:
		if v > 0 {
			return uint64(v)
		}
	}
	return 0
}

// Uint32Value recovers a uint32 field through the wider unsigned conversion path.
// It intentionally inherits Uint64Value's permissive type handling before the
// final narrowing cast.
func Uint32Value(value any) uint32 {
	return uint32(Uint64Value(value))
}

// Int64Value recovers a signed integer field from a msgpack-decoded map and
// rejects uint64 values that cannot fit into int64. Unsupported values return
// zero instead of failing because these helpers are used after map-based msgpack
// decoding where individual field types can vary across old records.
func Int64Value(value any) int64 {
	switch v := value.(type) {
	case int64:
		return v
	case int32:
		return int64(v)
	case int16:
		return int64(v)
	case int8:
		return int64(v)
	case int:
		return int64(v)
	case uint64:
		// Reject values above MaxInt64 instead of overflowing during conversion.
		if v <= uint64(^uint64(0)>>1) {
			return int64(v)
		}
	case uint32:
		return int64(v)
	case uint16:
		return int64(v)
	case uint8:
		return int64(v)
	case uint:
		return int64(v)
	}
	return 0
}
