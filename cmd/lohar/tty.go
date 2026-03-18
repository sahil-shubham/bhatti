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

	"github.com/sahilshubham/bhatti/pkg/agent/proto"
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
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0,
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
				sess.Scrollback.Write(buf[:n])
				sess.mu.Lock()
				if sess.Attached != nil {
					proto.WriteFrame(sess.Attached, proto.STDOUT, buf[:n])
				}
				sess.mu.Unlock()
			}
			if err != nil {
				// PTY closed — process exited
				exitCode := exitCodeFromErr(cmd.Wait())
				sess.mu.Lock()
				sess.ExitCode = &exitCode
				if sess.Attached != nil {
					exit := proto.ExitPayload(int32(exitCode))
					proto.WriteFrame(sess.Attached, proto.EXIT, exit[:])
				}
				sess.mu.Unlock()
				master.Close()
				return
			}
		}
	}()

	// Host → PTY master (STDIN, RESIZE, KILL, disconnect)
	readHostInput(conn, sess)
}

// handleSessionAttach reconnects a client to an existing session.
func handleSessionAttach(conn net.Conn, sessionID string) {
	sess := getSession(sessionID)
	if sess == nil {
		proto.WriteFrame(conn, proto.ERROR, []byte("session not found"))
		return
	}

	sess.mu.Lock()
	// Detach previous client if any
	if sess.Attached != nil {
		exit := proto.ExitPayload(0)
		proto.WriteFrame(sess.Attached, proto.EXIT, exit[:])
		sess.Attached = nil
	}
	sess.cancelIdleTimer()
	sess.Attached = conn
	sess.mu.Unlock()

	// Send session info
	sess.mu.Lock()
	info := proto.SessionInfo{
		SessionID: sess.ID,
		Argv:      strings.Join(sess.Argv, " "),
		TTY:       sess.TTY,
		Running:   sess.ExitCode == nil,
		ExitCode:  sess.ExitCode,
		Attached:  true,
		CreatedAt: sess.CreatedAt.Unix(),
	}
	sess.mu.Unlock()
	proto.SendJSON(conn, proto.SESSION_INFO, info)

	// Replay scrollback
	if sess.Scrollback != nil {
		scrollback := sess.Scrollback.Bytes()
		if len(scrollback) > 0 {
			proto.WriteFrame(conn, proto.STDOUT, scrollback)
		}
	}

	// If process already exited, send exit and clean up
	sess.mu.Lock()
	exited := sess.ExitCode != nil
	exitCode := sess.ExitCode
	sess.mu.Unlock()
	if exited {
		exit := proto.ExitPayload(int32(*exitCode))
		proto.WriteFrame(conn, proto.EXIT, exit[:])
		removeSession(sess.ID)
		return
	}

	// Read host input until disconnect
	readHostInput(conn, sess)
}

// readHostInput reads frames from the host and forwards to the session's PTY.
// Returns when the host disconnects or sends KILL.
func readHostInput(conn net.Conn, sess *Session) {
	for {
		msgType, payload, err := proto.ReadFrame(conn)
		if err != nil {
			// Host disconnected — detach, don't kill
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
			if sess.Master != nil {
				if r, c, ok := proto.ParseResize(payload); ok {
					setWinsize(sess.Master, r, c)
				}
			}
		case proto.KILL:
			sess.mu.Lock()
			if sess.Cmd != nil && sess.Cmd.Process != nil {
				sess.Cmd.Process.Signal(syscall.SIGTERM)
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
	cmd.Env = buildEnv(nil)
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
				sess.Scrollback.Write(buf[:n])
				sess.mu.Lock()
				if sess.Attached != nil {
					proto.WriteFrame(sess.Attached, proto.STDOUT, buf[:n])
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
