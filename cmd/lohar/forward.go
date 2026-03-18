//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/sahilshubham/bhatti/pkg/agent/proto"
)

func handleForwardConnection(conn net.Conn) {
	defer conn.Close()

	// Auth check
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
		fmt.Fprintf(os.Stderr, "lohar: forward read: %v\n", err)
		return
	}
	if msgType != proto.FWD_REQ {
		proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("expected FWD_REQ, got 0x%02x", msgType)))
		return
	}

	var req proto.ForwardRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		errMsg := fmt.Sprintf("bad forward request: %v", err)
		sendForwardError(conn, errMsg)
		return
	}

	tcp, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", req.Port))
	if err != nil {
		sendForwardError(conn, err.Error())
		return
	}

	proto.SendJSON(conn, proto.FWD_RESP, proto.ForwardResponse{Status: "ok"})

	// Bidirectional relay — raw bytes, no framing after handshake.
	done := make(chan struct{})
	go func() {
		io.Copy(tcp, conn)
		tcp.(*net.TCPConn).CloseWrite()
		close(done)
	}()
	io.Copy(conn, tcp)
	<-done
}

func sendForwardError(conn net.Conn, msg string) {
	proto.SendJSON(conn, proto.FWD_RESP, proto.ForwardResponse{
		Status:  "error",
		Message: &msg,
	})
}
