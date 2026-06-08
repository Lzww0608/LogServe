package objectstore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

type S3Config struct {
	Endpoint     string
	Bucket       string
	Region       string
	AccessKey    string
	SecretKey    string
	CreateBucket bool
}

type S3Store struct {
	cfg    S3Config
	client *http.Client
}

func S3ConfigFromEnv() S3Config {
	createBucket := os.Getenv("LOGSERVE_S3_CREATE_BUCKET") != "0"
	return S3Config{
		Endpoint:     firstNonEmpty(os.Getenv("LOGSERVE_S3_ENDPOINT"), os.Getenv("MINIO_ENDPOINT")),
		Bucket:       firstNonEmpty(os.Getenv("LOGSERVE_S3_BUCKET"), "logserve-results"),
		Region:       firstNonEmpty(os.Getenv("LOGSERVE_S3_REGION"), "us-east-1"),
		AccessKey:    firstNonEmpty(os.Getenv("LOGSERVE_S3_ACCESS_KEY"), os.Getenv("MINIO_ROOT_USER")),
		SecretKey:    firstNonEmpty(os.Getenv("LOGSERVE_S3_SECRET_KEY"), os.Getenv("MINIO_ROOT_PASSWORD")),
		CreateBucket: createBucket,
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
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	store := &S3Store{cfg: cfg, client: &http.Client{Timeout: 30 * time.Second}}
	return store, nil
}

func (s *S3Store) Put(ctx context.Context, namespace string, data []byte) (string, error) {
	if s.cfg.CreateBucket {
		if err := s.ensureBucket(ctx); err != nil {
			return "", err
		}
	}
	sum := sha256.Sum256(data)
	key := path.Join(filepathSlash(cleanNamespace(namespace)), hex.EncodeToString(sum[:])+".json")
	if err := s.do(ctx, http.MethodPut, key, data, nil); err != nil {
		return "", err
	}
	return "s3://" + s.cfg.Bucket + "/" + key, nil
}

func (s *S3Store) Get(ctx context.Context, ref string) ([]byte, error) {
	const prefix = "s3://"
	if !strings.HasPrefix(ref, prefix) {
		return nil, errors.New("unsupported object ref")
	}
	rest := strings.TrimPrefix(ref, prefix)
	bucket, key, ok := strings.Cut(rest, "/")
	if !ok || bucket == "" || key == "" {
		return nil, errors.New("invalid s3 object ref")
	}
	if bucket != s.cfg.Bucket {
		return nil, fmt.Errorf("s3 object bucket %q does not match configured bucket %q", bucket, s.cfg.Bucket)
	}
	var out []byte
	err := s.do(ctx, http.MethodGet, key, nil, func(resp *http.Response) error {
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		out = data
		return nil
	})
	return out, err
}

func (s *S3Store) ensureBucket(ctx context.Context) error {
	err := s.doBucket(ctx, http.MethodPut, nil)
	var statusErr *s3StatusError
	if errors.As(err, &statusErr) && (statusErr.statusCode == http.StatusConflict || statusErr.statusCode == http.StatusOK) {
		return nil
	}
	return err
}

func (s *S3Store) do(ctx context.Context, method, key string, body []byte, handle func(*http.Response) error) error {
	return s.doRequest(ctx, method, s.objectURL(key), body, handle)
}

func (s *S3Store) doBucket(ctx context.Context, method string, body []byte) error {
	return s.doRequest(ctx, method, s.bucketURL(), body, nil)
}

func (s *S3Store) doRequest(ctx context.Context, method, rawURL string, body []byte, handle func(*http.Response) error) error {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	payloadHash := sha256Hex(body)
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("x-amz-date", time.Now().UTC().Format("20060102T150405Z"))
	signS3Request(req, s.cfg, payloadHash)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &s3StatusError{statusCode: resp.StatusCode, message: strings.TrimSpace(string(data))}
	}
	if handle != nil {
		return handle(resp)
	}
	return nil
}

func (s *S3Store) bucketURL() string {
	return s.cfg.Endpoint + "/" + escapePath(s.cfg.Bucket)
}

func (s *S3Store) objectURL(key string) string {
	return s.bucketURL() + "/" + escapePath(key)
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

func signS3Request(req *http.Request, cfg S3Config, payloadHash string) {
	amzDate := req.Header.Get("x-amz-date")
	shortDate := amzDate[:8]
	scope := shortDate + "/" + cfg.Region + "/s3/aws4_request"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalHeaders := "host:" + req.URL.Host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
