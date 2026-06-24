package objectstore

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

const (
	defaultS3MultipartThreshold = 64 << 20
	defaultS3MultipartPartSize  = 16 << 20
	minS3MultipartPartSize      = 5 << 20
	emptyPayloadSHA256          = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

type S3Config struct {
	Endpoint           string
	Bucket             string
	Region             string
	AccessKey          string
	SecretKey          string
	CreateBucket       bool
	MultipartThreshold int64
	MultipartPartSize  int64
}

type S3Store struct {
	cfg       S3Config
	client    *http.Client
	ensure    sync.Once
	ensureErr error
}

func S3ConfigFromEnv() S3Config {
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
	_ = ctx
	return store, nil
}

func (s *S3Store) Put(ctx context.Context, namespace string, r io.Reader, size int64) (string, error) {
	if err := s.ensureBucket(ctx); err != nil {
		return "", err
	}
	file, tmpPath, actualSize, hashHex, hashBase64, err := spoolToTemp(ctx, r, size)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = file.Close()
		_ = os.Remove(tmpPath)
	}()

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

func (s *S3Store) ensureBucket(ctx context.Context) error {
	if !s.cfg.CreateBucket {
		return nil
	}
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

func (s *S3Store) putSingle(ctx context.Context, key string, file *os.File, size int64, hashHex, hashBase64 string) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	headers := checksumMetadataHeaders(hashHex, size)
	headers.Set("x-amz-checksum-sha256", hashBase64)
	return s.do(ctx, http.MethodPut, key, file, size, hashHex, headers, nil)
}

func (s *S3Store) putMultipart(ctx context.Context, key string, file *os.File, size int64, hashHex string) error {
	uploadID, err := s.createMultipartUpload(ctx, key, hashHex, size)
	if err != nil {
		return err
	}
	completed := false
	defer func() {
		if !completed {
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

func (s *S3Store) uploadParts(ctx context.Context, key, uploadID string, file *os.File, size int64) ([]completedPart, error) {
	parts := make([]completedPart, 0, int((size+s.cfg.MultipartPartSize-1)/s.cfg.MultipartPartSize))
	for offset, partNumber := int64(0), 1; offset < size; partNumber++ {
		partSize := s.cfg.MultipartPartSize
		if remaining := size - offset; remaining < partSize {
			partSize = remaining
		}
		section := io.NewSectionReader(file, offset, partSize)
		h := sha256.New()
		if _, err := copyWithContext(ctx, h, section); err != nil {
			return nil, err
		}
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

func (s *S3Store) abortMultipartUpload(ctx context.Context, key, uploadID string) error {
	query := url.Values{"uploadId": {uploadID}}
	return s.doRequest(ctx, http.MethodDelete, s.objectURLWithQuery(key, query), nil, 0, emptyPayloadSHA256, nil, nil)
}

func (s *S3Store) do(ctx context.Context, method, key string, body io.Reader, size int64, payloadHash string, headers http.Header, handle func(*http.Response) error) error {
	return s.doRequest(ctx, method, s.objectURL(key), body, size, payloadHash, headers, handle)
}

func (s *S3Store) doBucket(ctx context.Context, method string, body io.Reader, size int64, payloadHash string, headers http.Header) error {
	return s.doRequest(ctx, method, s.bucketURL(), body, size, payloadHash, headers, nil)
}

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
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, &s3StatusError{statusCode: resp.StatusCode, message: strings.TrimSpace(string(data))}
	}
	return resp, nil
}

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

func (s *S3Store) bucketURL() string {
	return s.cfg.Endpoint + "/" + escapePath(s.cfg.Bucket)
}

func (s *S3Store) objectURL(key string) string {
	return s.bucketURL() + "/" + escapePath(key)
}

func (s *S3Store) objectURLWithQuery(key string, query url.Values) string {
	rawURL := s.objectURL(key)
	encoded := query.Encode()
	if encoded == "" {
		return rawURL
	}
	return rawURL + "?" + encoded
}

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
		return "", fmt.Errorf("s3 object bucket %q does not match configured bucket %q", bucket, s.cfg.Bucket)
	}
	return key, nil
}

type s3StatusError struct {
	statusCode int
	message    string
}

func (e *s3StatusError) Error() string {
	if e.message == "" {
		return fmt.Sprintf("s3 request failed: status %d", e.statusCode)
	}
	return fmt.Sprintf("s3 request failed: status %d: %s", e.statusCode, e.message)
}

type initiateMultipartUploadResult struct {
	UploadID string `xml:"UploadId"`
}

type completeMultipartUploadRequest struct {
	XMLName xml.Name        `xml:"CompleteMultipartUpload"`
	Parts   []completedPart `xml:"Part"`
}

type completedPart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

type s3ErrorResult struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

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

func canonicalSignedHeaders(req *http.Request) (string, string) {
	values := map[string][]string{"host": {req.URL.Host}}
	for name, headerValues := range req.Header {
		lower := strings.ToLower(name)
		if lower == "authorization" {
			continue
		}
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

func normalizeHeaderValues(values []string) string {
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		normalized = append(normalized, strings.Join(strings.Fields(value), " "))
	}
	return strings.Join(normalized, ",")
}

func signingKey(secret, date, region string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte("s3"))
	return hmacSHA256(kService, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(data)
	return mac.Sum(nil)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func escapePath(value string) string {
	parts := strings.Split(filepathSlash(value), "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func filepathSlash(value string) string {
	return strings.ReplaceAll(value, "\\", "/")
}

func checksumMetadataHeaders(hashHex string, size int64) http.Header {
	headers := http.Header{}
	headers.Set("x-amz-meta-sha256", hashHex)
	headers.Set("x-amz-meta-size", strconv.FormatInt(size, 10))
	return headers
}

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
	cleanup = false
	return file, file.Name(), written, hex.EncodeToString(sum), base64.StdEncoding.EncodeToString(sum), nil
}

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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
