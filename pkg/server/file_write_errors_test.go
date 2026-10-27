package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

// fileWriteEngine is a FileEngine whose FileWrite fails with a canned error.
type fileWriteEngine struct {
	*mockEngine
	err error
}

func (e *fileWriteEngine) FileWrite(context.Context, string, string, string, int64, io.Reader) error {
	return e.err
}
func (e *fileWriteEngine) FileRead(context.Context, string, string, io.Writer, ...agent.FileReadOpts) (int64, string, error) {
	return 0, "", nil
}
func (e *fileWriteEngine) FileStat(context.Context, string, string) (*proto.FileInfo, error) {
	return nil, nil
}
func (e *fileWriteEngine) FileList(context.Context, string, string) ([]proto.FileInfo, error) {
	return nil, nil
}

func TestHTTPFileWriteMapsGuestRefusals(t *testing.T) {
	srv, ts := setup(t)
	sb := createSandbox(t, ts, "file-write-errors")
	eng := &fileWriteEngine{mockEngine: srv.engine.(*mockEngine)}
	srv.engine = eng

	for _, tc := range []struct {
		name     string
		err      error
		status   int
		contains string
	}{
		{"no space", fmt.Errorf("no space: need 189 MB, 1.2 GB available: %w", agent.ErrNoSpace), http.StatusInsufficientStorage, "no space: need 189 MB, 1.2 GB available"},
		{"old agent cap", fmt.Errorf("file too large: 198180864 bytes (max 104857600): %w", agent.ErrFileTooLarge), http.StatusRequestEntityTooLarge, "file too large: 198180864 bytes"},
		{"anything else", errors.New("write stdin frame: broken pipe at /secret/internal/path"), http.StatusInternalServerError, "internal error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng.err = tc.err
			req, _ := http.NewRequest(http.MethodPut, ts.URL+"/sandboxes/"+sb.ID+"/files?path=/workspace/x", bytes.NewReader([]byte("data")))
			req.Header.Set("Authorization", "Bearer "+testAPIKey)
			resp, err := testHTTPClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.status || !strings.Contains(string(body), tc.contains) {
				t.Fatalf("PUT = %d %s; want %d containing %q", resp.StatusCode, body, tc.status, tc.contains)
			}
			if tc.status == http.StatusInternalServerError && strings.Contains(string(body), "/secret") {
				t.Fatalf("500 leaked the internal error: %s", body)
			}
		})
	}
}

// The engine refuses before reading the body (the guest said "no space:"), so
// the upload is still streaming when the 507 goes out. The client must get
// that response, not a reset from the half-sent body.
func TestHTTPFileWriteRejectionReachesClientMidUpload(t *testing.T) {
	srv, ts := setup(t)
	sb := createSandbox(t, ts, "file-write-midupload")
	srv.engine = &fileWriteEngine{mockEngine: srv.engine.(*mockEngine), err: fmt.Errorf("no space: need 1 GB: %w", agent.ErrNoSpace)}

	// Keep-alive client, like the CLI and SDKs: net/http only lets the 507 win
	// the race with the upload when the request did not ask for Connection: close.
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)

	const size = 4 << 30
	for i := range 5 {
		req, _ := http.NewRequest(http.MethodPut, ts.URL+"/sandboxes/"+sb.ID+"/files?path=/workspace/x", io.LimitReader(zeroReader{}, size))
		req.ContentLength = size
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("attempt %d: %v; want the 507 response", i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusInsufficientStorage || !strings.Contains(string(body), "no space: need 1 GB") {
			t.Fatalf("attempt %d: PUT = %d %s", i, resp.StatusCode, body)
		}
	}
}
