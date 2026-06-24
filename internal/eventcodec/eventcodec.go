package eventcodec

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/vmihailenco/msgpack/v5"
)

type Kind byte

const (
	KindTaskSubmitted Kind = 1
	KindWorkflowEvent Kind = 2
	KindActorEvent    Kind = 3
)

var magic = []byte{'L', 'S', 'E', 1}

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

func Body(kind Kind, data []byte) ([]byte, bool, error) {
	if len(data) < len(magic)+1 || !bytes.Equal(data[:len(magic)], magic) {
		return nil, false, nil
	}
	if data[len(magic)] != byte(kind) {
		return nil, true, fmt.Errorf("event payload kind %d does not match %d", data[len(magic)], kind)
	}
	return data[len(magic)+1:], true, nil
}

func StringValue(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	if b, ok := value.([]byte); ok {
		return string(b)
	}
	return ""
}

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

func Uint32Value(value any) uint32 {
	return uint32(Uint64Value(value))
}

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
