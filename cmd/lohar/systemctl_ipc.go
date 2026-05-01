//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

// systemctlSocketPath is the in-guest UDS where PID-1 lohar listens for
// privileged systemctl operations. Created at boot, mode 0666 so any user
// can connect; the actual authorisation check uses SO_PEERCRED on the
// accepted connection (kernel-vouched caller uid, not a client-claimed one).
const systemctlSocketPath = "/run/bhatti/systemctl.sock"

// privilegedOps lists systemctl subcommands that require root and therefore
// route through PID-1 lohar over the IPC. Read-only ops (status, show,
// is-active, list-units, etc.) stay in-process \u2014 they don't need privilege
// and the IPC round-trip would be wasteful.
//
// The server uses this same set: a non-root caller asking for any of these
// gets "Access denied".
// daemon-reload and daemon-reexec are deliberately NOT in this set:
// in real systemd they require privilege because they actually do
// something (re-parse all unit files; re-execute PID 1). In our shim
// they're no-ops — we re-read unit files on every Registry.Resolve —
// so gating them on root would break the common 'systemctl
// daemon-reload' invocation that scripts and admins run without
// thinking, and our no-op path is harmless to expose.
var privilegedOps = map[string]bool{
	"start":             true,
	"stop":              true,
	"restart":           true,
	"try-restart":       true,
	"reload":            true,
	"reload-or-restart": true,
	"enable":            true,
	"disable":           true,
	"mask":              true,
	"unmask":            true,
	"kill":              true,
	"reset-failed":      true,
	"preset":            true,
}

// requiresPrivilege returns true if the given systemctl subcommand should
// be routed through the IPC daemon when not running as root.
func requiresPrivilege(op string) bool {
	return privilegedOps[op]
}

// --- Client side ---

// tryDispatchViaIPC attempts to forward a privileged op to PID-1 lohar.
// Returns (response, true) on successful round-trip; (nil, false) if no
// daemon is reachable (caller should fall back to in-process). Errors that
// indicate the daemon IS there but rejected the request are surfaced as
// (response, true) with a non-zero ExitCode and Stderr filled in.
//
// We deliberately don't pass the caller's uid in the request \u2014 the server
// reads it via SO_PEERCRED so it can't be lied about.
func tryDispatchViaIPC(req proto.SystemctlRequest) (*proto.SystemctlResponse, bool) {
	// Are we PID 1? If so, we ARE the daemon \u2014 no IPC needed; the caller
	// will fall back to in-process which runs as root anyway.
	if os.Getpid() == 1 {
		return nil, false
	}

	conn, err := net.DialTimeout("unix", systemctlSocketPath, 1*time.Second)
	if err != nil {
		return nil, false // no daemon (test env, dev host)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(60 * time.Second))

	if err := proto.SendJSON(conn, proto.SYSTEMCTL_REQ, req); err != nil {
		return nil, false
	}

	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		return nil, false
	}
	if msgType != proto.SYSTEMCTL_RESP {
		return nil, false
	}
	var resp proto.SystemctlResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		return nil, false
	}
	return &resp, true
}

// --- Server side ---

// startSystemctlListener binds the UDS at /run/bhatti/systemctl.sock and
// spawns an accept loop. Called from runAgent during boot; safe to call
// even if the socket dir doesn't exist yet (we MkdirAll first).
//
// Each accepted connection is handled in its own goroutine so a slow op
// doesn't block other systemctl invocations. Read-only paths don't reach
// here (they stay in-process), so the only ops we serve are privileged.
func startSystemctlListener() {
	os.MkdirAll("/run/bhatti", 0755)
	os.Remove(systemctlSocketPath) // stale from previous run
	ln, err := net.Listen("unix", systemctlSocketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lohar: systemctl listener: %v\n", err)
		return
	}
	// World-accessible so unprivileged clients can connect; SO_PEERCRED
	// is what actually decides who's allowed to do what.
	if err := os.Chmod(systemctlSocketPath, 0666); err != nil {
		fmt.Fprintf(os.Stderr, "lohar: chmod systemctl.sock: %v\n", err)
	}
	go acceptLoop(ln, handleSystemctlConnection)
}

