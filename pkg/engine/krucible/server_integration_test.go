//go:build krucible

package krucible

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sahil-shubham/bhatti/pkg/engine"
	"github.com/sahil-shubham/bhatti/pkg/server"
	"github.com/sahil-shubham/bhatti/pkg/store"
)

// TestKrucibleServerIntegration drives the real HTTP API and store. Exec
// against a powered-off sandbox auto-boots from disk through the server's
// ensureHot -> EnsureHot -> Start path.
func TestKrucibleServerIntegration(t *testing.T) {
	_, do, _ := krucibleServer(t, newBlockRootEngine) // skips without VM prerequisites

	// --- create over HTTP ---
	resp := do("POST", "/sandboxes", map[string]any{"name": "srv-it", "memory_mb": 512})
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create: want 201, got %d: %s", resp.StatusCode, b)
	}
	var sb store.Sandbox
	json.NewDecoder(resp.Body).Decode(&sb)
	resp.Body.Close()
	t.Cleanup(func() { do("DELETE", "/sandboxes/"+sb.ID, nil) })

	exec := func(want string, cmd ...string) {
		t.Helper()
		resp := do("POST", "/sandboxes/"+sb.ID+"/exec", map[string]any{"cmd": cmd})
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("exec %v: want 200, got %d: %s", cmd, resp.StatusCode, b)
		}
		var res engine.ExecResult
		json.NewDecoder(resp.Body).Decode(&res)
		if !strings.Contains(res.Stdout, want) {
			t.Fatalf("exec %v: stdout %q does not contain %q", cmd, res.Stdout, want)
		}
	}

	t.Run("ExecHot", func(t *testing.T) { exec("srv-hello", "echo", "srv-hello") })

	t.Run("ColdStop", func(t *testing.T) {
		resp := do("POST", "/sandboxes/"+sb.ID+"/stop", nil)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("stop: want 200, got %d", resp.StatusCode)
		}
	})

	// Exec against a powered-off sandbox auto-boots without an explicit /start.
	t.Run("ExecAutoWakesFromCold", func(t *testing.T) { exec("woke-cold", "echo", "woke-cold") })
}

