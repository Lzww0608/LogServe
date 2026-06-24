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

type Store interface {
	Put(ctx context.Context, namespace string, r io.Reader, size int64) (string, error)
	Get(ctx context.Context, ref string) (io.ReadCloser, ObjectInfo, error)
}

type ObjectInfo struct {
	Ref            string
	Size           int64
	SHA256         string
	ChecksumSHA256 string
	ETag           string
	Metadata       map[string]string
}

func PutBytes(ctx context.Context, s Store, ns string, data []byte) (string, error) {
	if s == nil {
		return "", errors.New("object store is nil")
	}
	return s.Put(ctx, ns, bytes.NewReader(data), int64(len(data)))
}

func GetBytes(ctx context.Context, s Store, ref string, maxBytes int64) ([]byte, error) {
	if s == nil {
		return nil, errors.New("object store is nil")
	}
	rc, info, err := s.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	if maxBytes >= 0 && info.Size > maxBytes {
		return nil, fmt.Errorf("object %s is %d bytes, exceeds max %d", ref, info.Size, maxBytes)
	}
	if maxBytes < 0 {
		return io.ReadAll(rc)
	}
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

func OpenFromEnv(ctx context.Context) (Store, error) {
	kind := strings.ToLower(strings.TrimSpace(os.Getenv("LOGSERVE_RESULT_STORE")))
	switch kind {
	case "", "local":
		dir := os.Getenv("LOGSERVE_OBJECTSTORE_DIR")
		if dir == "" {
			dir = filepath.Join(os.TempDir(), "logserve-objectstore")
		}
		return OpenLocal(dir)
	case "minio", "s3":
		return OpenS3(ctx, S3ConfigFromEnv())
	default:
		return nil, errors.New("unsupported LOGSERVE_RESULT_STORE: " + kind)
	}
}