// handleSystemctlConnection is the per-connection server. It:
//  1. Establishes the caller's uid via SO_PEERCRED (kernel-vouched).
//  2. Reads the request frame.
//  3. Authorises: non-root callers get "Access denied" for any op that
//     reaches this socket (read-only ops shouldn't be sent here at all).
//  4. Executes the op against a fresh Registry, capturing stdout/stderr.
//  5. Sends a SystemctlResponse back.
func handleSystemctlConnection(conn net.Conn) {
	defer conn.Close()

	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return
	}
	uid, _, ok := peerCredentials(uc)
	if !ok {
		return
	}

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msgType, payload, err := proto.ReadFrame(conn)
	conn.SetReadDeadline(time.Time{})
	if err != nil || msgType != proto.SYSTEMCTL_REQ {
		return
	}
	var req proto.SystemctlRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		writeResp(conn, proto.SystemctlResponse{
			ExitCode: 1,
			Stderr:   fmt.Sprintf("systemctl: bad request: %v\n", err),
		})
		return
	}

	if uid != 0 {
		// Match systemd's polkit-rejection wording so existing scripts and
		// parsers see what they expect. Format depends on whether the op
		// targets a specific unit:
		//   with unit:    "Failed to start ssh.service: Access denied"
		//   without unit: "Failed to <op>: Access denied"
		var msg string
		if len(req.Units) > 0 {
			unit := req.Units[0]
			if !strings.Contains(unit, ".") {
				unit += ".service"
			}
			msg = fmt.Sprintf("Failed to %s %s: Access denied\n", req.Op, unit)
		} else {
			msg = fmt.Sprintf("Failed to %s: Access denied\n", req.Op)
		}
		writeResp(conn, proto.SystemctlResponse{
			ExitCode: 1,
			Stderr:   msg,
		})
		return
	}

	resp := executePrivilegedOp(req)
	writeResp(conn, resp)
}

