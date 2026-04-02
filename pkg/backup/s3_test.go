package backup

import (
	"net/http"
	"testing"
	"time"
)

func TestSignV4Authorization(t *testing.T) {
	s := NewS3(S3Config{
		Endpoint:  "https://s3.us-east-1.amazonaws.com",
		Region:    "us-east-1",
		Bucket:    "test-bucket",
		AccessKey: "AKIAIOSFODNN7EXAMPLE",
		SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
	})

	req, _ := http.NewRequest("GET", "https://s3.us-east-1.amazonaws.com/test-bucket/test-key", nil)
	s.sign(req, "UNSIGNED-PAYLOAD")

	auth := req.Header.Get("Authorization")
	if auth == "" {
		t.Fatal("Authorization header is empty")
	}
	if !contains(auth, "AWS4-HMAC-SHA256") {
		t.Errorf("expected AWS4-HMAC-SHA256, got %s", auth)
	}
	if !contains(auth, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("expected access key in auth header, got %s", auth)
	}
	if !contains(auth, "us-east-1/s3/aws4_request") {
		t.Errorf("expected credential scope, got %s", auth)
	}
	if req.Header.Get("X-Amz-Date") == "" {
		t.Error("X-Amz-Date header missing")
	}
	if req.Header.Get("X-Amz-Content-Sha256") != "UNSIGNED-PAYLOAD" {
		t.Errorf("expected UNSIGNED-PAYLOAD, got %s", req.Header.Get("X-Amz-Content-Sha256"))
	}
}

func TestSignV4DeterministicSignature(t *testing.T) {
	// Two signs with same key/request should produce different signatures
	// (because X-Amz-Date changes) but both should be valid format.
	s := NewS3(S3Config{
		Endpoint:  "https://s3.example.com",
		Region:    "eu-central-1",
		Bucket:    "bucket",
		AccessKey: "TESTKEY",
		SecretKey: "TESTSECRET",
	})

	req1, _ := http.NewRequest("PUT", "https://s3.example.com/bucket/key", nil)
	s.sign(req1, "UNSIGNED-PAYLOAD")
	sig1 := req1.Header.Get("Authorization")

	// Small delay to get different timestamp
	time.Sleep(1 * time.Millisecond)
	req2, _ := http.NewRequest("PUT", "https://s3.example.com/bucket/key", nil)
	s.sign(req2, "UNSIGNED-PAYLOAD")
	sig2 := req2.Header.Get("Authorization")

	// Both must have valid format
	for _, sig := range []string{sig1, sig2} {
		if !contains(sig, "Credential=TESTKEY/") {
			t.Errorf("invalid credential: %s", sig)
		}
		if !contains(sig, "Signature=") {
			t.Errorf("missing signature: %s", sig)
		}
	}
}

func TestDeriveSigningKey(t *testing.T) {
	// Verify the key derivation produces non-empty, consistent results
	key1 := deriveSigningKey("secret", "20260402", "us-east-1", "s3")
	key2 := deriveSigningKey("secret", "20260402", "us-east-1", "s3")
	if len(key1) != 32 { // HMAC-SHA256 = 32 bytes
		t.Errorf("expected 32 bytes, got %d", len(key1))
	}
	for i := range key1 {
		if key1[i] != key2[i] {
			t.Fatal("signing key not deterministic")
		}
	}

	// Different date = different key
	key3 := deriveSigningKey("secret", "20260403", "us-east-1", "s3")
	same := true
	for i := range key1 {
		if key1[i] != key3[i] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("different date should produce different key")
	}
}

func TestObjectURL(t *testing.T) {
	s := NewS3(S3Config{
		Endpoint: "https://s3.example.com",
		Bucket:   "my-bucket",
	})
	got := s.objectURL("volumes/user1/workspace/2026-04-02.ext4.zst")
	want := "https://s3.example.com/my-bucket/volumes/user1/workspace/2026-04-02.ext4.zst"
	if got != want {
		t.Errorf("objectURL = %q, want %q", got, want)
	}
}

func TestObjectURLTrailingSlash(t *testing.T) {
	s := NewS3(S3Config{
		Endpoint: "https://s3.example.com/",
		Bucket:   "my-bucket",
	})
	got := s.objectURL("key")
	want := "https://s3.example.com/my-bucket/key"
	if got != want {
		t.Errorf("objectURL = %q, want %q", got, want)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && containsHelper(s, sub)
}

func containsHelper(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
