package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/sahil-shubham/bhatti/pkg/agent"
	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// smokeEngine gives the mock engine the optional capabilities the smoke test
// drives: files that live in memory, and a piped session that replays a canned
// run of the command it is given.
type smokeEngine struct {
	*mockEngine
	filesMu sync.Mutex
	files   map[string][]byte
}

func (e *smokeEngine) FileWrite(_ context.Context, _, path, _ string, size int64, r io.Reader) error {
	data, err := io.ReadAll(io.LimitReader(r, size))
	if err != nil {
		return err
	}
	e.filesMu.Lock()
	defer e.filesMu.Unlock()
	e.files[path] = data
	return nil
}

func (e *smokeEngine) FileRead(_ context.Context, _, path string, w io.Writer, _ ...agent.FileReadOpts) (int64, string, error) {
	e.filesMu.Lock()
	data, ok := e.files[path]
	e.filesMu.Unlock()
	if !ok {
		return 0, "", agent.ErrFileNotFound
	}
	n, err := w.Write(data)
	return int64(n), "0644", err
}

func (e *smokeEngine) FileStat(_ context.Context, _, path string) (*proto.FileInfo, error) {
	e.filesMu.Lock()
	defer e.filesMu.Unlock()
	data, ok := e.files[path]
	if !ok {
		return nil, agent.ErrFileNotFound
	}
	return &proto.FileInfo{Name: path, Size: int64(len(data)), Mode: "0644"}, nil
}

func (e *smokeEngine) FileList(context.Context, string, string) ([]proto.FileInfo, error) {
	e.filesMu.Lock()
	defer e.filesMu.Unlock()
	var out []proto.FileInfo
	for path, data := range e.files {
		out = append(out, proto.FileInfo{Name: path, Size: int64(len(data)), Mode: "0644"})
	}
	return out, nil
}

func (e *smokeEngine) PipedSession(_ context.Context, _ string, spec engine.PipedSpec) (*proto.SessionInfo, engine.PipedConn, error) {
	exit := proto.ExitPayload(0)
	return &proto.SessionInfo{SessionID: "sess-1"}, &fakePiped{frames: [][2][]byte{
		{{proto.STDOUT}, []byte("ran " + strings.Join(spec.Cmd, " "))},
		{{proto.EXIT}, exit[:]},
	}}, nil
}

func (e *smokeEngine) PipedSessionAttach(context.Context, string, string, bool) (*proto.SessionInfo, engine.PipedConn, error) {
	return nil, nil, errors.New("no such session")
}

