//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/sahilshubham/bhatti/pkg/agent/proto"
)

// agentToken is set during boot from the config drive. Empty = no auth required.
var agentToken string

func handleControlConnection(conn net.Conn) {
	defer conn.Close()

	// Auth check: if a token is configured, the first frame must be AUTH.
	if agentToken != "" {
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		msgType, payload, err := proto.ReadFrame(conn)
		conn.SetReadDeadline(time.Time{})
		if err != nil || msgType != proto.AUTH || string(payload) != agentToken {
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
		var req proto.ExecRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("bad exec request: %v", err)))
			return
		}
		if req.SessionID != nil {
			// Attach to existing session
			handleSessionAttach(conn, *req.SessionID)
		} else if len(req.Argv) == 0 {
			proto.WriteFrame(conn, proto.ERROR, []byte("empty argv"))
		} else if req.TTY != nil && *req.TTY {
			// Create new TTY session
			handleTTYSession(conn, req)
		} else {
			// Non-TTY exec (one-shot, blocks until done)
			handlePipedExec(conn, req)
		}

	case proto.EXEC_LIST_REQ:
		sessions := listSessions()
		proto.SendJSON(conn, proto.EXEC_LIST_RESP, sessions)

	case proto.EXEC_KILL:
		var req struct {
			SessionID string `json:"session_id"`
		}
		json.Unmarshal(payload, &req)
		s := getSession(req.SessionID)
		if s == nil {
			proto.WriteFrame(conn, proto.ERROR, []byte("session not found"))
			return
		}
		s.mu.Lock()
		if s.Cmd != nil && s.Cmd.Process != nil {
			s.Cmd.Process.Signal(syscall.SIGTERM)
		}
		s.mu.Unlock()
		exit := proto.ExitPayload(0)
		proto.WriteFrame(conn, proto.EXIT, exit[:])

	default:
		proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("unexpected frame type 0x%02x", msgType)))
	}
}
