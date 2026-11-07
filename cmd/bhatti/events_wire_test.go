package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/server"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

// The CLI tests above fake the daemon from the wire contract; these run the
// command against the real handler, so the two halves cannot drift apart
// unnoticed: the parameter names, the cursor, and the two control records.

// wireSandbox has the shape of a sandbox ID, which is what the daemon takes as
// the ID of a sandbox that no longer exists; no sandbox is created here.
const wireSandbox = "0123456789abcdef"

// wireBuffer is an io.Writer a test can read while a stream is still writing it.
type wireBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *wireBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *wireBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// realEventsServer is a daemon with a recorder and one user, with the CLI
// pointed at it. The engine is absent: /events never asks it anything.
func realEventsServer(t *testing.T) (*server.Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(t.Context(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	const key = "wire-key"
	sum := sha256.Sum256([]byte(key))
	if err := st.CreateUser(t.Context(), store.User{
		ID: "usr_wire", Name: "wire", APIKeyHash: hex.EncodeToString(sum[:]),
		MaxSandboxes: 5, MaxCPUsPerSandbox: 2, MaxMemoryMBPerSandbox: 1024, SubnetIndex: 1, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	srv := server.New(nil, st, dir)
	srv.StartEventRecorder()
	ts := httptest.NewServer(srv)
	t.Cleanup(func() { srv.Close(); ts.Close(); st.Close() })
	useTestAPI(t, ts.URL)
	apiToken = key
	return srv, st
}

func TestEventsCommandAgainstTheRealServer(t *testing.T) {
	noGoroutineLeak(t)
	srv, st := realEventsServer(t)
	seed := func(events ...store.Event) {
		t.Helper()
		if err := st.InsertEvents(t.Context(), events); err != nil {
			t.Fatal(err)
		}
	}
	run := func(o eventsOptions) (string, string, error) {
		t.Helper()
		var out, errOut bytes.Buffer
		err := streamEvents(t.Context(), &out, &errOut, o)
		return out.String(), errOut.String(), err
	}
	lines := func(s string) []string { return strings.Split(strings.TrimSpace(s), "\n") }

	// A listing: the caller's events only, in order, as one line each.
	seed(
		store.Event{Type: "sandbox.created", UserID: "usr_wire", SandboxID: wireSandbox, Meta: map[string]any{"name": "dev", "cpus": 1}},
		store.Event{Type: "auth.failed", Meta: map[string]any{"ip": "203.0.113.9"}},
		store.Event{Type: "sandbox.destroyed", UserID: "usr_wire", SandboxID: wireSandbox, Meta: map[string]any{"name": "dev"}},
	)
	out, errOut, err := run(eventsOptions{})
	if err != nil || errOut != "" {
		t.Fatalf("listing: %v, stderr %q", err, errOut)
	}
	if got := lines(out); len(got) != 2 || !strings.Contains(got[0], "sandbox.created") || !strings.Contains(got[1], "sandbox.destroyed") {
		t.Fatalf("listing printed %q, want the two sandbox events", out)
	}

	// --json is the server's own lines, and --after continues from one of them.
	out, _, err = run(eventsOptions{json: true})
	if err != nil {
		t.Fatal(err)
	}
	var first store.Event
	if err := json.Unmarshal([]byte(lines(out)[0]), &first); err != nil || first.ID == 0 || first.Type != "sandbox.created" {
		t.Fatalf("first json line = %q (%v)", lines(out)[0], err)
	}
	out, _, err = run(eventsOptions{afterSet: true, after: first.ID})
	if got := lines(out); err != nil || len(got) != 1 || !strings.Contains(got[0], "sandbox.destroyed") {
		t.Fatalf("--after %d printed %q (%v), want only the event after it", first.ID, out, err)
	}

	// A filter by sandbox ID and by type reaches the server as the server reads it.
	out, _, err = run(eventsOptions{sandbox: wireSandbox, typePrefix: "sandbox.d"})
	if got := lines(out); err != nil || len(got) != 1 || !strings.Contains(got[0], "sandbox.destroyed") {
		t.Fatalf("--sandbox %s --type sandbox.d printed %q (%v)", wireSandbox, out, err)
	}
	if _, _, err = run(eventsOptions{sandbox: "no-such-box"}); err == nil || !strings.Contains(err.Error(), "no-such-box") {
		t.Fatalf("an unknown sandbox gave %v, want the server's 404 naming it", err)
	}

	// Retention removed events the cursor pointed at: the command says so.
	if n, err := st.PurgeOldEvents(t.Context(), -time.Hour); err != nil || n != 3 {
		t.Fatalf("purge removed %d, %v", n, err)
	}
	seed(store.Event{Type: "kept.event", UserID: "usr_wire"})
	out, errOut, err = run(eventsOptions{afterSet: true, after: first.ID})
	if err != nil || !strings.Contains(out, "kept.event") || !strings.Contains(errOut, "pruned") {
		t.Fatalf("stale cursor: stdout %q stderr %q err %v, want the kept event and a pruned notice", out, errOut, err)
	}

	// More than a page: 1000 lines and how to continue, with the filter echoed.
	batch := make([]store.Event, 0, 1001)
	for range 1001 {
		batch = append(batch, store.Event{Type: "wire.page", UserID: "usr_wire"})
	}
	seed(batch...)
	out, errOut, err = run(eventsOptions{typePrefix: "wire.page"})
	if err != nil || len(lines(out)) != 1000 {
		t.Fatalf("page: %d lines, err %v; want 1000", len(lines(out)), err)
	}
	lastOnPage := strings.Fields(lines(out)[999])[0]
	if want := fmt.Sprintf("--after %s --type wire.page", lastOnPage); !strings.Contains(errOut, want) {
		t.Fatalf("continuation hint %q should say %q", errOut, want)
	}

	// Following: an event stored after the stream opened is printed while the
	// stream is still open, and ending the daemon's streams ends the command
	// with the cursor to resume from.
	live := &wireBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- streamEvents(t.Context(), live, &bytes.Buffer{}, eventsOptions{follow: true, typePrefix: "live."})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(live.String(), "live.event") && time.Now().Before(deadline) {
		srv.RecordEvent(store.Event{Type: "live.event", UserID: "usr_wire"})
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(live.String(), "live.event") {
		t.Fatal("a followed stream never printed an event stored after it opened")
	}
	srv.EndEventStreams()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "resume with: bhatti events --follow --after ") || !strings.Contains(err.Error(), "--type live.") {
			t.Fatalf("follow ended with %v, want a resume hint", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stream did not end when the daemon ended its streams")
	}
}
