//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

// handlePipedSession creates a non-TTY session with scrollback and reattach.
// Like handleTTYSession but uses stdin/stdout pipes instead of PTY.
// Designed for embedding long-running services (e.g. pi --mode rpc).
func handlePipedSession(conn net.Conn, req proto.ExecRequest) {
	maxIdle := time.Duration(0)
	if req.MaxIdleSec != nil {
		maxIdle = time.Duration(*req.MaxIdleSec) * time.Second
	}

	sess := newSession(req.Argv, false, maxIdle) // tty=false
	if sess == nil {
		proto.WriteFrame(conn, proto.ERROR, []byte("session limit exceeded"))
		return
	}

	// Create pipes — NOT a PTY. No terminal escape sequences.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("stdin pipe: %v", err)))
		removeSession(sess.ID)
		return
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("stdout pipe: %v", err)))
		removeSession(sess.ID)
		return
	}

	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Env = buildEnv(req.Env)
	if req.Cwd != nil {
		cmd.Dir = *req.Cwd
	}
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = stdoutW // merge stderr into stdout
	// setsid: process survives host disconnect.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:     true,
		Credential: &syscall.Credential{Uid: 1000, Gid: 1000},
	}

	if err := cmd.Start(); err != nil {
		stdinR.Close()
		stdinW.Close()
		stdoutR.Close()
		stdoutW.Close()
		proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("start: %v", err)))
		removeSession(sess.ID)
		return
	}
	// Child has inherited stdinR and stdoutW. Close our copies.
	stdinR.Close()
	stdoutW.Close()

	sess.Cmd = cmd
	// Store the stdin write end in Master so reattach can write to it.
	// For piped sessions: Master = stdin write pipe (we write TO the child).
	sess.Master = stdinW

	// Allocate scrollback for piped sessions (newSession only allocates
	// when tty=true). We need it for reattach replay.
	sess.mu.Lock()
	sess.Scrollback = newRingBuffer(65536) // 64KB
	sess.mu.Unlock()

	// Send session info to host
	proto.SendJSON(conn, proto.SESSION_INFO, proto.SessionInfo{
		SessionID: sess.ID,
		Argv:      strings.Join(req.Argv, " "),
		TTY:       false,
		Running:   true,
		Attached:  true,
		CreatedAt: sess.CreatedAt.Unix(),
	})

	sess.mu.Lock()
	sess.Attached = conn
	sess.mu.Unlock()

	// Background goroutine: stdout pipe → scrollback + attached conn
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := stdoutR.Read(buf)
			if n > 0 {
				sess.mu.Lock()
				sess.Scrollback.Write(buf[:n])
				if sess.Attached != nil {
					if werr := proto.WriteFrame(sess.Attached, proto.STDOUT, buf[:n]); werr != nil {
						sess.Attached.Close()
					}
				}
				sess.mu.Unlock()
			}
			if err != nil {
				// Pipe closed — process exited.
				exitCode := exitCodeFromErr(cmd.Wait())
				sess.mu.Lock()
				sess.ExitCode = &exitCode
				if sess.Attached != nil {
					exit := proto.ExitPayload(int32(exitCode))
					proto.WriteFrame(sess.Attached, proto.EXIT, exit[:])
					sess.mu.Unlock()
					stdoutR.Close()
					removeSession(sess.ID)
					return
				}
				sess.mu.Unlock()
				stdoutR.Close()
				// No client attached — keep session for 30s
				time.AfterFunc(30*time.Second, func() {
					removeSession(sess.ID)
				})
				return
			}
		}
	}()

	// Host → child stdin (STDIN frames only, no RESIZE for piped sessions)
	readPipedHostInput(conn, sess)
}

// readPipedHostInput reads frames from the host and forwards to the session's
// stdin pipe. Returns when the host disconnects or sends KILL.
// Unlike readHostInput (TTY), this ignores RESIZE frames.
func readPipedHostInput(conn net.Conn, sess *Session) {
	for {
		msgType, payload, err := proto.ReadFrame(conn)
		if err != nil {
			// Host disconnected — detach, don't kill.
			// The stdin pipe stays open (held by sess.Master).
			// The child process continues running.
			fmt.Fprintf(os.Stderr, "lohar: session %s: host disconnected: %v\n", sess.ID, err)
			sess.mu.Lock()
			sess.Attached = nil
			sess.mu.Unlock()
			sess.startIdleTimer()
			return
		}
		switch msgType {
		case proto.STDIN:
			updateActivity()
			if sess.Master != nil {
				sess.Master.Write(payload)
			}
		case proto.KILL:
			sess.mu.Lock()
			if sess.Cmd != nil && sess.Cmd.Process != nil {
				syscall.Kill(-sess.Cmd.Process.Pid, syscall.SIGTERM)
			}
			sess.mu.Unlock()
			return
		// RESIZE: ignored for piped sessions (no PTY)
		}
	}
}
