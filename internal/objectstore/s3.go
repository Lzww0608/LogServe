package objectstore

// This file implements a minimal S3-compatible object store using raw HTTP
// requests and AWS Signature Version 4. It is used for S3 and MinIO deployments.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// S3 multipart defaults balance memory use, object size support, and the S3
// minimum part-size rule.
const (
	// defaultS3MultipartThreshold keeps small objects on the simpler single-PUT path.
	defaultS3MultipartThreshold = 64 << 20
	// defaultS3MultipartPartSize is above S3's minimum and bounds per-part memory.
	defaultS3MultipartPartSize = 16 << 20
	// minS3MultipartPartSize is the S3 multipart minimum except for the final part.
	minS3MultipartPartSize = 5 << 20
	// emptyPayloadSHA256 is the canonical SigV4 hash for requests with no body.
	emptyPayloadSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// S3Config contains endpoint, credential, bucket, and multipart settings for the
// S3-compatible backend.
type S3Config struct {
	// Endpoint is the scheme and host of the S3-compatible service.
	Endpoint string
	// Bucket is the only bucket this store accepts refs for.
	Bucket string
	// Region participates in SigV4 credential scope.
	Region string
	// AccessKey is the SigV4 access key ID.
	AccessKey string
	// SecretKey is the SigV4 signing secret.
	SecretKey string
	// CreateBucket controls the one-time best-effort bucket creation before Put.
	CreateBucket bool
	// MultipartThreshold chooses single PUT below the threshold and multipart at or above it.
	MultipartThreshold int64
	// MultipartPartSize is clamped to S3's minimum during OpenS3.
	MultipartPartSize int64
}

// S3Store writes immutable content-addressed objects to one configured bucket.
type S3Store struct {
	// cfg is normalized by OpenS3 and then treated as immutable.
	cfg S3Config
	// client owns transport pooling for all signed requests.
	client *http.Client
	// ensure serializes optional bucket creation across concurrent Put calls.
	ensure sync.Once
	// ensureErr records the memoized bucket-create outcome.
	ensureErr error
}

// S3ConfigFromEnv builds S3Config from LOGSERVE_* variables with MINIO_* aliases
// for local MinIO setups.
func S3ConfigFromEnv() S3Config {
	// Default to creating the bucket for local MinIO/dev deployments; production
	// deployments can opt out with LOGSERVE_S3_CREATE_BUCKET=0.
	createBucket := os.Getenv("LOGSERVE_S3_CREATE_BUCKET") != "0"
	return S3Config{
		Endpoint:           firstNonEmpty(os.Getenv("LOGSERVE_S3_ENDPOINT"), os.Getenv("MINIO_ENDPOINT")),
		Bucket:             firstNonEmpty(os.Getenv("LOGSERVE_S3_BUCKET"), "logserve-results"),
		Region:             firstNonEmpty(os.Getenv("LOGSERVE_S3_REGION"), "us-east-1"),
		AccessKey:          firstNonEmpty(os.Getenv("LOGSERVE_S3_ACCESS_KEY"), os.Getenv("MINIO_ROOT_USER")),
		SecretKey:          firstNonEmpty(os.Getenv("LOGSERVE_S3_SECRET_KEY"), os.Getenv("MINIO_ROOT_PASSWORD")),
		CreateBucket:       createBucket,
		MultipartThreshold: envInt64("LOGSERVE_S3_MULTIPART_THRESHOLD", defaultS3MultipartThreshold),
		MultipartPartSize:  envInt64("LOGSERVE_S3_MULTIPART_PART_SIZE", defaultS3MultipartPartSize),
	}
}

// OpenS3 validates configuration, fills defaults, and constructs an HTTP-backed
// store. It does not contact the endpoint until the first operation.
func OpenS3(ctx context.Context, cfg S3Config) (*S3Store, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("LOGSERVE_S3_ENDPOINT is required for s3 result store")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("LOGSERVE_S3_BUCKET is required for s3 result store")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, errors.New("LOGSERVE_S3_ACCESS_KEY and LOGSERVE_S3_SECRET_KEY are required for s3 result store")
	}
	if cfg.MultipartThreshold <= 0 {
		cfg.MultipartThreshold = defaultS3MultipartThreshold
	}
	if cfg.MultipartPartSize <= 0 {
		cfg.MultipartPartSize = defaultS3MultipartPartSize
	}
	if cfg.MultipartPartSize < minS3MultipartPartSize {
		// S3 rejects non-final parts below 5 MiB, so silently clamp instead of
		// accepting a config that would fail only after upload initiation.
		cfg.MultipartPartSize = minS3MultipartPartSize
	}
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	store := &S3Store{cfg: cfg, client: &http.Client{Transport: transport}}

	// Keep ctx in the signature for parity with other open helpers even though this
	// constructor performs no network I/O.
	_ = ctx
	return store, nil
}