// peerCredentials returns the kernel-vouched (uid, gid) of the connected
// peer via SO_PEERCRED on the Unix socket. This is the trusted source of
// caller identity \u2014 NEVER look at a JSON-claimed uid for authorisation.
func peerCredentials(uc *net.UnixConn) (uid, gid uint32, ok bool) {
	f, err := uc.File()
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	cred, err := syscall.GetsockoptUcred(int(f.Fd()), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	if err != nil {
		return 0, 0, false
	}
	return cred.Uid, cred.Gid, true
}

// writeResp serialises a response and writes it as a SYSTEMCTL_RESP frame.
func writeResp(conn net.Conn, resp proto.SystemctlResponse) {
	_ = proto.SendJSON(conn, proto.SYSTEMCTL_RESP, resp)
}

// --- Execution: invoke svc* with stdout/stderr captured to buffers ---

// captureOutputMu serialises os.Stdout/Stderr swapping. The capture trick
// is process-global \u2014 if two privileged ops ran concurrently they'd see
// each other's output. Privileged ops are fast (start/stop), so a mutex
// is acceptable; the alternative (plumb io.Writer through every svc*) is
// a much bigger refactor and orthogonal to this patch.
var captureOutputMu sync.Mutex

// executePrivilegedOp runs the op in-process, with os.Stdout/Stderr piped
// into buffers so the formatted text can be sent back to the IPC client.
//
// The op functions still call fmt.Print/Fprintln to stdout/stderr as they
// always have \u2014 we don't change them. We just redirect those streams for
// the duration of the call.
func executePrivilegedOp(req proto.SystemctlRequest) proto.SystemctlResponse {
	captureOutputMu.Lock()
	defer captureOutputMu.Unlock()

	origOut, origErr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	if err != nil {
		return proto.SystemctlResponse{ExitCode: 1, Stderr: fmt.Sprintf("pipe: %v\n", err)}
	}
	rErr, wErr, err := os.Pipe()
	if err != nil {
		rOut.Close()
		wOut.Close()
		return proto.SystemctlResponse{ExitCode: 1, Stderr: fmt.Sprintf("pipe: %v\n", err)}
	}
	os.Stdout = wOut
	os.Stderr = wErr

	var stdout, stderr bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { io.Copy(&stdout, rOut); wg.Done() }()
	go func() { io.Copy(&stderr, rErr); wg.Done() }()

	exitCode := dispatchPrivilegedOp(req)

	wOut.Close()
	wErr.Close()
	wg.Wait()
	rOut.Close()
	rErr.Close()
	os.Stdout = origOut
	os.Stderr = origErr

	return proto.SystemctlResponse{
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}
}

// dispatchPrivilegedOp runs a single privileged op against a fresh Registry
// and returns the exit code. Mirrors the relevant cases from runSystemctl's
// switch but never calls os.Exit \u2014 the IPC server can't kill the daemon.
//
// stdout/stderr go to whatever the caller has redirected them to (in the
// IPC server: pipes that feed bytes.Buffers; in unit tests: real os
// streams). The caller does the final exit-code propagation.
func dispatchPrivilegedOp(req proto.SystemctlRequest) int {
	reg := NewRegistry()
	flags := req.Flags
	nowFlag := flags["now"] == "1"
	signalName := flags["signal"]

	switch req.Op {
	case "start":
		for _, raw := range req.Units {
			u, err := reg.Resolve(raw)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Failed to start %s: Unit %s not found.\n", raw, raw)
				return 5
			}
			if err := svcStart(u); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to start %s: %v\n", raw, err)
				return 1
			}
		}
	case "stop":
		for _, raw := range req.Units {
			u, err := reg.Resolve(raw)
			if err != nil {
				continue // stopping an unknown unit is not an error
			}
			if err := svcStop(u); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to stop %s: %v\n", raw, err)
				return 1
			}
		}
	case "restart":
		for _, raw := range req.Units {
			u, err := reg.Resolve(raw)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Failed to restart %s: Unit %s not found.\n", raw, raw)
				return 5
			}
			svcStop(u)
			if err := svcStart(u); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to restart %s: %v\n", raw, err)
				return 1
			}
		}
	case "try-restart":
		for _, raw := range req.Units {
			u, err := reg.Resolve(raw)
			if err != nil || !svcIsActive(u) {
				continue
			}
			svcStop(u)
			if err := svcStart(u); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to restart %s: %v\n", raw, err)
				return 1
			}
		}
	case "reload":
		for _, raw := range req.Units {
			u, err := reg.Resolve(raw)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Failed to reload %s: Unit %s not found.\n", raw, raw)
				return 5
			}
			if err := svcReload(u); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to reload %s: %v\n", raw, err)
				return 1
			}
		}
	case "reload-or-restart":
		for _, raw := range req.Units {
			u, err := reg.Resolve(raw)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Failed to reload-or-restart %s: Unit %s not found.\n", raw, raw)
				return 5
			}
			if err := svcReload(u); err != nil {
				svcStop(u)
				if err := svcStart(u); err != nil {
					fmt.Fprintf(os.Stderr, "Failed to restart %s: %v\n", raw, err)
					return 1
				}
			}
		}
	case "enable":
		for _, raw := range req.Units {
			u, err := reg.Resolve(raw)
			if err != nil {
				continue // enable on missing is silently tolerated
			}
			if err := svcEnable(u); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to enable %s: %v\n", raw, err)
				return 1
			}
			if nowFlag {
				svcStart(u)
			}
		}
	case "disable":
		for _, raw := range req.Units {
			u, err := reg.Resolve(raw)
			if err != nil {
				svcDisableByName(normalizeName(raw), unitSuffix(raw))
				continue
			}
			if nowFlag {
				svcStop(u)
			}
			if err := svcDisable(u); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to disable %s: %v\n", raw, err)
				return 1
			}
		}
	case "mask":
		for _, raw := range req.Units {
			if err := svcMaskName(normalizeName(raw)); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to mask %s: %v\n", raw, err)
				return 1
			}
		}
	case "unmask":
		for _, raw := range req.Units {
			if err := svcUnmaskName(normalizeName(raw)); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to unmask %s: %v\n", raw, err)
				return 1
			}
		}
	case "kill":
		sig := syscall.SIGTERM
		switch signalName {
		case "KILL", "SIGKILL":
			sig = syscall.SIGKILL
		case "HUP", "SIGHUP":
			sig = syscall.SIGHUP
		}
		for _, raw := range req.Units {
			u, err := reg.Resolve(raw)
			if err != nil {
				continue
			}
			pid, err := u.ReadPID()
			if err != nil {
				continue
			}
			if err := syscall.Kill(-pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
				fmt.Fprintf(os.Stderr, "Failed to kill %s: %v\n", raw, err)
				return 1
			}
		}
	case "preset":
		for _, raw := range req.Units {
			u, err := reg.Resolve(raw)
			if err != nil {
				continue
			}
			svcEnable(u)
		}
	case "daemon-reload", "daemon-reexec":
		// no-op (we re-read unit files on each Resolve)
	case "reset-failed":
		for _, raw := range req.Units {
			u, err := reg.Resolve(raw)
			if err != nil {
				continue
			}
			u.ClearFailed()
		}
	default:
		fmt.Fprintf(os.Stderr, "systemctl: unsupported privileged op %q\n", req.Op)
		return 1
	}
	return 0
}
