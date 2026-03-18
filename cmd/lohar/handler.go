//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
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

	if msgType != proto.EXEC_REQ {
		proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("expected EXEC_REQ, got 0x%02x", msgType)))
		return
	}

	var req proto.ExecRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("bad exec request: %v", err)))
		return
	}

	if len(req.Argv) == 0 {
		proto.WriteFrame(conn, proto.ERROR, []byte("empty argv"))
		return
	}

	if req.TTY != nil && *req.TTY {
		handleTTYExec(conn, req)
	} else {
		handlePipedExec(conn, req)
	}
}