// Put spools the stream to a temp file to compute checksum and size before
// issuing either a single PUT or a multipart upload.
func (s *S3Store) Put(ctx context.Context, namespace string, r io.Reader, size int64) (string, error) {
	if err := s.ensureBucket(ctx); err != nil {
		return "", err
	}

	// S3 signing and checksum headers require the payload hash up front, so streaming
	// uploads are first spooled to local temp storage.
	file, tmpPath, actualSize, hashHex, hashBase64, err := spoolToTemp(ctx, r, size)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = file.Close()
		_ = os.Remove(tmpPath)
	}()

	// Keys mirror the local backend: cleaned namespace plus whole-object hash, so
	// refs remain content-addressed even when S3 overwrites the same key.
	key := path.Join(filepathSlash(cleanNamespace(namespace)), hashHex+".json")
	if actualSize >= s.cfg.MultipartThreshold {
		err = s.putMultipart(ctx, key, file, actualSize, hashHex)
	} else {
		err = s.putSingle(ctx, key, file, actualSize, hashHex, hashBase64)
	}
	if err != nil {
		return "", err
	}
	return "s3://" + s.cfg.Bucket + "/" + key, nil
}

// Get opens an s3://bucket/key ref from the configured bucket and returns the
// response body for the caller to close.
func (s *S3Store) Get(ctx context.Context, ref string) (io.ReadCloser, ObjectInfo, error) {
	key, err := s.keyFromRef(ref)
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	resp, err := s.openRequest(ctx, http.MethodGet, s.objectURL(key), nil, 0, emptyPayloadSHA256, nil)
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	info := objectInfoFromS3Headers(ref, resp.Header, resp.ContentLength)
	return resp.Body, info, nil
}

// ensureBucket creates the bucket at most once when enabled. A preexisting bucket
// is treated as success for idempotent startup.
func (s *S3Store) ensureBucket(ctx context.Context) error {
	if !s.cfg.CreateBucket {
		return nil
	}

	// sync.Once intentionally memoizes the first bucket-create result so concurrent
	// Put calls do not stampede the object-store endpoint.
	s.ensure.Do(func() {
		err := s.doBucket(ctx, http.MethodPut, nil, 0, emptyPayloadSHA256, nil)
		var statusErr *s3StatusError
		if errors.As(err, &statusErr) && (statusErr.statusCode == http.StatusConflict || statusErr.statusCode == http.StatusOK) {
			err = nil
		}
		s.ensureErr = err
	})
	return s.ensureErr
}

// putSingle rewinds the spooled file and uploads it with metadata plus the S3
// checksum header for non-multipart objects.
func (s *S3Store) putSingle(ctx context.Context, key string, file *os.File, size int64, hashHex, hashBase64 string) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	headers := checksumMetadataHeaders(hashHex, size)
	headers.Set("x-amz-checksum-sha256", hashBase64)
	return s.do(ctx, http.MethodPut, key, file, size, hashHex, headers, nil)
}

// putMultipart runs the initiate/upload/complete sequence and aborts unfinished
// uploads on any error path.
func (s *S3Store) putMultipart(ctx context.Context, key string, file *os.File, size int64, hashHex string) error {
	uploadID, err := s.createMultipartUpload(ctx, key, hashHex, size)
	if err != nil {
		return err
	}
	completed := false
	defer func() {
		if !completed {
			// Abort with a fresh background context because the caller context may already
			// be canceled when cleanup is needed.
			_ = s.abortMultipartUpload(context.Background(), key, uploadID)
		}
	}()
	parts, err := s.uploadParts(ctx, key, uploadID, file, size)
	if err != nil {
		return err
	}
	if err := s.completeMultipartUpload(ctx, key, uploadID, parts); err != nil {
		return err
	}
	completed = true
	return nil
}