// The real server's idle clock, Activity query, PAUSE and cold Stop must be
// durable as a single lifecycle: a write just before idle cooling survives
// power-off, including the final guest sync after RESUME.
func TestKrucibleServerThermalDurability(t *testing.T) {
	var eng *Engine
	srv, do, _ := krucibleServer(t, func(t *testing.T) engine.Engine {
		eng = newPauseEngine(t).(*Engine)
		return eng
	})
	resp := do("POST", "/sandboxes", map[string]any{"name": "thermal-durable", "memory_mb": 512})
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create: status=%d body=%s", resp.StatusCode, body)
	}
	var sb store.Sandbox
	err := json.NewDecoder(resp.Body).Decode(&sb)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { do("DELETE", "/sandboxes/"+sb.ID, nil).Body.Close() })

	if err := srv.StartThermalManager(server.ThermalConfig{
		WarmTimeout: time.Millisecond, ColdTimeout: time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}
	// The manager ticks every 10s. Write close to its first idle decision,
	// rather than letting background writeback persist a file long beforehand.
	time.Sleep(6 * time.Second)
	resp = do("POST", "/sandboxes/"+sb.ID+"/exec",
		map[string]any{"cmd": []string{"writeuid", "/workspace/thermal-last-write"}})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("last write: status=%d body=%s", resp.StatusCode, body)
	}
	var write engine.ExecResult
	err = json.NewDecoder(resp.Body).Decode(&write)
	resp.Body.Close()
	if err != nil || write.ExitCode != 0 {
		t.Fatalf("last write: %+v, %v", write, err)
	}
	prePause, err := eng.Exec(context.Background(), sb.EngineID, []string{"cat", "/workspace/thermal-last-write"})
	if err != nil || prePause.ExitCode != 0 || prePause.Stdout != "1000" {
		t.Fatalf("file was not written before thermal pause: %+v, %v", prePause, err)
	}

	deadline := time.Now().Add(40 * time.Second) // includes a delayed first idle tick
	var warmAt time.Time
	for time.Now().Before(deadline) {
		resp = do("GET", "/sandboxes", nil)
		var listed []struct {
			store.Sandbox
			Thermal string `json:"thermal"`
		}
		err = json.NewDecoder(resp.Body).Decode(&listed)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || err != nil {
			t.Fatalf("list during cooling: status=%d err=%v", resp.StatusCode, err)
		}
		if len(listed) != 1 || listed[0].ID != sb.ID {
			t.Fatalf("thermal sandbox disappeared: %+v", listed)
		}
		switch {
		case listed[0].Status == "running" && listed[0].Thermal == "warm":
			if warmAt.IsZero() {
				warmAt = time.Now()
			}
		case listed[0].Status == "stopped":
			if warmAt.IsZero() {
				t.Fatal("server cold-stopped sandbox without observing a warm pause")
			}
			if elapsed := time.Since(warmAt); elapsed > 17*time.Second {
				t.Fatalf("warm→cold took %s, expected bounded guest resync", elapsed)
			}
			resp = do("POST", "/sandboxes/"+sb.ID+"/exec",
				map[string]any{"cmd": []string{"cat", "/workspace/thermal-last-write"}})
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				t.Fatalf("wake from cold: status=%d body=%s", resp.StatusCode, body)
			}
			var read engine.ExecResult
			err = json.NewDecoder(resp.Body).Decode(&read)
			resp.Body.Close()
			if err != nil || read.ExitCode != 0 || read.Stdout != "1000" {
				t.Fatalf("thermal cold stop lost last write: %+v, err=%v", read, err)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("server never completed the real hot→warm→cold transition")
}

// The HTTP manual stop also resumes and syncs a warm VM before powering it off.
func TestKrucibleServerManualWarmStop(t *testing.T) {
	var eng *Engine
	_, do, _ := krucibleServer(t, func(t *testing.T) engine.Engine {
		eng = newPauseEngine(t).(*Engine)
		return eng
	})
	resp := do("POST", "/sandboxes", map[string]any{"name": "manual-warm-stop", "memory_mb": 512})
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("create: status=%d body=%s", resp.StatusCode, body)
	}
	var sb store.Sandbox
	err := json.NewDecoder(resp.Body).Decode(&sb)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { do("DELETE", "/sandboxes/"+sb.ID, nil).Body.Close() })
	resp = do("POST", "/sandboxes/"+sb.ID+"/exec",
		map[string]any{"cmd": []string{"writeuid", "/workspace/manual-warm-write"}})
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("write: status=%d body=%s", resp.StatusCode, body)
	}
	var write engine.ExecResult
	err = json.NewDecoder(resp.Body).Decode(&write)
	resp.Body.Close()
	if err != nil || write.ExitCode != 0 {
		t.Fatalf("write: %+v, %v", write, err)
	}
	prePause, err := eng.Exec(context.Background(), sb.EngineID, []string{"cat", "/workspace/manual-warm-write"})
	if err != nil || prePause.ExitCode != 0 || prePause.Stdout != "1000" {
		t.Fatalf("file was not written before manual pause: %+v, %v", prePause, err)
	}
	if err := eng.Pause(context.Background(), sb.EngineID); err != nil {
		t.Fatalf("pause before manual stop: %v", err)
	}
	start := time.Now()
	resp = do("POST", "/sandboxes/"+sb.ID+"/stop", nil)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		resp.Body.Close()
		t.Fatalf("manual warm stop exceeded bounded guest resync: %s", elapsed)
	}
	var stopped store.Sandbox
	err = json.NewDecoder(resp.Body).Decode(&stopped)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || err != nil || stopped.Status != "stopped" {
		t.Fatalf("manual warm stop status=%d body=%+v err=%v", resp.StatusCode, stopped, err)
	}
	resp = do("POST", "/sandboxes/"+sb.ID+"/exec",
		map[string]any{"cmd": []string{"cat", "/workspace/manual-warm-write"}})
	var read engine.ExecResult
	err = json.NewDecoder(resp.Body).Decode(&read)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || err != nil || read.ExitCode != 0 || read.Stdout != "1000" {
		t.Fatalf("manual warm stop lost write: status=%d result=%+v err=%v", resp.StatusCode, read, err)
	}
}

// doFunc issues an authenticated request against the krucible-backed test daemon.
type doFunc func(method, path string, body any) *http.Response