// The API's main path, over real HTTP and through the whole chain: create, exec
// (buffered and streamed), files, the events stream, a websocket upgrade, and
// destroy. Streaming and hijacking are the parts a wrapped ResponseWriter can
// break.
func TestAPISmokeThroughTheChain(t *testing.T) {
	srv, ts := eventsServer(t, true)
	srv.engine = &smokeEngine{mockEngine: srv.engine.(*mockEngine), files: map[string][]byte{}}

	follow := openFollow(t, ts, testAPIKey, "?follow=true")
	eventOf := func(typ string) eventRecord {
		t.Helper()
		for {
			if r := follow.next(); r["type"] == typ {
				return r
			}
		}
	}
	requestID := func(resp *http.Response) {
		t.Helper()
		if !requestIDRe.MatchString(resp.Header.Get("X-Request-ID")) {
			t.Errorf("%s response has no request ID: %q", resp.Request.URL.Path, resp.Header.Get("X-Request-ID"))
		}
	}
	do := func(method, path string, headers map[string]string, body io.Reader) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, ts.URL+path, body)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+testAPIKey)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := testHTTPClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		requestID(resp)
		return resp
	}
	want := func(resp *http.Response, status int) []byte {
		t.Helper()
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("%s %s: reading the body: %v", resp.Request.Method, resp.Request.URL.Path, err)
		}
		if resp.StatusCode != status {
			t.Fatalf("%s %s = %d %s, want %d", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, body, status)
		}
		return body
	}

	// Create.
	var sb struct{ ID, Name string }
	json.Unmarshal(want(do("POST", "/sandboxes", nil, strings.NewReader(`{"name":"smoke"}`)), 201), &sb)
	if sb.ID == "" || sb.Name != "smoke" {
		t.Fatalf("created sandbox = %+v", sb)
	}
	if created := eventOf("sandbox.created"); created["sandbox_id"] != sb.ID {
		t.Fatalf("created event = %v, want sandbox %s", created, sb.ID)
	}

	// Exec, buffered and streamed, by name.
	var result engine.ExecResult
	json.Unmarshal(want(do("POST", "/sandboxes/smoke/exec", nil, strings.NewReader(`{"cmd":["echo","hi"]}`)), 200), &result)
	if result.ExitCode != 0 || result.Stdout != "mock output\n" {
		t.Fatalf("exec = %+v", result)
	}
	resp := do("POST", "/sandboxes/"+sb.ID+"/exec", map[string]string{"Accept": "application/x-ndjson"}, strings.NewReader(`{"cmd":["echo","hi"]}`))
	if ct := resp.Header.Get("Content-Type"); resp.StatusCode != 200 || ct != "application/x-ndjson" {
		t.Fatalf("streamed exec = %d (%s)", resp.StatusCode, ct)
	}
	var kinds []string
	for lines := bufio.NewScanner(resp.Body); lines.Scan(); {
		var ev struct{ Type string }
		json.Unmarshal(lines.Bytes(), &ev)
		kinds = append(kinds, ev.Type)
	}
	resp.Body.Close()
	if strings.Join(kinds, ",") != "stdout,exit" {
		t.Fatalf("streamed exec events = %v, want stdout then exit", kinds)
	}

	// Files: write, read back, stat, list.
	want(do("PUT", "/sandboxes/smoke/files?path=/work/a.txt", nil, strings.NewReader("hello\n")), 200)
	resp = do("GET", "/sandboxes/smoke/files?path=/work/a.txt", nil, nil)
	if got := want(resp, 200); string(got) != "hello\n" || resp.Header.Get("X-File-Size") != "6" {
		t.Fatalf("read back %q (X-File-Size %q), want the 6 bytes written", got, resp.Header.Get("X-File-Size"))
	}
	resp = do("HEAD", "/sandboxes/smoke/files?path=/work/a.txt", nil, nil)
	want(resp, 200)
	if resp.Header.Get("X-File-Size") != "6" || resp.Header.Get("X-File-Mode") != "0644" {
		t.Fatalf("stat headers = %v", resp.Header)
	}
	var listing []proto.FileInfo
	json.Unmarshal(want(do("GET", "/sandboxes/smoke/files?path=/work&ls=true", nil, nil), 200), &listing)
	if len(listing) != 1 || listing[0].Name != "/work/a.txt" {
		t.Fatalf("listing = %+v", listing)
	}
	want(do("GET", "/sandboxes/smoke/files?path=/work/missing", nil, nil), 404)

	// A websocket upgrade through the chain: the connection is hijacked, and
	// frames flow both ways over it.
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/sandboxes/" + sb.ID + "/exec/ws"
	ws, upgrade, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer " + testAPIKey}})
	if err != nil {
		t.Fatalf("exec ws upgrade: %v", err)
	}
	defer ws.Close()
	if upgrade.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("exec ws upgrade = %d", upgrade.StatusCode)
	}
	if err := ws.WriteJSON(map[string]any{"cmd": []string{"echo", "hi"}}); err != nil {
		t.Fatal(err)
	}
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var frames []string
	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			if !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
				t.Fatalf("exec ws ended with %v after %q", err, frames)
			}
			break
		}
		frames = append(frames, string(msg))
	}
	if len(frames) != 3 || !strings.Contains(frames[0], `"session_id":"sess-1"`) || frames[1] != "ran echo hi" || !strings.Contains(frames[2], `"type":"exit"`) {
		t.Fatalf("exec ws frames = %q, want the session, the output and the exit", frames)
	}
	if piped := eventOf("exec.piped_session"); piped["sandbox_id"] != sb.ID {
		t.Fatalf("piped session event = %v", piped)
	}

	// Destroy, and the sandbox is gone.
	if body := want(do("DELETE", "/sandboxes/smoke", nil, nil), 200); !strings.Contains(string(body), "destroyed") {
		t.Fatalf("destroy answered %s", body)
	}
	if destroyed := eventOf("sandbox.destroyed"); destroyed["sandbox_id"] != sb.ID {
		t.Fatalf("destroyed event = %v", destroyed)
	}
	want(do("GET", "/sandboxes/smoke", nil, nil), 404)
}