// createMultipartUpload starts an S3 multipart upload and attaches whole-object
// checksum metadata to the object.
func (s *S3Store) createMultipartUpload(ctx context.Context, key, hashHex string, size int64) (string, error) {
	var result initiateMultipartUploadResult
	headers := checksumMetadataHeaders(hashHex, size)
	err := s.doRequest(ctx, http.MethodPost, s.objectURLWithQuery(key, url.Values{"uploads": {""}}), nil, 0, emptyPayloadSHA256, headers, func(resp *http.Response) error {
		if err := xml.NewDecoder(resp.Body).Decode(&result); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if result.UploadID == "" {
		return "", errors.New("s3 multipart initiate response missing upload id")
	}
	return result.UploadID, nil
}

// uploadParts uploads contiguous file sections serially and returns the ETags
// required by CompleteMultipartUpload.
func (s *S3Store) uploadParts(ctx context.Context, key, uploadID string, file *os.File, size int64) ([]completedPart, error) {
	parts := make([]completedPart, 0, int((size+s.cfg.MultipartPartSize-1)/s.cfg.MultipartPartSize))
	for offset, partNumber := int64(0), 1; offset < size; partNumber++ {
		partSize := s.cfg.MultipartPartSize
		if remaining := size - offset; remaining < partSize {
			partSize = remaining
		}

		// SectionReader limits each request body to the current part without copying the
		// spooled file into memory.
		section := io.NewSectionReader(file, offset, partSize)
		h := sha256.New()
		if _, err := copyWithContext(ctx, h, section); err != nil {
			return nil, err
		}

		// The same section is read once for its checksum and then rewound for upload.
		if _, err := section.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		sum := h.Sum(nil)
		headers := http.Header{}
		headers.Set("x-amz-checksum-sha256", base64.StdEncoding.EncodeToString(sum))
		query := url.Values{
			"partNumber": {strconv.Itoa(partNumber)},
			"uploadId":   {uploadID},
		}
		var etag string
		if err := s.doRequest(ctx, http.MethodPut, s.objectURLWithQuery(key, query), section, partSize, hex.EncodeToString(sum), headers, func(resp *http.Response) error {
			etag = resp.Header.Get("ETag")
			return nil
		}); err != nil {
			return nil, err
		}
		if etag == "" {
			return nil, fmt.Errorf("s3 upload part %d response missing ETag", partNumber)
		}
		parts = append(parts, completedPart{PartNumber: partNumber, ETag: etag})
		offset += partSize
	}
	return parts, nil
}

// completeMultipartUpload sends the part list and detects S3-compatible servers
// that return HTTP 200 with an embedded XML error body.
func (s *S3Store) completeMultipartUpload(ctx context.Context, key, uploadID string, parts []completedPart) error {
	body, err := xml.Marshal(completeMultipartUploadRequest{Parts: parts})
	if err != nil {
		return err
	}
	payloadHash := sha256Hex(body)
	headers := http.Header{}
	headers.Set("Content-Type", "application/xml")
	query := url.Values{"uploadId": {uploadID}}
	return s.doRequest(ctx, http.MethodPost, s.objectURLWithQuery(key, query), bytes.NewReader(body), int64(len(body)), payloadHash, headers, func(resp *http.Response) error {
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}

		// Some S3 APIs report complete-multipart failures inside a 200 response body, so
		// the XML body must be inspected even after HTTP success.
		if bytes.Contains(data, []byte("<Error>")) {
			var s3Err s3ErrorResult
			if err := xml.Unmarshal(data, &s3Err); err != nil {
				return fmt.Errorf("s3 complete multipart failed: %s", strings.TrimSpace(string(data)))
			}
			return fmt.Errorf("s3 complete multipart failed: %s: %s", s3Err.Code, s3Err.Message)
		}
		return nil
	})
}

// abortMultipartUpload cancels an unfinished multipart upload to avoid orphaned
// parts.
func (s *S3Store) abortMultipartUpload(ctx context.Context, key, uploadID string) error {
	query := url.Values{"uploadId": {uploadID}}
	return s.doRequest(ctx, http.MethodDelete, s.objectURLWithQuery(key, query), nil, 0, emptyPayloadSHA256, nil, nil)
}

// do issues an object-scoped signed request for a bucket-relative key.
func (s *S3Store) do(ctx context.Context, method, key string, body io.Reader, size int64, payloadHash string, headers http.Header, handle func(*http.Response) error) error {
	return s.doRequest(ctx, method, s.objectURL(key), body, size, payloadHash, headers, handle)
}

// doBucket issues a signed bucket-level request.
func (s *S3Store) doBucket(ctx context.Context, method string, body io.Reader, size int64, payloadHash string, headers http.Header) error {
	return s.doRequest(ctx, method, s.bucketURL(), body, size, payloadHash, headers, nil)
}

// doRequest opens a signed HTTP request, closes the response body, and lets
// callers parse success responses when needed.
func (s *S3Store) doRequest(ctx context.Context, method, rawURL string, body io.Reader, size int64, payloadHash string, headers http.Header, handle func(*http.Response) error) error {
	resp, err := s.openRequest(ctx, method, rawURL, body, size, payloadHash, headers)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if handle != nil {
		return handle(resp)
	}
	return nil
}

// openRequest sends a signed request and converts non-2xx responses into a
// bounded s3StatusError.
func (s *S3Store) openRequest(ctx context.Context, method, rawURL string, body io.Reader, size int64, payloadHash string, headers http.Header) (*http.Response, error) {
	req, err := s.newRequest(ctx, method, rawURL, body, size, payloadHash, headers)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()

		// Limit error-body reads so a failing object-store response cannot consume
		// unbounded memory.
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &s3StatusError{statusCode: resp.StatusCode, message: strings.TrimSpace(string(data))}
	}
	return resp, nil
}

