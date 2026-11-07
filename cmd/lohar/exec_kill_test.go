//go:build linux

package main

import (
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

// The KILL path (a host frame, an EXEC_KILL, an idle timer) runs on a different
// goroutine from the one that collects the child's exit. These tests drive each
// kill path against the real handler with a live child, so under -race any
// state the two goroutines share without synchronisation is reported, and
// without it they still pin what the host sees: the group dies and the exit
// status is the kernel's.

// hangingShell backgrounds a sleep, prints its PID and waits for it.
var hangingShell = []string{"sh", "-c", "sleep 3600 & echo $!; wait"}

// readBackgroundPID returns the PID that hangingShell prints, read from STDOUT
// frames up to the first newline.
func readBackgroundPID(t *testing.T, conn net.Conn) int {
	t.Helper()
	var out strings.Builder
	for {
		kind, payload, err := proto.ReadFrame(conn)
		if err != nil {
			t.Fatalf("read frame: %v (output so far %q)", err, out.String())
		}
		switch kind {
		case proto.STDOUT:
			out.Write(payload)
			if line, _, ok := strings.Cut(out.String(), "\n"); ok {
				pid, err := strconv.Atoi(strings.TrimSpace(line))
				if err != nil {
					t.Fatalf("background PID %q: %v", line, err)
				}
				return pid
			}
		case proto.EXIT, proto.ERROR:
			t.Fatalf("frame 0x%02x before the PID: %q", kind, payload)
		}
	}
}

// readExit returns the status of the EXIT frame, skipping everything before it.
func readExit(t *testing.T, conn net.Conn) int32 {
	t.Helper()
	for {
		kind, payload, err := proto.ReadFrame(conn)
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		switch kind {
		case proto.EXIT:
			code, ok := proto.ParseExitCode(payload)
			if !ok {
				t.Fatal("bad exit payload")
			}
			return code
		case proto.ERROR:
			t.Fatalf("agent error: %s", payload)
		}
	}
}

func readSessionInfo(t *testing.T, conn net.Conn) proto.SessionInfo {
	t.Helper()
	kind, payload, err := proto.ReadFrame(conn)
	if err != nil || kind != proto.SESSION_INFO {
		t.Fatalf("first frame 0x%02x, err %v; want SESSION_INFO", kind, err)
	}
	var info proto.SessionInfo
	if err := json.Unmarshal(payload, &info); err != nil {
		t.Fatalf("session info: %v", err)
	}
	return info
}

// waitSessionGone fails the test unless the session leaves the registry: a
// client that was attached for EXIT leaves nothing behind.
func waitSessionGone(t *testing.T, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for getSession(id) != nil {
		if time.Now().After(deadline) {
			removeSession(id)
			t.Fatalf("session %s still registered after EXIT", id)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPipedExecKillEndsProcessGroup(t *testing.T) {
	requireGuestExec(t)
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	host.SetDeadline(time.Now().Add(15 * time.Second))

	ended := make(chan struct{})
	go func() {
		defer close(ended)
		handlePipedExec(guest, proto.ExecRequest{Argv: hangingShell})
	}()

	sleeper := readBackgroundPID(t, host)
	if err := proto.WriteFrame(host, proto.KILL, nil); err != nil {
		t.Fatalf("write KILL: %v", err)
	}
	if code := readExit(t, host); code != 128+int32(syscall.SIGKILL) {
		t.Errorf("exit code %d, want %d (128+SIGKILL)", code, 128+int32(syscall.SIGKILL))
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after EXIT")
	}
	waitProcessGone(t, sleeper)
}

// Session KILL is SIGTERM to the group, so the shell dies by that signal.
func TestSessionKillEndsProcessGroup(t *testing.T) {
	requireGuestExec(t)
	for _, tc := range []struct {
		name    string
		handler func(net.Conn, proto.ExecRequest)
	}{
		{"piped", handlePipedSession},
		{"PTY", handleTTYSession},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, guest := net.Pipe()
			defer host.Close()
			defer guest.Close()
			host.SetDeadline(time.Now().Add(15 * time.Second))

			go tc.handler(guest, proto.ExecRequest{Argv: hangingShell})

			info := readSessionInfo(t, host)
			sleeper := readBackgroundPID(t, host)
			if err := proto.WriteFrame(host, proto.KILL, nil); err != nil {
				t.Fatalf("write KILL: %v", err)
			}
			if code := readExit(t, host); code != 128+int32(syscall.SIGTERM) {
				t.Errorf("exit code %d, want %d (128+SIGTERM)", code, 128+int32(syscall.SIGTERM))
			}
			waitProcessGone(t, sleeper)
			waitSessionGone(t, info.SessionID)
		})
	}
}

// EXEC_KILL names the session from a connection of its own, and is SIGKILL.
func TestExecKillFrameEndsSession(t *testing.T) {
	requireGuestExec(t)
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	host.SetDeadline(time.Now().Add(15 * time.Second))
	go handlePipedSession(guest, proto.ExecRequest{Argv: hangingShell})

	info := readSessionInfo(t, host)
	sleeper := readBackgroundPID(t, host)

	killHost, killGuest := net.Pipe()
	defer killHost.Close()
	defer killGuest.Close()
	killHost.SetDeadline(time.Now().Add(15 * time.Second))
	go handleControlConnection(killGuest)
	if err := proto.SendJSON(killHost, proto.EXEC_KILL, map[string]string{"session_id": info.SessionID}); err != nil {
		t.Fatalf("send EXEC_KILL: %v", err)
	}
	if kind, _, err := proto.ReadFrame(killHost); err != nil || kind != proto.EXIT {
		t.Fatalf("EXEC_KILL reply 0x%02x, err %v; want EXIT", kind, err)
	}

	if code := readExit(t, host); code != 128+int32(syscall.SIGKILL) {
		t.Errorf("exit code %d, want %d (128+SIGKILL)", code, 128+int32(syscall.SIGKILL))
	}
	waitProcessGone(t, sleeper)
	waitSessionGone(t, info.SessionID)
}

// A session whose client went away is killed MaxIdle later.
func TestSessionIdleTimerKillsDetachedSession(t *testing.T) {
	requireGuestExec(t)
	host, guest := net.Pipe()
	defer guest.Close()
	host.SetDeadline(time.Now().Add(15 * time.Second))

	idle := 1
	go handlePipedSession(guest, proto.ExecRequest{Argv: hangingShell, MaxIdleSec: &idle})

	info := readSessionInfo(t, host)
	defer removeSession(info.SessionID)
	sleeper := readBackgroundPID(t, host)
	host.Close()

	sess := getSession(info.SessionID)
	if sess == nil {
		t.Fatalf("session %s not registered", info.SessionID)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		sess.mu.Lock()
		code := sess.ExitCode
		sess.mu.Unlock()
		if code != nil {
			if want := 128 + int(syscall.SIGKILL); *code != want {
				t.Errorf("idle kill exit code %d, want %d (128+SIGKILL)", *code, want)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("detached session still running after MaxIdle")
		}
		time.Sleep(20 * time.Millisecond)
	}
	waitProcessGone(t, sleeper)
}
