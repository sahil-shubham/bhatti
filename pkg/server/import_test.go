package server

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"net/http"
	"testing"
)

func TestImageImportEndpoint(t *testing.T) {
	_, ts := setup(t)

	// Create a minimal but valid tarball — the server will fail at
	// oci.ImportFromTarball because we don't have mke2fs / lohar on
	// the CI runner. But we can test the endpoint routing, name
	// validation, and duplicate detection.

	// Empty tarball body — will fail at import, but should get past
	// name validation
	resp := doReq(t, ts, "POST", "/images/import?name=test-img", nil)
	// The server reads Content-Type to decide mode. Our doReq sends
	// application/json, so it tries to parse JSON. Let's send raw tar.
	resp.Body.Close()

	// Send as application/x-tar
	body := bytes.NewReader([]byte("not a real tarball"))
	req, _ := http.NewRequest("POST", ts.URL+"/images/import?name=test-import", body)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Should fail at tarball parsing (400), not at routing (404)
	if resp.StatusCode == 404 {
		t.Fatal("endpoint not found — routing broken")
	}
	if resp.StatusCode == 405 {
		t.Fatal("method not allowed — routing broken")
	}
	// 400 is expected (invalid tarball)
	var errResp struct{ Error string `json:"error"` }
	json.NewDecoder(resp.Body).Decode(&errResp)
	t.Logf("import response: %d %s", resp.StatusCode, errResp.Error)
}

func TestImageImportNameValidation(t *testing.T) {
	_, ts := setup(t)

	// Missing name
	body := bytes.NewReader([]byte("fake"))
	req, _ := http.NewRequest("POST", ts.URL+"/images/import", body)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/x-tar")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400 for missing name, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Invalid name
	body = bytes.NewReader([]byte("fake"))
	req, _ = http.NewRequest("POST", ts.URL+"/images/import?name=../etc/passwd", body)
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/x-tar")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 400 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400 for invalid name, got %d: %s", resp.StatusCode, bodyBytes)
	}
	resp.Body.Close()
}

// An install without the guest agent (lohar) can't convert images: pull and
// import answer 501 with the reason up front, instead of a pull task that fails
// in the background ("inject lohar: open …: no such file").
func TestImagePullWithoutAgentIs501(t *testing.T) {
	srv, ts := setup(t)
	srv.loharPath = filepath.Join(t.TempDir(), "no-lohar-here")
	resp := doReq(t, ts, "POST", "/images/pull", map[string]any{"ref": "alpine:3", "name": "alpine"})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 501 || !strings.Contains(string(body), "lohar") {
		t.Fatalf("pull without the agent: %d %s, want 501 naming lohar", resp.StatusCode, body)
	}
}