// krucibleServer stands up the HTTP API and store over a real krucible
// block-root engine. It returns the base URL for WebSocket upgrades; callers
// may start its thermal manager explicitly. Skips without VM prerequisites.
func krucibleServer(t *testing.T, newEngine func(*testing.T) engine.Engine) (*server.Server, doFunc, string) {
	t.Helper()
	eng := newEngine(t)
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	const apiKey = "test-token"
	sum := sha256.Sum256([]byte(apiKey))
	if err := st.CreateUser(store.User{
		ID: "usr_test", Name: "test-user", APIKeyHash: hex.EncodeToString(sum[:]),
		MaxSandboxes: 50, MaxCPUsPerSandbox: 4, MaxMemoryMBPerSandbox: 4096,
		SubnetIndex: 1, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	srv := server.New(eng, st, dir)
	ts := httptest.NewServer(srv)
	t.Cleanup(func() { srv.Close(); ts.Close() })

	do := func(method, path string, body any) *http.Response {
		t.Helper()
		var br io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			br = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, ts.URL+path, br)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return resp
	}
	return srv, do, ts.URL
}

// TestKrucibleServerForward drives the authenticated WebSocket tunnel through
// the full daemon and a real VM: detached guest HTTP server -> GET /tunnel
// upgrade -> binary HTTP request/response over the guest connection.
func TestKrucibleServerForward(t *testing.T) {
	_, do, serverURL := krucibleServer(t, newBlockRootEngine)

	resp := do("POST", "/sandboxes", map[string]any{"name": "fwd-srv"})
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create: want 201, got %d: %s", resp.StatusCode, b)
	}
	var sb store.Sandbox
	json.NewDecoder(resp.Body).Decode(&sb)
	resp.Body.Close()
	t.Cleanup(func() { do("DELETE", "/sandboxes/"+sb.ID, nil).Body.Close() })

	// High port: under TSI the guest shares the host's port namespace, so
	// avoid host-occupied ports. Detached exec may finish before serve listens.
	resp = do("POST", "/sandboxes/"+sb.ID+"/exec", map[string]any{
		"cmd": []string{"/bin/netcheck", "serve", "18080"}, "detach": true, "output_file": "/tmp/serve.log",
	})
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("detached exec: want 200, got %d: %s", resp.StatusCode, b)
	}
	resp.Body.Close()

	wsURL := "ws" + strings.TrimPrefix(serverURL, "http") + "/sandboxes/" + sb.ID + "/tunnel?port=18080"
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	header := http.Header{"Authorization": {"Bearer test-token"}}
	fetch := func() (string, error) {
		ws, upgrade, err := dialer.Dial(wsURL, header)
		if err != nil {
			if upgrade != nil {
				upgrade.Body.Close()
			}
			return "", err
		}
		defer ws.Close()
		if err := ws.WriteMessage(websocket.BinaryMessage, []byte("GET / HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n")); err != nil {
			return "", err
		}
		ws.SetReadDeadline(time.Now().Add(3 * time.Second))
		var raw bytes.Buffer
		for {
			kind, chunk, err := ws.ReadMessage()
			if err != nil {
				if raw.Len() == 0 {
					return "", fmt.Errorf("read tunnel: %w", err)
				}
				break
			}
			if kind != websocket.BinaryMessage {
				return "", fmt.Errorf("unexpected tunnel frame %d", kind)
			}
			raw.Write(chunk)
		}
		reply, err := http.ReadResponse(bufio.NewReader(&raw), &http.Request{Method: http.MethodGet})
		if err != nil {
			return "", err
		}
		defer reply.Body.Close()
		body, err := io.ReadAll(reply.Body)
		if err != nil {
			return "", err
		}
		if reply.StatusCode != http.StatusOK {
			return "", fmt.Errorf("guest HTTP status %d: %s", reply.StatusCode, body)
		}
		return string(body), nil
	}

	deadline := time.Now().Add(25 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		body, err := fetch()
		if err == nil && strings.Contains(body, "hello-from-guest") {
			return
		}
		if err != nil {
			last = err.Error()
		} else {
			last = body
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("WebSocket tunnel never returned hello-from-guest (last: %q)", last)
}

// TestKrucibleServerFork exercises `create --from` end-to-end (Phase 2 #4):
// POST /sandboxes with {from} forks a running sandbox via the engine's Fork
// capability; the fork is a distinct, working, independent VM.
func TestKrucibleServerFork(t *testing.T) {
	_, do, _ := krucibleServer(t, newCheckpointEngine) // skips if libkrun/vmm/mke2fs unavailable

	resp := do("POST", "/sandboxes", map[string]any{"name": "fork-src", "memory_mb": 512})
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create source: want 201, got %d: %s", resp.StatusCode, b)
	}
	var src store.Sandbox
	json.NewDecoder(resp.Body).Decode(&src)
	resp.Body.Close()
	t.Cleanup(func() { do("DELETE", "/sandboxes/"+src.ID, nil) })

	// Fork by name via create --from.
	resp = do("POST", "/sandboxes", map[string]any{"name": "fork-child", "from": "fork-src"})
	if resp.StatusCode != 201 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("fork: want 201, got %d: %s", resp.StatusCode, b)
	}
	var child store.Sandbox
	json.NewDecoder(resp.Body).Decode(&child)
	resp.Body.Close()
	t.Cleanup(func() { do("DELETE", "/sandboxes/"+child.ID, nil) })
	if child.ID == src.ID {
		t.Fatal("fork returned the source sandbox, not a new one")
	}

	// The fork is a working, independent VM.
	resp = do("POST", "/sandboxes/"+child.ID+"/exec", map[string]any{"cmd": []string{"echo", "fork-alive"}})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("exec on fork: want 200, got %d: %s", resp.StatusCode, b)
	}
	var res engine.ExecResult
	json.NewDecoder(resp.Body).Decode(&res)
	if !strings.Contains(res.Stdout, "fork-alive") {
		t.Fatalf("exec on fork: stdout %q lacks fork-alive", res.Stdout)
	}
}
