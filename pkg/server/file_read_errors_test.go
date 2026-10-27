package server

import (
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

// fileReadEngine is a FileEngine whose stat and list fail with a canned error.
type fileReadEngine struct {
	*mockEngine
	err error
}

func (e *fileReadEngine) FileWrite(context.Context, string, string, string, int64, io.Reader) error {
	return nil
}
func (e *fileReadEngine) FileRead(context.Context, string, string, io.Writer, ...agent.FileReadOpts) (int64, string, error) {
	return 0, "", e.err
}
func (e *fileReadEngine) FileStat(context.Context, string, string) (*proto.FileInfo, error) {
	return nil, e.err
}
func (e *fileReadEngine) FileList(context.Context, string, string) ([]proto.FileInfo, error) {
	return nil, e.err
}

func TestHTTPFileReadStatListMissingPathIs404(t *testing.T) {
	srv, ts := setup(t)
	sb := createSandbox(t, ts, "file-read-errors")
	eng := &fileReadEngine{mockEngine: srv.engine.(*mockEngine)}
	srv.engine = eng

	missing := fmt.Errorf("agent: stat /workspace/nope: no such file or directory: %w", agent.ErrFileNotFound)
	for _, tc := range []struct {
		name     string
		method   string
		query    string
		err      error
		status   int
		contains string
	}{
		{"read missing", http.MethodGet, "path=/workspace/nope", missing, http.StatusNotFound, "/workspace/nope"},
		{"list missing", http.MethodGet, "path=/workspace/nope&ls=true", missing, http.StatusNotFound, "/workspace/nope"},
		{"stat missing", http.MethodHead, "path=/workspace/nope", missing, http.StatusNotFound, ""},
		{"read other error", http.MethodGet, "path=/workspace/x", errors.New("agent: open /secret/internal: permission denied"), http.StatusInternalServerError, "internal error"},
		{"list other error", http.MethodGet, "path=/workspace/x&ls=true", errors.New("agent: open /secret/internal: permission denied"), http.StatusInternalServerError, "internal error"},
		{"stat other error", http.MethodHead, "path=/workspace/x", errors.New("read stat resp: i/o timeout"), http.StatusInternalServerError, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng.err = tc.err
			req, _ := http.NewRequest(tc.method, ts.URL+"/sandboxes/"+sb.ID+"/files?"+tc.query, nil)
			req.Header.Set("Authorization", "Bearer "+testAPIKey)
			resp, err := testHTTPClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.status || !strings.Contains(string(body), tc.contains) {
				t.Fatalf("%s = %d %s; want %d containing %q", tc.method, resp.StatusCode, body, tc.status, tc.contains)
			}
			if strings.Contains(string(body), "/secret") {
				t.Fatalf("500 leaked the internal error: %s", body)
			}
		})
	}
}