// newRequest constructs and signs an S3 request with explicit payload hash and
// content length when known.
func (s *S3Store) newRequest(ctx context.Context, method, rawURL string, body io.Reader, size int64, payloadHash string, headers http.Header) (*http.Request, error) {
	if body == nil {
		body = http.NoBody
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	if payloadHash == "" {
		// SigV4 always signs a payload hash; empty-body requests use the fixed
		// SHA-256 value rather than omitting the header.
		payloadHash = emptyPayloadSHA256
	}
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("x-amz-date", time.Now().UTC().Format("20060102T150405Z"))
	for name, values := range headers {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	signS3Request(req, s.cfg, payloadHash)
	return req, nil
}

// bucketURL returns the path-style bucket URL used for MinIO compatibility.
func (s *S3Store) bucketURL() string {
	return s.cfg.Endpoint + "/" + escapePath(s.cfg.Bucket)
}

// objectURL returns the escaped path-style URL for one object key.
func (s *S3Store) objectURL(key string) string {
	return s.bucketURL() + "/" + escapePath(key)
}

// objectURLWithQuery appends encoded S3 subresource query parameters.
func (s *S3Store) objectURLWithQuery(key string, query url.Values) string {
	rawURL := s.objectURL(key)
	encoded := query.Encode()
	if encoded == "" {
		return rawURL
	}
	return rawURL + "?" + encoded
}

// keyFromRef validates an s3://bucket/key ref and ensures it targets this
// store bucket.
func (s *S3Store) keyFromRef(ref string) (string, error) {
	const prefix = "s3://"
	if !strings.HasPrefix(ref, prefix) {
		return "", errors.New("unsupported object ref")
	}
	rest := strings.TrimPrefix(ref, prefix)
	bucket, key, ok := strings.Cut(rest, "/")
	if !ok || bucket == "" || key == "" {
		return "", errors.New("invalid s3 object ref")
	}
	if bucket != s.cfg.Bucket {
		// Ref buckets are pinned to the configured store to avoid accidental
		// cross-bucket reads when metadata is copied between deployments.
		return "", fmt.Errorf("s3 object bucket %q does not match configured bucket %q", bucket, s.cfg.Bucket)
	}
	return key, nil
}

// s3StatusError carries HTTP status and a bounded response body snippet.
type s3StatusError struct {
	statusCode int
	message    string
}

// Error formats an S3 HTTP failure for callers and tests.
func (e *s3StatusError) Error() string {
	if e.message == "" {
		return fmt.Sprintf("s3 request failed: status %d", e.statusCode)
	}
	return fmt.Sprintf("s3 request failed: status %d: %s", e.statusCode, e.message)
}

// initiateMultipartUploadResult decodes the UploadId from S3 initiate XML.
type initiateMultipartUploadResult struct {
	UploadID string `xml:"UploadId"`
}

// completeMultipartUploadRequest is the XML payload sent to finish multipart
// uploads.
type completeMultipartUploadRequest struct {
	XMLName xml.Name        `xml:"CompleteMultipartUpload"`
	Parts   []completedPart `xml:"Part"`
}

// completedPart records a completed part number and ETag for the final XML body.
type completedPart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

// s3ErrorResult decodes embedded S3 error XML from successful HTTP responses.
type s3ErrorResult struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

// signS3Request applies AWS Signature Version 4 using the request path, query,
// signed headers, and payload hash.
func signS3Request(req *http.Request, cfg S3Config, payloadHash string) {
	amzDate := req.Header.Get("x-amz-date")
	shortDate := amzDate[:8]
	scope := shortDate + "/" + cfg.Region + "/s3/aws4_request"
	signedHeaders, canonicalHeaders := canonicalSignedHeaders(req)
	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")
	signature := hex.EncodeToString(hmacSHA256(signingKey(cfg.SecretKey, shortDate, cfg.Region), []byte(stringToSign)))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+cfg.AccessKey+"/"+scope+", SignedHeaders="+signedHeaders+", Signature="+signature)
}

// canonicalSignedHeaders returns the signed header list and canonical header
// block required by SigV4.
func canonicalSignedHeaders(req *http.Request) (string, string) {
	values := map[string][]string{"host": {req.URL.Host}}
	for name, headerValues := range req.Header {
		lower := strings.ToLower(name)
		if lower == "authorization" {
			continue
		}

		// Sign host and x-amz-* headers because those carry the payload hash, date,
		// checksums, and metadata used by S3-compatible servers.
		if lower == "host" || strings.HasPrefix(lower, "x-amz-") {
			values[lower] = append(values[lower], headerValues...)
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var canonical strings.Builder
	for _, key := range keys {
		canonical.WriteString(key)
		canonical.WriteByte(':')
		canonical.WriteString(normalizeHeaderValues(values[key]))
		canonical.WriteByte('\n')
	}
	return strings.Join(keys, ";"), canonical.String()
}

// normalizeHeaderValues collapses whitespace and joins repeated header values
// according to SigV4 canonicalization rules.
func normalizeHeaderValues(values []string) string {
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		normalized = append(normalized, strings.Join(strings.Fields(value), " "))
	}
	return strings.Join(normalized, ",")
}

// signingKey derives the SigV4 date/region/service signing key.
func signingKey(secret, date, region string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte("s3"))
	return hmacSHA256(kService, []byte("aws4_request"))
}

// hmacSHA256 returns the HMAC-SHA256 digest for SigV4 key derivation and
// signatures.
func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(data)
	return mac.Sum(nil)
}

