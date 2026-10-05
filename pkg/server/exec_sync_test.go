package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sahil-shubham/bhatti/pkg/engine"
)

type syncExecEngine struct {
	*mockEngine
	bufferedCalls int
	streamCalls   int
}

func (m *syncExecEngine) ExecWithSync(_ context.Context, _ string, _ []string, sync bool) (engine.ExecResult, error) {
	if !sync {
		return engine.ExecResult{}, engine.ErrNotSupported
	}
	m.bufferedCalls++
	return engine.ExecResult{Stdout: "flushed"}, nil
}

func (m *syncExecEngine) ExecStreamWithSync(_ context.Context, _ string, _ []string, sync bool, onEvent func(engine.StreamEvent)) error {
	if !sync {
		return engine.ErrNotSupported
	}
	m.streamCalls++
	code := 0
	onEvent(engine.StreamEvent{Type: "stdout", Data: "flushed"})
	onEvent(engine.StreamEvent{Type: "exit", ExitCode: &code})
	return nil
}

func TestHTTPExecSyncRoutesBufferedAndStreamOnlyOnOptIn(t *testing.T) {
	srv, ts := setup(t)
	sb := createSandbox(t, ts, "sync-request")
	base := srv.engine.(*mockEngine)
	base.ExecResult = engine.ExecResult{Stdout: "ordinary"}
	syncEngine := &syncExecEngine{mockEngine: base}
	srv.engine = syncEngine
	endpoint := "/sandboxes/" + sb.ID + "/exec"

	resp := doReq(t, ts, http.MethodPost, endpoint, map[string]any{"cmd": []string{"true"}})
	var result engine.ExecResult
	decodeJSON(t, resp, &result)
	if resp.StatusCode != http.StatusOK || result.Stdout != "ordinary" || syncEngine.bufferedCalls != 0 {
		t.Fatalf("default exec = %d %+v, sync calls=%d", resp.StatusCode, result, syncEngine.bufferedCalls)
	}
	resp = doReq(t, ts, http.MethodPost, endpoint, map[string]any{"cmd": []string{"true"}, "sync": true})
	decodeJSON(t, resp, &result)
	if resp.StatusCode != http.StatusOK || result.Stdout != "flushed" || syncEngine.bufferedCalls != 1 {
		t.Fatalf("synchronized exec = %d %+v, sync calls=%d", resp.StatusCode, result, syncEngine.bufferedCalls)
	}

	body, _ := json.Marshal(map[string]any{"cmd": []string{"true"}, "sync": true})
	req, err := http.NewRequest(http.MethodPost, ts.URL+endpoint, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testAPIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/x-ndjson")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || syncEngine.streamCalls != 1 ||
		!strings.Contains(string(data), `"data":"flushed"`) || !strings.Contains(string(data), `"type":"exit"`) {
		t.Fatalf("streamed sync = %d %q, calls=%d, err=%v", resp.StatusCode, data, syncEngine.streamCalls, err)
	}
}

func TestHTTPExecSyncRefusesOldOrUnsupportedAgentBeforeRunning(t *testing.T) {
	srv, ts := setup(t)
	sb := createSandbox(t, ts, "sync-refused")
	base := srv.engine.(*mockEngine)
	endpoint := "/sandboxes/" + sb.ID + "/exec"

	resp := doReq(t, ts, http.MethodPost, endpoint, map[string]any{"cmd": []string{"true"}, "sync": true})
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("engine without opt-in sync returned %d, want 501", resp.StatusCode)
	}
	resp.Body.Close()

	syncEngine := &syncExecEngine{mockEngine: base}
	srv.engine = syncEngine
	base.GuestFeatureErr = engine.GuestAgentOutdated("exec_sync")
	resp = doReq(t, ts, http.MethodPost, endpoint, map[string]any{"cmd": []string{"true"}, "sync": true})
	if got := errorBody(t, resp); resp.StatusCode != http.StatusConflict || !strings.Contains(got, "recreate it") || syncEngine.bufferedCalls != 0 {
		t.Fatalf("old lohar = %d %q, calls=%d; want 409 without command", resp.StatusCode, got, syncEngine.bufferedCalls)
	}
	resp = doReq(t, ts, http.MethodPost, endpoint, map[string]any{"cmd": []string{"true"}, "sync": true, "detach": true})
	if resp.StatusCode != http.StatusBadRequest || syncEngine.bufferedCalls != 0 {
		t.Fatalf("detached sync = %d, calls=%d; want 400 without command", resp.StatusCode, syncEngine.bufferedCalls)
	}
	resp.Body.Close()
}
