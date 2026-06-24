package objectstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var copyBufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 32*1024)
		return &buf
	},
}

type LocalStore struct {
	dir string
}

func OpenLocal(dir string) (*LocalStore, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	return &LocalStore{dir: abs}, nil
}

func (s *LocalStore) Put(ctx context.Context, namespace string, r io.Reader, size int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ns := cleanNamespace(namespace)
	dir := filepath.Join(s.dir, ns)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmpPath := filepath.Join(dir, fmt.Sprintf("object.tmp.%d.%s", os.Getpid(), randomHex(16)))
	tmpFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = os.Remove(tmpPath)
		}
	}()

	h := sha256.New()
	written, err := copyWithContext(ctx, io.MultiWriter(tmpFile, h), r)
	if err != nil {
		_ = tmpFile.Close()
		return "", err
	}
	if size >= 0 && written != size {
		_ = tmpFile.Close()
		return "", fmt.Errorf("object size mismatch: wrote %d bytes, expected %d", written, size)
	}
	if err := errors.Join(tmpFile.Sync(), tmpFile.Close()); err != nil {
		return "", err
	}

	sumHex := hex.EncodeToString(h.Sum(nil))
	rel := filepath.Join(ns, sumHex+".json")
	finalPath := filepath.Join(s.dir, rel)
	ref := "local://" + filepath.ToSlash(rel)

	matches, err := fileSHA256Matches(ctx, finalPath, sumHex)
	if err == nil {
		if matches {
			return ref, nil
		}
		return "", fmt.Errorf("object path %s already exists with different content", finalPath)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	if err := os.Rename(tmpPath, finalPath); err != nil {
		matches, matchErr := fileSHA256Matches(ctx, finalPath, sumHex)
		if matchErr == nil && matches {
			return ref, nil
		}
		return "", err
	}
	cleanupTmp = false
	if err := syncDir(filepath.Dir(finalPath)); err != nil {
		return "", err
	}
	return ref, nil
}

func (s *LocalStore) Get(ctx context.Context, ref string) (io.ReadCloser, ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, ObjectInfo{}, err
	}
	path, rel, err := s.pathForRef(ref)
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	stat, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, ObjectInfo{}, err
	}
	info := ObjectInfo{
		Ref:    ref,
		Size:   stat.Size(),
		SHA256: hashFromObjectName(filepath.Base(rel)),
	}
	return file, info, nil
}

func (s *LocalStore) pathForRef(ref string) (string, string, error) {
	const prefix = "local://"
	if !strings.HasPrefix(ref, prefix) {
		return "", "", errors.New("unsupported object ref")
	}
	rawRel := strings.TrimPrefix(ref, prefix)
	if rawRel == "" {
		return "", "", errors.New("invalid local object ref")
	}
	rel := filepath.Clean(filepath.FromSlash(rawRel))
	if rel == "." || filepath.IsAbs(rel) {
		return "", "", errors.New("invalid local object ref")
	}
	path := filepath.Join(s.dir, rel)
	cleanDir := filepath.Clean(s.dir)
	cleanPath, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	if cleanPath != cleanDir && !strings.HasPrefix(cleanPath, cleanDir+string(os.PathSeparator)) {
		return "", "", errors.New("object ref escapes store")
	}
	return cleanPath, rel, nil
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

func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	ptr := copyBufferPool.Get().(*[]byte)
	buf := *ptr
	defer copyBufferPool.Put(ptr)

	var written int64
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			m, writeErr := dst.Write(buf[:n])
			written += int64(m)
			if writeErr != nil {
				return written, writeErr
			}
			if m != n {
				return written, io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			return written, nil
		}
		if readErr != nil {
			return written, readErr
		}
	}
}
func fileSHA256Matches(ctx context.Context, path, wantHex string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	h := sha256.New()
	if _, err := copyWithContext(ctx, h, file); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == wantHex, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func hashFromObjectName(name string) string {
	return strings.TrimSuffix(name, filepath.Ext(name))
}

func syncDir(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer file.Close()
	_ = file.Sync()
	return nil
}
