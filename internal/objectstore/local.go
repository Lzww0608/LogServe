package objectstore

// This file implements the local filesystem object store. Objects are
// content-addressed by SHA-256 and published with rename after a temp-file write.

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

// copyBufferPool amortizes allocations for context-aware copy loops used by both
// local and S3 stores.
var copyBufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 32*1024)
		return &buf
	},
}

// LocalStore stores immutable objects under a single absolute root directory.
type LocalStore struct {
	// dir is absolute so later ref validation can compare cleaned absolute paths.
	dir string
}

// OpenLocal resolves and creates the local object-store root directory.
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

// Put writes an object into a cleaned namespace, verifies the optional size, and
// produces a local:// content-addressed ref.
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

	// Use O_EXCL with a randomized temp name so concurrent writers never share a
	// partially written temp object.
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
	// Flush and close the temp file before rename so readers never observe a
	// content-addressed path whose file data is still buffered in this process.
	if err := errors.Join(tmpFile.Sync(), tmpFile.Close()); err != nil {
		return "", err
	}

	sumHex := hex.EncodeToString(h.Sum(nil))
	rel := filepath.Join(ns, sumHex+".json")
	finalPath := filepath.Join(s.dir, rel)
	ref := "local://" + filepath.ToSlash(rel)

	// The final path is content-addressed, so an existing matching object makes Put
	// idempotent and avoids rewriting durable data.
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

	// Another concurrent writer may win the rename race; re-read the final object
	// before treating the rename failure as fatal.
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

// Get opens a local:// ref after validating that it stays under the store root.
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

// pathForRef resolves a local:// ref to an absolute path and rejects absolute,
// empty, or parent-escaping paths.
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
	// Compare absolute cleaned paths after joining with the root; this catches
	// refs such as local://namespace/../../outside even when separators differ.
	if cleanPath != cleanDir && !strings.HasPrefix(cleanPath, cleanDir+string(os.PathSeparator)) {
		return "", "", errors.New("object ref escapes store")
	}
	return cleanPath, rel, nil
}

// cleanNamespace converts user-supplied namespaces into safe relative path
// components, dropping empty and traversal elements.
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
		// Keep the empty namespace usable while avoiding writes directly at the
		// object-store root.
		return "default"
	}
	return filepath.Join(cleaned...)
}

// copyWithContext copies with a pooled buffer and checks ctx before each read so
// large object transfers can be canceled.
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

// fileSHA256Matches verifies that an existing content-addressed file still
// matches the hash encoded in its ref.
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
	// Re-hashing is slower than trusting the filename, but it makes rename races
	// safe when another process has already published the content-addressed path.
	return hex.EncodeToString(h.Sum(nil)) == wantHex, nil
}

// randomHex returns a random hex suffix for temp filenames, falling back to time
// if the OS random source is unavailable.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// hashFromObjectName extracts the content hash from a stored object filename.
func hashFromObjectName(name string) string {
	return strings.TrimSuffix(name, filepath.Ext(name))
}

// syncDir opens the containing directory so a successful rename can be flushed on
// filesystems that support directory fsync.
func syncDir(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer file.Close()

	// Directory fsync is best effort because some Windows/filesystem combinations do
	// not support it even after the object file itself was synced.
	_ = file.Sync()
	return nil
}
