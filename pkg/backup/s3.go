package backup

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// S3Config configures an S3-compatible backend.
type S3Config struct {
	Endpoint  string // e.g. "https://s3.eu-central-003.backblazeb2.com"
	Region    string // e.g. "eu-central-003"
	Bucket    string
	AccessKey string
	SecretKey string
}

// S3 implements Backend for any S3-compatible object storage.
// Uses AWS Signature V4 signing with no external dependencies.
type S3 struct {
	cfg    S3Config
	client *http.Client
}

// NewS3 creates an S3 backend.
func NewS3(cfg S3Config) *S3 {
	return &S3{
		cfg: cfg,
		client: &http.Client{
			Timeout: 0, // per-request context controls timeout
		},
	}
}

func (s *S3) Upload(ctx context.Context, key string, r io.ReadSeeker, size int64) error {
	u := s.objectURL(key)
	req, err := http.NewRequestWithContext(ctx, "PUT", u, r)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")

	s.sign(req, "UNSIGNED-PAYLOAD")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("s3 upload: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("s3 upload %s: %s %s", key, resp.Status, string(body))
	}
	return nil
}

func (s *S3) Download(ctx context.Context, key string) (io.ReadCloser, error) {
	u := s.objectURL(key)
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}

	s.sign(req, "UNSIGNED-PAYLOAD")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("s3 download: %w", err)
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("s3 download %s: %s %s", key, resp.Status, string(body))
	}
	return resp.Body, nil
}

func (s *S3) List(ctx context.Context, prefix string) ([]Entry, error) {
	var entries []Entry
	var continuationToken string

	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if continuationToken != "" {
			q.Set("continuation-token", continuationToken)
		}

		u := s.bucketURL() + "?" + q.Encode()
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return nil, err
		}

		s.sign(req, "UNSIGNED-PAYLOAD")

		resp, err := s.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("s3 list: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return nil, fmt.Errorf("s3 list: %s %s", resp.Status, string(body))
		}

		var result listBucketResult
		if err := xml.NewDecoder(resp.Body).Decode(&result); err != nil {
			return nil, fmt.Errorf("s3 list parse: %w", err)
		}

		for _, obj := range result.Contents {
			t, _ := time.Parse(time.RFC3339Nano, obj.LastModified)
			entries = append(entries, Entry{
				Key:       obj.Key,
				Size:      obj.Size,
				Timestamp: t,
			})
		}

		if !result.IsTruncated {
			break
		}
		continuationToken = result.NextContinuationToken
	}
	return entries, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	u := s.objectURL(key)
	req, err := http.NewRequestWithContext(ctx, "DELETE", u, nil)
	if err != nil {
		return err
	}

	s.sign(req, "UNSIGNED-PAYLOAD")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("s3 delete: %w", err)
	}
	defer resp.Body.Close()
	// S3 returns 204 on success, but some implementations return 200
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("s3 delete %s: %s %s", key, resp.Status, string(body))
	}
	return nil
}

// --- S3 URL helpers ---

func (s *S3) bucketURL() string {
	endpoint := strings.TrimRight(s.cfg.Endpoint, "/")
	return endpoint + "/" + s.cfg.Bucket
}

func (s *S3) objectURL(key string) string {
	return s.bucketURL() + "/" + key
}

// --- XML response types ---

type listBucketResult struct {
	XMLName               xml.Name       `xml:"ListBucketResult"`
	Contents              []s3Object     `xml:"Contents"`
	IsTruncated           bool           `xml:"IsTruncated"`
	NextContinuationToken string         `xml:"NextContinuationToken"`
}

type s3Object struct {
	Key          string `xml:"Key"`
	Size         int64  `xml:"Size"`
	LastModified string `xml:"LastModified"`
}

// --- AWS Signature V4 ---
//
// Implements the signing algorithm described at:
// https://docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-header-based-auth.html
//
// Uses UNSIGNED-PAYLOAD for the payload hash — safe over HTTPS and avoids
// buffering/double-reading large upload bodies.

func (s *S3) sign(req *http.Request, payloadHash string) {
	now := time.Now().UTC()
	datestamp := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")
	service := "s3"

	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if req.Header.Get("Host") == "" {
		req.Header.Set("Host", req.URL.Host)
	}

	// 1. Canonical request
	canonicalHeaders, signedHeaders := canonicalAndSignedHeaders(req)
	canonicalQuery := req.URL.Query().Encode()
	// URL.Query().Encode() sorts by key, which is what SigV4 requires
	canonicalURI := req.URL.Path
	if canonicalURI == "" {
		canonicalURI = "/"
	}

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		canonicalQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	// 2. String to sign
	credentialScope := datestamp + "/" + s.cfg.Region + "/" + service + "/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + credentialScope + "\n" + hashSHA256([]byte(canonicalRequest))

	// 3. Signing key
	signingKey := deriveSigningKey(s.cfg.SecretKey, datestamp, s.cfg.Region, service)

	// 4. Signature
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	// 5. Authorization header
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.cfg.AccessKey, credentialScope, signedHeaders, signature))
}

func canonicalAndSignedHeaders(req *http.Request) (canonical, signed string) {
	// Collect headers to sign (lowercase, sorted)
	type kv struct{ k, v string }
	var headers []kv
	for k := range req.Header {
		lk := strings.ToLower(k)
		headers = append(headers, kv{lk, strings.TrimSpace(req.Header.Get(k))})
	}
	sort.Slice(headers, func(i, j int) bool { return headers[i].k < headers[j].k })

	var canonicalParts []string
	var signedParts []string
	for _, h := range headers {
		canonicalParts = append(canonicalParts, h.k+":"+h.v)
		signedParts = append(signedParts, h.k)
	}

	return strings.Join(canonicalParts, "\n") + "\n", strings.Join(signedParts, ";")
}

func deriveSigningKey(secret, datestamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(datestamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	return hmacSHA256(kService, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func hashSHA256(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
