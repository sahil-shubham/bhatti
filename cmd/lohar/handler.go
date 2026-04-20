//go:build linux

package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

// agentToken is set during boot from the config drive. Empty = no auth required.
var agentToken string

// lastActivity tracks the unix timestamp of the last client interaction.
var lastActivity int64

func updateActivity() {
	atomic.StoreInt64(&lastActivity, time.Now().Unix())
}

func getActivity() proto.ActivityInfo {
	registry.Lock()
	active, attached := 0, 0
	for _, s := range registry.sessions {
		s.mu.Lock()
		if s.ExitCode == nil {
			active++
		}
		if s.Attached != nil {
			attached++
		}
		s.mu.Unlock()
	}
	registry.Unlock()
	return proto.ActivityInfo{
		LastActivityUnix: atomic.LoadInt64(&lastActivity),
		ActiveSessions:   active,
		AttachedSessions: attached,
	}
}

const maxConcurrentConns = 50
const maxActiveSessions = 20

var activeConns atomic.Int32

func handleControlConnection(conn net.Conn) {
	if activeConns.Add(1) > maxConcurrentConns {
		activeConns.Add(-1)
		proto.WriteFrame(conn, proto.ERROR, []byte("connection limit exceeded"))
		conn.Close()
		return
	}
	defer activeConns.Add(-1)
	defer conn.Close()

	// Auth check: if a token is configured, the first frame must be AUTH.
	if agentToken != "" {
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		msgType, payload, err := proto.ReadFrame(conn)
		conn.SetReadDeadline(time.Time{})
		if err != nil || msgType != proto.AUTH ||
			subtle.ConstantTimeCompare(payload, []byte(agentToken)) != 1 {
			proto.WriteFrame(conn, proto.ERROR, []byte("auth required"))
			return
		}
	}

	msgType, payload, err := proto.ReadFrame(conn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lohar: control read: %v\n", err)
		return
	}

	switch msgType {
	case proto.EXEC_REQ:
		updateActivity()
		var req proto.ExecRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("bad exec request: %v", err)))
			return
		}
		if req.SessionID != nil {
			// Attach to existing session
			ifDetached := req.IfDetached != nil && *req.IfDetached
			handleSessionAttach(conn, *req.SessionID, ifDetached)
		} else if len(req.Argv) == 0 {
			proto.WriteFrame(conn, proto.ERROR, []byte("empty argv"))
		} else if req.Detach != nil && *req.Detach {
			// Detached exec: fire-and-forget, returns PID immediately
			handleDetachedExec(conn, req)
		} else if req.TTY != nil && *req.TTY {
			// Create new TTY session
			handleTTYSession(conn, req)
		} else if req.Session != nil && *req.Session {
			// Non-TTY session with scrollback+reattach (for embedding pi --mode rpc)
			handlePipedSession(conn, req)
		} else {
			// Non-TTY exec (one-shot, blocks until done)
			handlePipedExec(conn, req)
		}

	case proto.EXEC_LIST_REQ:
		sessions := listSessions()
		proto.SendJSON(conn, proto.EXEC_LIST_RESP, sessions)

	case proto.ACTIVITY_REQ:
		proto.SendJSON(conn, proto.ACTIVITY_RESP, getActivity())

	case proto.EXEC_KILL:
		var req struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(payload, &req); err != nil {
			proto.WriteFrame(conn, proto.ERROR, []byte("bad kill request"))
			return
		}
		s := getSession(req.SessionID)
		if s == nil {
			proto.WriteFrame(conn, proto.ERROR, []byte("session not found"))
			return
		}
		s.mu.Lock()
		if s.Cmd != nil && s.Cmd.Process != nil {
			// Kill entire process group for reliable cleanup
			syscall.Kill(-s.Cmd.Process.Pid, syscall.SIGKILL)
		}
		s.mu.Unlock()
		exit := proto.ExitPayload(0)
		proto.WriteFrame(conn, proto.EXIT, exit[:])

	case proto.FILE_READ_REQ:
		updateActivity()
		handleFileRead(conn, payload)
	case proto.FILE_WRITE_REQ:
		updateActivity()
		handleFileWrite(conn, payload)
	case proto.FILE_STAT_REQ:
		handleFileStat(conn, payload)
	case proto.FILE_LS_REQ:
		handleFileList(conn, payload)

	default:
		proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("unexpected frame type 0x%02x", msgType)))
	}
}
