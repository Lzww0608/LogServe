package objectstore

// This file tests local and S3-compatible object-store behavior without
// requiring an external object-store service.
import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestLocalPutGetBytesAndNoTempLeak verifies local content-addressed idempotency,
// byte round-trips, and temp-file cleanup.
func TestLocalPutGetBytesAndNoTempLeak(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := OpenLocal(dir)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"ok":true}`)
	ref, err := PutBytes(ctx, store, "workflow/results", data)
	if err != nil {
		t.Fatal(err)
	}
	retryRef, err := PutBytes(ctx, store, "workflow/results", data)
	if err != nil {
		t.Fatal(err)
	}
	if retryRef != ref {
		t.Fatalf("retry ref = %q, want %q", retryRef, ref)
	}
	got, err := GetBytes(ctx, store, ref, -1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("GetBytes = %q, want %q", string(got), string(data))
	}
	temps, err := filepath.Glob(filepath.Join(dir, "workflow", "results", "*.tmp.*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("temp files leaked: %v", temps)
	}
}

// TestLocalGetBytesLimitAndEscape verifies read-size enforcement and local ref
// path traversal rejection.
func TestLocalGetBytesLimitAndEscape(t *testing.T) {
	ctx := context.Background()
	store, err := OpenLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := PutBytes(ctx, store, "limits", []byte("abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := GetBytes(ctx, store, ref, 3); err == nil {
		t.Fatal("GetBytes succeeded despite maxBytes limit")
	}
	if _, _, err := store.Get(ctx, "local://../outside.json"); err == nil {
		t.Fatal("Get accepted escaping local ref")
	}
}

// TestS3PutEnsuresBucketOnceAndSendsChecksumMetadata verifies bucket creation is
// guarded by sync.Once and single PUTs include checksum metadata.
func TestS3PutEnsuresBucketOnceAndSendsChecksumMetadata(t *testing.T) {
	ctx := context.Background()
	payload := []byte("def add(a, b):\n    return a + b\n")
	expectedHash := sha256Hex(payload)
	var bucketCreates int32
	var objectPuts int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/logserve-results":
			atomic.AddInt32(&bucketCreates, 1)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/logserve-results/functions/"):
			atomic.AddInt32(&objectPuts, 1)
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read body: %v", err)
			}
			if !bytes.Equal(body, payload) {
				t.Errorf("put body = %q, want %q", string(body), string(payload))
			}
			if got := r.Header.Get("x-amz-meta-sha256"); got != expectedHash {
				t.Errorf("x-amz-meta-sha256 = %q, want %q", got, expectedHash)
			}
			if got := r.Header.Get("x-amz-checksum-sha256"); got == "" {
				t.Error("missing x-amz-checksum-sha256")
			}
			w.Header().Set("ETag", `"object"`)
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/logserve-results/functions/"):
			w.Header().Set("x-amz-meta-sha256", expectedHash)
			w.Header().Set("x-amz-meta-size", fmt.Sprint(len(payload)))
			_, _ = w.Write(payload)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	store, err := OpenS3(ctx, S3Config{
		Endpoint:           server.URL,
		Bucket:             "logserve-results",
		Region:             "us-east-1",
		AccessKey:          "access",
		SecretKey:          "secret",
		CreateBucket:       true,
		MultipartThreshold: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := PutBytes(ctx, store, "functions", payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PutBytes(ctx, store, "functions", payload); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&bucketCreates); got != 1 {
		t.Fatalf("bucket creates = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&objectPuts); got != 2 {
		t.Fatalf("object puts = %d, want 2", got)
	}
	got, err := GetBytes(ctx, store, ref, -1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("s3 GetBytes = %q, want %q", string(got), string(payload))
	}
}

// TestS3MultipartUploadUsesParts verifies payloads above threshold use multipart
// upload, per-part checksums, and final completion XML.
func TestS3MultipartUploadUsesParts(t *testing.T) {
	ctx := context.Background()
	payload := bytes.Repeat([]byte("x"), 6<<20)
	expectedHash := sha256Hex(payload)
	var mu sync.Mutex
	partLengths := make(map[string]int)
	completed := false

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		switch {
		case r.Method == http.MethodPost && query.Has("uploads"):
			if got := r.Header.Get("x-amz-meta-sha256"); got != expectedHash {
				t.Errorf("init metadata hash = %q, want %q", got, expectedHash)
			}
			_, _ = w.Write([]byte(`<InitiateMultipartUploadResult><UploadId>upload-1</UploadId></InitiateMultipartUploadResult>`))
		case r.Method == http.MethodPut && query.Get("uploadId") == "upload-1" && query.Get("partNumber") != "":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read part: %v", err)
			}
			if got := r.Header.Get("x-amz-checksum-sha256"); got == "" {
				t.Error("missing part checksum")
			}
			partNumber := query.Get("partNumber")
			mu.Lock()
			partLengths[partNumber] = len(body)
			mu.Unlock()
			w.Header().Set("ETag", fmt.Sprintf(`"part-%s"`, partNumber))
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && query.Get("uploadId") == "upload-1":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read complete: %v", err)
			}
			if !bytes.Contains(body, []byte("part-1")) || !bytes.Contains(body, []byte("part-2")) {
				t.Errorf("complete body missing parts: %s", string(body))
			}
			mu.Lock()
			completed = true
			mu.Unlock()
			_, _ = w.Write([]byte(`<CompleteMultipartUploadResult/>`))
		case r.Method == http.MethodDelete && query.Get("uploadId") == "upload-1":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPut:
			t.Errorf("unexpected single PUT for multipart payload: %s", r.URL.String())
			http.Error(w, "single put", http.StatusBadRequest)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	store, err := OpenS3(ctx, S3Config{
		Endpoint:           server.URL,
		Bucket:             "logserve-results",
		Region:             "us-east-1",
		AccessKey:          "access",
		SecretKey:          "secret",
		MultipartThreshold: 5 << 20,
		MultipartPartSize:  5 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := PutBytes(ctx, store, "snapshots", payload)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ref, expectedHash) {
		t.Fatalf("ref = %q, want content hash %q", ref, expectedHash)
	}

	mu.Lock()
	defer mu.Unlock()
	if !completed {
		t.Fatal("multipart upload was not completed")
	}
	if partLengths["1"] != 5<<20 || partLengths["2"] != 1<<20 {
		t.Fatalf("part lengths = %+v, want 5MiB and 1MiB", partLengths)
	}
}
