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
	"unsafe"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

// PTY ioctl constants (same on amd64 and arm64 Linux).
const (
	TIOCGPTN   = 0x80045430
	TIOCSPTLCK = 0x40045431
	TIOCSWINSZ = 0x5414
	TIOCSCTTY  = 0x540E
)

type winsize struct {
	Rows   uint16
	Cols   uint16
	XPixel uint16
	YPixel uint16
}

// handleTTYSession creates a new TTY session. On host disconnect, the process
// stays alive (no SIGHUP). The scrollback buffer captures output for reattach.
func handleTTYSession(conn net.Conn, req proto.ExecRequest) {
	maxIdle := time.Duration(0) // forever by default
	if req.MaxIdleSec != nil {
		maxIdle = time.Duration(*req.MaxIdleSec) * time.Second
	}

	sess := newSession(req.Argv, true, maxIdle)
	if sess == nil {
		proto.WriteFrame(conn, proto.ERROR, []byte("session limit exceeded"))
		return
	}

	master, slave, err := openPTY()
	if err != nil {
		proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("pty: %v", err)))
		removeSession(sess.ID)
		return
	}
	sess.Master = master

	rows := uint16(24)
	cols := uint16(80)
	if req.Rows != nil {
		rows = *req.Rows
	}
	if req.Cols != nil {
		cols = *req.Cols
	}
	setWinsize(master, rows, cols)

	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Env = buildEnv(req.Env)
	if req.Cwd != nil {
		cmd.Dir = *req.Cwd
	}
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	// Run as lohar (uid 1000). Users can sudo for root.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:     true,
		Setctty:    true,
		Ctty:       0,
		Credential: &syscall.Credential{Uid: 1000, Gid: 1000},
	}

	if err := cmd.Start(); err != nil {
		slave.Close()
		master.Close()
		proto.WriteFrame(conn, proto.ERROR, []byte(fmt.Sprintf("start: %v", err)))
		removeSession(sess.ID)
		return
	}
	slave.Close()
	sess.Cmd = cmd

	// Send session info to host
	proto.SendJSON(conn, proto.SESSION_INFO, proto.SessionInfo{
		SessionID: sess.ID,
		Argv:      strings.Join(req.Argv, " "),
		TTY:       true,
		Running:   true,
		Attached:  true,
		CreatedAt: sess.CreatedAt.Unix(),
	})

	sess.mu.Lock()
	sess.Attached = conn
	sess.mu.Unlock()

	// Background goroutine: PTY master → scrollback + attached conn
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				sess.mu.Lock()
				sess.Scrollback.Write(buf[:n])
				if sess.Attached != nil {
					if werr := proto.WriteFrame(sess.Attached, proto.STDOUT, buf[:n]); werr != nil {
						// Connection dead — close it so readHostInput
						// gets a read error and does the canonical detach.
						sess.Attached.Close()
					}
				}
				sess.mu.Unlock()
			}
			if err != nil {
				// PTY closed — process exited.
				// Keep the session in the registry so clients can reattach
				// to retrieve scrollback. The session is removed when:
				//   1. A client attaches and we send the exit frame (handleSessionAttach)
				//   2. The reap timer fires (30s after exit, no reattach)
				exitCode := exitCodeFromErr(cmd.Wait())
				sess.mu.Lock()
				sess.ExitCode = &exitCode
				if sess.Attached != nil {
					exit := proto.ExitPayload(int32(exitCode))
					proto.WriteFrame(sess.Attached, proto.EXIT, exit[:])
					// Client is attached and got the exit — clean up now
					sess.mu.Unlock()
					master.Close()
					removeSession(sess.ID)
					return
				}
				sess.mu.Unlock()
				master.Close()
				// No client attached — keep session for 30s so scrollback
				// can be retrieved via reattach.
				time.AfterFunc(30*time.Second, func() {
					removeSession(sess.ID)
				})
				return
			}
		}
	}()

	// Host → PTY master (STDIN, RESIZE, KILL, disconnect)
	readHostInput(conn, sess)
}

