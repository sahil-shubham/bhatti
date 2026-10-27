package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

// slowExecEngine behaves like the krucible engine under a deadline: the command
// never finishes, and when the exec deadline passes the agent read fails with an
// i/o timeout that carries no context error.
type slowExecEngine struct {
	*mockEngine
	stdout   string // streamed before hanging, if set
	failWith error  // streamed exec fails at once with this instead of hanging
}

func (e *slowExecEngine) hang(ctx context.Context) error {
	<-ctx.Done()
	if _, ok := ctx.Deadline(); ok && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("agent read: %w", os.ErrDeadlineExceeded)
	}
	return fmt.Errorf("agent read: %w", ctx.Err())
}

func (e *slowExecEngine) Exec(ctx context.Context, _ string, _ []string) (engine.ExecResult, error) {
	return engine.ExecResult{}, e.hang(ctx)
}

func (e *slowExecEngine) ExecStream(ctx context.Context, _ string, _ []string, onEvent func(engine.StreamEvent)) error {
	if e.failWith != nil {
		return e.failWith
	}
	if e.stdout != "" {
		onEvent(engine.StreamEvent{Type: "stdout", Data: e.stdout})
	}
	return e.hang(ctx)
}

func streamExec(t *testing.T, tsURL, id string, body map[string]any) (*http.Response, []engine.StreamEvent) {
	t.Helper()
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, tsURL+"/sandboxes/"+id+"/exec", bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	resp, err := testHTTPClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var events []engine.StreamEvent
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var ev engine.StreamEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			t.Fatalf("bad NDJSON line %q: %v", sc.Text(), err)
		}
		events = append(events, ev)
	}
	return resp, events
}

const oneSecondTimeoutMsg = "exec timed out after 1s (use --timeout, up to 86400s)"

func TestHTTPExecTimeoutBufferedIs504NamingLimit(t *testing.T) {
	srv, ts := setup(t)
	sb := createSandbox(t, ts, "exec-timeout-buffered")
	srv.engine = &slowExecEngine{mockEngine: srv.engine.(*mockEngine)}

	resp := doReq(t, ts, http.MethodPost, "/sandboxes/"+sb.ID+"/exec", map[string]any{"cmd": []string{"sleep", "999"}, "timeout_sec": 1})
	var body map[string]string
	decodeJSON(t, resp, &body)
	if resp.StatusCode != http.StatusGatewayTimeout || body["error"] != oneSecondTimeoutMsg {
		t.Fatalf("exec past its timeout = %d %v; want 504 %q", resp.StatusCode, body, oneSecondTimeoutMsg)
	}
}

// The 200 is already sent when the stream starts, so the timeout is the last
// event; the client gets no exit event, only the error naming the limit.
func TestHTTPExecTimeoutStreamEndsWithErrorEvent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stdout string
		want   int // events before the timeout error
	}{
		{"silent", "", 0},
		{"after output", "building...\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, ts := setup(t)
			sb := createSandbox(t, ts, "exec-timeout-stream")
			srv.engine = &slowExecEngine{mockEngine: srv.engine.(*mockEngine), stdout: tc.stdout}

			resp, events := streamExec(t, ts.URL, sb.ID, map[string]any{"cmd": []string{"sleep", "999"}, "timeout_sec": 1})
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			if len(events) != tc.want+1 {
				t.Fatalf("events = %+v; want %d output events then one error", events, tc.want)
			}
			if tc.want == 1 && (events[0].Type != "stdout" || events[0].Data != tc.stdout) {
				t.Fatalf("first event = %+v; want the output", events[0])
			}
			last := events[len(events)-1]
			if last.Type != "error" || last.Data != oneSecondTimeoutMsg {
				t.Fatalf("last event = %+v; want error %q", last, oneSecondTimeoutMsg)
			}
		})
	}
}

// A stream that fails for any other reason must still end in an event: the
// client derives its exit status from one.
func TestHTTPExecStreamFailureEndsWithErrorEvent(t *testing.T) {
	srv, ts := setup(t)
	sb := createSandbox(t, ts, "exec-stream-failure")
	srv.engine = &slowExecEngine{mockEngine: srv.engine.(*mockEngine), failWith: errors.New("agent read: connection reset")}

	_, events := streamExec(t, ts.URL, sb.ID, map[string]any{"cmd": []string{"true"}})
	if len(events) != 1 || events[0].Type != "error" || events[0].Data != "agent read: connection reset" {
		t.Fatalf("events = %+v; want one error event", events)
	}
}

// Output cut mid-character by the guest's chunking must reach the client whole.
func TestHTTPExecStreamKeepsSplitCharactersWhole(t *testing.T) {
	srv, ts := setup(t)
	sb := createSandbox(t, ts, "exec-stream-utf8")
	srv.engine = &chunkedStreamEngine{
		mockEngine: srv.engine.(*mockEngine),
		chunks:     []string{"caf\xc3", "\xa9 \xe2\x82", "\xac!"},
	}

	_, events := streamExec(t, ts.URL, sb.ID, map[string]any{"cmd": []string{"true"}})
	var out string
	for _, ev := range events {
		if ev.Type == "stdout" {
			out += ev.Data
		}
	}
	if out != "café €!" || events[len(events)-1].Type != "exit" {
		t.Fatalf("stdout = %q, events = %+v; want %q then exit", out, events, "café €!")
	}
}

type chunkedStreamEngine struct {
	*mockEngine
	chunks []string
}

func (e *chunkedStreamEngine) ExecStream(_ context.Context, _ string, _ []string, onEvent func(engine.StreamEvent)) error {
	for _, c := range e.chunks {
		onEvent(engine.StreamEvent{Type: "stdout", Data: c})
	}
	code := 0
	onEvent(engine.StreamEvent{Type: "exit", ExitCode: &code})
	return nil
}

// A client that hangs up mid-exec isn't a timeout, however close the deadline.
func TestExecTimedOutIgnoresClientDisconnect(t *testing.T) {
	request, cancelRequest := context.WithCancel(context.Background())
	execCtx, cancelExec := context.WithTimeout(request, time.Millisecond)
	defer cancelExec()
	time.Sleep(5 * time.Millisecond) // deadline passed

	if !execTimedOut(request, execCtx) {
		t.Fatal("deadline passed with the client still connected: want timeout")
	}
	cancelRequest()
	if execTimedOut(request, execCtx) {
		t.Fatal("client disconnected: not a timeout")
	}
}