// sha256Hex returns a lowercase hex SHA-256 digest.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// escapePath path-escapes each key segment while preserving slash separators.
func escapePath(value string) string {
	parts := strings.Split(filepathSlash(value), "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

// filepathSlash normalizes Windows separators before building S3 keys or URLs.
func filepathSlash(value string) string {
	return strings.ReplaceAll(value, "\\", "/")
}

// checksumMetadataHeaders stores whole-object checksum and size as S3 user
// metadata so later reads can recover ObjectInfo.
func checksumMetadataHeaders(hashHex string, size int64) http.Header {
	headers := http.Header{}
	headers.Set("x-amz-meta-sha256", hashHex)
	headers.Set("x-amz-meta-size", strconv.FormatInt(size, 10))
	return headers
}

// objectInfoFromS3Headers maps S3 response headers and user metadata into the
// generic ObjectInfo shape.
func objectInfoFromS3Headers(ref string, headers http.Header, contentLength int64) ObjectInfo {
	metadata := make(map[string]string)
	for name, values := range headers {
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, "x-amz-meta-") || len(values) == 0 {
			continue
		}
		metadata[strings.TrimPrefix(lower, "x-amz-meta-")] = values[0]
	}
	size := contentLength

	// Some S3-compatible responses omit Content-Length for streamed bodies; fall back
	// to the size metadata written at Put time.
	if size < 0 {
		if parsed, err := strconv.ParseInt(metadata["size"], 10, 64); err == nil {
			size = parsed
		}
	}
	return ObjectInfo{
		Ref:            ref,
		Size:           size,
		SHA256:         metadata["sha256"],
		ChecksumSHA256: headers.Get("x-amz-checksum-sha256"),
		ETag:           headers.Get("ETag"),
		Metadata:       metadata,
	}
}

// spoolToTemp copies a stream to a temp file while computing both hex and base64
// SHA-256 encodings needed by S3 requests.
func spoolToTemp(ctx context.Context, r io.Reader, size int64) (*os.File, string, int64, string, string, error) {
	file, err := os.CreateTemp("", "logserve-s3-object-*.tmp")
	if err != nil {
		return nil, "", 0, "", "", err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = file.Close()
			_ = os.Remove(file.Name())
		}
	}()
	h := sha256.New()
	written, err := copyWithContext(ctx, io.MultiWriter(file, h), r)
	if err != nil {
		return nil, "", 0, "", "", err
	}
	if size >= 0 && written != size {
		return nil, "", 0, "", "", fmt.Errorf("object size mismatch: wrote %d bytes, expected %d", written, size)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, "", 0, "", "", err
	}
	sum := h.Sum(nil)
	// cleanup=false transfers responsibility for closing/removing the temp file
	// to the caller, which needs the file for a later single or multipart upload.
	cleanup = false
	return file, file.Name(), written, hex.EncodeToString(sum), base64.StdEncoding.EncodeToString(sum), nil
}

// envInt64 parses a positive int64 environment value or returns fallback.
func envInt64(name string, fallback int64) int64 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

// firstNonEmpty returns the first non-empty string in a fallback chain.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
