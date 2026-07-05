// Package objectstore persists large immutable runtime payloads and returns
// compact refs that can be stored in metadata or replay logs.
package objectstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Store persists immutable objects and returns backend-specific refs that can be
// stored in metadata or log records. Implementations should honor ctx while
// copying data and reject unsupported ref schemes on Get.
type Store interface {
	// Put writes the reader into namespace and returns a backend-specific ref.
	// size >= 0 is treated as an expected byte count; size < 0 means unknown.
	Put(ctx context.Context, namespace string, r io.Reader, size int64) (string, error)
	// Get opens a ref produced by a compatible backend and returns a stream plus
	// best-effort metadata. Callers own and must close the returned reader.
	Get(ctx context.Context, ref string) (io.ReadCloser, ObjectInfo, error)
}

// ObjectInfo describes an opened object and carries backend checksum metadata
// when available.
type ObjectInfo struct {
	// Ref is the original object ref used to open the object.
	Ref string
	// Size is the object byte length when known; S3 may report -1 for streaming bodies.
	Size int64
	// SHA256 is the whole-object content hash when the backend can recover it.
	SHA256 string
	// ChecksumSHA256 is the backend checksum header value when present.
	ChecksumSHA256 string
	// ETag is the backend entity tag for systems that expose one.
	ETag string
	// Metadata carries normalized backend user metadata.
	Metadata map[string]string
}

// PutBytes stores an in-memory byte slice through a Store and returns its ref.
func PutBytes(ctx context.Context, s Store, ns string, data []byte) (string, error) {
	if s == nil {
		return "", errors.New("object store is nil")
	}
	return s.Put(ctx, ns, bytes.NewReader(data), int64(len(data)))
}

// GetBytes reads an object into memory. maxBytes >= 0 enforces a hard size limit
// before and during the read; maxBytes < 0 disables the limit.
func GetBytes(ctx context.Context, s Store, ref string, maxBytes int64) ([]byte, error) {
	if s == nil {
		return nil, errors.New("object store is nil")
	}
	rc, info, err := s.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	// Known size metadata can fail fast, but the stream is still limited below so
	// backends with unknown or stale sizes cannot bypass maxBytes.
	if maxBytes >= 0 && info.Size > maxBytes {
		return nil, fmt.Errorf("object %s is %d bytes, exceeds max %d", ref, info.Size, maxBytes)
	}
	if maxBytes < 0 {
		return io.ReadAll(rc)
	}

	// Read one byte past the limit so stores with unknown or stale size metadata are
	// still bounded and rejected if the stream is too large.
	limited := io.LimitReader(rc, maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("object %s exceeds max %d bytes", ref, maxBytes)
	}
	return data, nil
}

// OpenFromEnv selects the configured object-store backend from environment
// variables, defaulting to a local temp directory.
func OpenFromEnv(ctx context.Context) (Store, error) {
	kind := strings.ToLower(strings.TrimSpace(os.Getenv("LOGSERVE_RESULT_STORE")))
	switch kind {
	case "", "local":
		dir := os.Getenv("LOGSERVE_OBJECTSTORE_DIR")
		if dir == "" {
			// The default is intentionally process-external temp storage so dev
			// services can restart and still resolve refs written earlier.
			dir = filepath.Join(os.TempDir(), "logserve-objectstore")
		}
		return OpenLocal(dir)
	case "minio", "s3":
		return OpenS3(ctx, S3ConfigFromEnv())
	default:
		return nil, errors.New("unsupported LOGSERVE_RESULT_STORE: " + kind)
	}
}
