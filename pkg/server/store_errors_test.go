package server

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// A volume's name is the caller's choice, and any word an error message about
// volumes contains can be one. What the delete answers depends on what the
// store reports, never on that text.
func TestVolumeDeleteStatusFollowsTheKindOfError(t *testing.T) {
	for _, name := range []string{"attachment", "not-found", "in-use", "already"} {
		t.Run(name, func(t *testing.T) {
			_, ts := setup(t)
			status := func(method, path string, body any) int {
				t.Helper()
				resp := doReq(t, ts, method, path, body)
				resp.Body.Close()
				return resp.StatusCode
			}

			if got := status("DELETE", "/volumes/"+name, nil); got != 404 {
				t.Fatalf("delete of a volume that does not exist: %d, want 404", got)
			}
			if got := status("POST", "/volumes", map[string]any{"name": name, "size_mb": 64}); got != 201 {
				t.Fatalf("create: %d", got)
			}
			resp := doReq(t, ts, "POST", "/sandboxes", map[string]any{
				"name":               uniqueName(t, "sb"),
				"persistent_volumes": []map[string]any{{"name": name, "mount": "/workspace"}},
			})
			if resp.StatusCode != 201 {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("create sandbox: %d %s", resp.StatusCode, body)
			}
			var sb struct{ ID string }
			decodeJSON(t, resp, &sb)

			if got := status("DELETE", "/volumes/"+name, nil); got != 409 {
				t.Fatalf("delete while attached: %d, want 409", got)
			}
			if got := status("GET", "/volumes/"+name, nil); got != 200 {
				t.Fatalf("a refused delete removed the volume: GET = %d", got)
			}
			if got := status("DELETE", "/sandboxes/"+sb.ID, nil); got != 200 {
				t.Fatalf("destroy the sandbox: %d", got)
			}
			if got := status("DELETE", "/volumes/"+name, nil); got != 200 {
				t.Fatalf("delete once detached: %d, want 200", got)
			}
		})
	}
}

// Someone else can delete the volume between this request's lookup and its
// delete. The delete then finds nothing: that is a 404 for a volume of any
// name. It was a 409 for one named "attachment", whose "not found" message
// contained the word the handler took to mean "attached".
func TestVolumeDeleteLostRaceIsNotFound(t *testing.T) {
	srv, ts := setup(t)
	const name = "attachment"
	resp := doReq(t, ts, "POST", "/volumes", map[string]any{"name": name, "size_mb": 64})
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("create: %d", resp.StatusCode)
	}

	// A second connection takes the database's write lock and deletes the
	// row without committing. The request below can still see the volume, so
	// it gets past its lookup, and then waits for the lock its own delete
	// needs.
	other, err := sql.Open("sqlite", filepath.Join(srv.dataDir, "test.db")+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	tx, err := other.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM volumes_v2 WHERE name = ?`, name); err != nil {
		t.Fatal(err)
	}

	status := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest("DELETE", ts.URL+"/volumes/"+name, nil)
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		resp, err := testHTTPClient.Do(req)
		if err != nil {
			status <- -1
			return
		}
		resp.Body.Close()
		status <- resp.StatusCode
	}()
	select {
	case got := <-status:
		t.Fatalf("the request finished (%d) while the volume was still locked: it never reached its delete", got)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-status:
		if got != 404 {
			t.Fatalf("delete of a volume deleted under it: %d, want 404", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the request did not finish after the lock was released")
	}
}

// takenNameEngine reports every snapshot name as already on disk, the way the
// engine does when a snapshot directory exists.
type takenNameEngine struct{ *mockEngine }

func (takenNameEngine) Checkpoint(_ context.Context, _, _ string, _ int, name, _ string) (any, error) {
	return nil, fmt.Errorf("snapshot %q %w", name, engine.ErrSnapshotExists)
}

func TestCheckpointNameTakenIsConflict(t *testing.T) {
	srv, ts := setup(t)
	srv.engine = takenNameEngine{srv.engine.(*mockEngine)}
	sb := createSandbox(t, ts, uniqueName(t, "sb"))

	resp := doReq(t, ts, "POST", "/sandboxes/"+sb.ID+"/checkpoint", map[string]any{"name": "dev-ready"})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatalf("checkpoint under a taken name: %d %s, want 409", resp.StatusCode, body)
	}
}