// handleSessionAttach reconnects a client to an existing session.
// If ifDetached is true, the attach fails if the session is already attached.
//
// The historical setup (SESSION_INFO + scrollback replay) runs under
// sess.mu and BEFORE sess.Attached is set. This is load-bearing: the
// PTY reader goroutine takes sess.mu before writing new bytes to both
// Scrollback and Attached. Holding the lock here blocks the PTY reader
// during the replay, and leaving Attached=nil until the replay finishes
// ensures the reader cannot interleave live bytes ahead of the
// historical buffer when the lock is finally released. Tranche 0a #2
// of PLAN-bhatti-v2.md.
//
// Pre-fix, sess.Attached was set early and scrollback was written
// outside the lock; the PTY reader could fire between the two, sending
// live STDOUT to the client before the historical replay arrived —
// observable as garbled terminal output on reconnect.
func handleSessionAttach(conn net.Conn, sessionID string, ifDetached bool) {
	sess := getSession(sessionID)
	if sess == nil {
		proto.WriteFrame(conn, proto.ERROR, []byte("session not found"))
		return
	}

	sess.mu.Lock()
	if ifDetached && sess.Attached != nil {
		sess.mu.Unlock()
		proto.WriteFrame(conn, proto.ERROR, []byte("session is attached"))
		return
	}
	// Detach previous client if any.
	if sess.Attached != nil {
		exit := proto.ExitPayload(0)
		proto.WriteFrame(sess.Attached, proto.EXIT, exit[:])
		sess.Attached = nil
	}
	sess.cancelIdleTimer()

	// Snapshot all state we need to replay. Keep sess.Attached = nil
	// for now — the PTY reader will not write to conn until we set it
	// below, after the historical bytes have been sent.
	info := proto.SessionInfo{
		SessionID: sess.ID,
		Argv:      strings.Join(sess.Argv, " "),
		TTY:       sess.TTY,
		Running:   sess.ExitCode == nil,
		ExitCode:  sess.ExitCode,
		Attached:  true,
		CreatedAt: sess.CreatedAt.Unix(),
	}
	var scrollback []byte
	if sess.Scrollback != nil {
		scrollback = sess.Scrollback.Bytes()
	}
	exited := sess.ExitCode != nil
	exitCode := sess.ExitCode

	// Send the historical state under the lock. The PTY reader is
	// blocked at its own sess.mu.Lock() for the duration, so no new
	// bytes are appended to Scrollback or sent to conn during the
	// replay. This is the same backpressure model the PTY reader
	// itself uses (it holds sess.mu across its WriteFrame).
	proto.SendJSON(conn, proto.SESSION_INFO, info)
	if len(scrollback) > 0 {
		proto.WriteFrame(conn, proto.STDOUT, scrollback)
	}

	if exited {
		exit := proto.ExitPayload(int32(*exitCode))
		proto.WriteFrame(conn, proto.EXIT, exit[:])
		sess.mu.Unlock()
		removeSession(sess.ID)
		return
	}

	// Hand off to the live path. From this point the PTY reader will
	// forward fresh bytes to conn, picking up exactly where the
	// scrollback ended.
	sess.Attached = conn
	sess.mu.Unlock()

	readHostInput(conn, sess)
}

// readHostInput reads frames from the host and forwards to the session's PTY.
// Returns when the host disconnects or sends KILL.
func readHostInput(conn net.Conn, sess *Session) {
	for {
		msgType, payload, err := proto.ReadFrame(conn)
		if err != nil {
			// Host disconnected — detach, don't kill
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
		case proto.RESIZE:
			if sess.TTY && sess.Master != nil {
				if r, c, ok := proto.ParseResize(payload); ok {
					setWinsize(sess.Master, r, c)
				}
			}
		case proto.KILL:
			sess.mu.Lock()
			if sess.Cmd != nil && sess.Cmd.Process != nil {
				// TTY sessions use SIGTERM to allow graceful shutdown.
				// The session's Setsid:true means the shell is the session
				// leader — SIGTERM to the process group lets it clean up.
				// This preserves the session model: if the process handles
				// SIGTERM and stays alive, the session remains reattachable.
				syscall.Kill(-sess.Cmd.Process.Pid, syscall.SIGTERM)
			}
			sess.mu.Unlock()
			return
		}
	}
}

func openPTY() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}

	var ptsNum uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(),
		TIOCGPTN, uintptr(unsafe.Pointer(&ptsNum))); errno != 0 {
		master.Close()
		return nil, nil, fmt.Errorf("TIOCGPTN: %v", errno)
	}

	var unlock int32 = 0
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(),
		TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		master.Close()
		return nil, nil, fmt.Errorf("TIOCSPTLCK: %v", errno)
	}

	slavePath := fmt.Sprintf("/dev/pts/%d", ptsNum)
	slave, err = os.OpenFile(slavePath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("open %s: %w", slavePath, err)
	}

	return master, slave, nil
}

// runInitSession runs the init script as a TTY session with well-known ID "init".
// The session can be attached to by the host via SessionAttach("init").
func runInitSession(script, user string) {
	sess := newSession([]string{"sh", "-c", script}, true, 0)
	if sess == nil {
		logf("init session: session limit exceeded")
		return
	}
	// Override the auto-generated ID with "init"
	registry.Lock()
	delete(registry.sessions, sess.ID)
	sess.ID = "init"
	registry.sessions["init"] = sess
	registry.Unlock()

	master, slave, err := openPTY()
	if err != nil {
		logf("init session PTY: %v", err)
		removeSession("init")
		return
	}
	sess.Master = master

	cmd := exec.Command("sh", "-c", script)
	initEnv := map[string]string{}
	if user == "root" {
		initEnv["HOME"] = "/root"
	}
	cmd.Env = buildEnv(initEnv)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave

	uid := uint32(1000)
	gid := uint32(1000)
	if user == "root" {
		uid = 0
		gid = 0
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0,
		Credential: &syscall.Credential{
			Uid: uid,
			Gid: gid,
		},
	}
	cmd.Dir = "/workspace"
	if err := cmd.Start(); err != nil {
		slave.Close()
		master.Close()
		logf("init session start: %v", err)
		removeSession("init")
		return
	}
	slave.Close()
	sess.Cmd = cmd

	logf("init session started (pid %d)", cmd.Process.Pid)

	// Background reader: PTY master → scrollback (+ attached conn if any)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
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
				exitCode := exitCodeFromErr(cmd.Wait())
				sess.mu.Lock()
				sess.ExitCode = &exitCode
				if sess.Attached != nil {
					exit := proto.ExitPayload(int32(exitCode))
					proto.WriteFrame(sess.Attached, proto.EXIT, exit[:])
				}
				sess.mu.Unlock()
				master.Close()
				logf("init session exited (code %d)", exitCode)
				removeSession(sess.ID)
				return
			}
		}
	}()
}

func setWinsize(f *os.File, rows, cols uint16) error {
	ws := winsize{Rows: rows, Cols: cols}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(),
		TIOCSWINSZ, uintptr(unsafe.Pointer(&ws)))
	if errno != 0 {
		return errno
	}
	return nil
}
