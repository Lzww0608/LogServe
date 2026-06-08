package objectstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type Store interface {
	Put(ctx context.Context, namespace string, data []byte) (string, error)
	Get(ctx context.Context, ref string) ([]byte, error)
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
