package objectstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type LocalStore struct {
	dir string
}

func OpenLocal(dir string) (*LocalStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &LocalStore{dir: dir}, nil
}

func (s *LocalStore) Put(ctx context.Context, namespace string, data []byte) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}
	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:]) + ".json"
	rel := filepath.Join(cleanNamespace(namespace), name)
	path := filepath.Join(s.dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return "local://" + filepath.ToSlash(rel), nil
}

func (s *LocalStore) Get(ctx context.Context, ref string) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	const prefix = "local://"
	if !strings.HasPrefix(ref, prefix) {
		return nil, errors.New("unsupported object ref")
	}
	rel := filepath.FromSlash(strings.TrimPrefix(ref, prefix))
	path := filepath.Join(s.dir, rel)
	cleanDir := filepath.Clean(s.dir)
	cleanPath := filepath.Clean(path)
	if cleanPath != cleanDir && !strings.HasPrefix(cleanPath, cleanDir+string(os.PathSeparator)) {
		return nil, errors.New("object ref escapes store")
	}
	return os.ReadFile(cleanPath)
}

func cleanNamespace(namespace string) string {
	namespace = strings.ReplaceAll(namespace, "\\", "/")
	parts := strings.Split(namespace, "/")
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || part == "." || part == ".." {
			continue
		}
		cleaned = append(cleaned, part)
	}
	if len(cleaned) == 0 {
		return "default"
	}
	return filepath.Join(cleaned...)
}
