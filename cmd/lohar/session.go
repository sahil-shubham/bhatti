//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

// Session represents a running or recently-completed exec session.
type Session struct {
	ID         string
	Argv       []string
	TTY        bool
	Master     *os.File    // PTY master fd (TTY only)
	group      *childGroup // nil until the process has started; guarded by mu
	Scrollback *ringBuffer // 64KB (TTY only)
	Attached   net.Conn    // currently attached connection (nil = detached)
	ExitCode   *int        // nil = still running
	MaxIdle    time.Duration
	CreatedAt  time.Time
	mu         sync.Mutex
	idleTimer  *time.Timer
}

var registry = struct {
	sync.Mutex
	sessions map[string]*Session
	counter  int
}{sessions: make(map[string]*Session)}

func newSession(argv []string, tty bool, maxIdle time.Duration) *Session {
	registry.Lock()
	defer registry.Unlock()
	if len(registry.sessions) >= maxActiveSessions {
		return nil // caller checks for nil and returns error
	}
	registry.counter++
	s := &Session{
		ID:        fmt.Sprintf("s%d", registry.counter),
		Argv:      argv,
		TTY:       tty,
		MaxIdle:   maxIdle,
		CreatedAt: time.Now(),
	}
	if tty {
		s.Scrollback = newRingBuffer(65536) // 64KB
	}
	registry.sessions[s.ID] = s
	return s
}

func getSession(id string) *Session {
	registry.Lock()
	defer registry.Unlock()
	return registry.sessions[id]
}

func removeSession(id string) {
	registry.Lock()
	defer registry.Unlock()
	delete(registry.sessions, id)
}

func listSessions() []proto.SessionInfo {
	registry.Lock()
	defer registry.Unlock()
	out := make([]proto.SessionInfo, 0, len(registry.sessions))
	for _, s := range registry.sessions {
		s.mu.Lock()
		info := proto.SessionInfo{
			SessionID: s.ID,
			Argv:      strings.Join(s.Argv, " "),
			TTY:       s.TTY,
			Running:   s.ExitCode == nil,
			ExitCode:  s.ExitCode,
			Attached:  s.Attached != nil,
			CreatedAt: s.CreatedAt.Unix(),
		}
		s.mu.Unlock()
		out = append(out, info)
	}
	return out
}

// signalGroup signals the session's process group; call it without s.mu held.
// It does nothing before the process has started or after its exit has been
// collected, when the PID may belong to something else.
func (s *Session) signalGroup(sig syscall.Signal) {
	s.mu.Lock()
	g := s.group
	s.mu.Unlock()
	if g != nil {
		g.signal(sig)
	}
}

func (s *Session) startIdleTimer() {
	if s.MaxIdle <= 0 {
		return // 0 = run forever
	}
	if s.idleTimer != nil {
		return // already ticking
	}
	s.idleTimer = time.AfterFunc(s.MaxIdle, func() {
		s.signalGroup(syscall.SIGKILL)
	})
}

func (s *Session) cancelIdleTimer() {
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
}

// --- Ring Buffer ---

type ringBuffer struct {
	buf  []byte
	size int
	w    int // next write position
	full bool
}

func newRingBuffer(size int) *ringBuffer {
	return &ringBuffer{buf: make([]byte, size), size: size}
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	for _, b := range p {
		r.buf[r.w] = b
		r.w = (r.w + 1) % r.size
		if r.w == 0 {
			r.full = true
		}
	}
	return len(p), nil
}

// Bytes returns the buffered content in order (oldest first).
func (r *ringBuffer) Bytes() []byte {
	if !r.full {
		return append([]byte{}, r.buf[:r.w]...)
	}
	out := make([]byte, r.size)
	n := copy(out, r.buf[r.w:])
	copy(out[n:], r.buf[:r.w])
	return out
}
