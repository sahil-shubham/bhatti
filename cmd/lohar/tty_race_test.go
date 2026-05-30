//go:build linux

package main

import (
	"bytes"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
)

// Regression test for the TTY scrollback / live-PTY ordering race on
// reattach. Tranche 0a item #2 of PLAN-bhatti-v2.md.
//
// Pre-fix, handleSessionAttach set sess.Attached=conn early, then
// snapshotted and wrote the scrollback OUTSIDE sess.mu. Between those
// two steps the PTY reader goroutine (which holds sess.mu while
// writing to Scrollback and to Attached) could fire — sending live
// bytes to the client before the historical scrollback replay landed.
// On the client this manifested as garbled terminal output after
// reconnect: live future bytes appearing ahead of historical past
// bytes, sometimes duplicated.
//
// The fix moves Attached=conn to AFTER the scrollback replay, keeps
// the entire replay under sess.mu, and only releases the lock when
// the live path is ready to take over. The PTY reader is blocked at
// its own sess.mu.Lock() throughout, so it cannot interleave bytes
// during the replay.

// TestSessionAttachOrdering_PTYReaderDoesNotInterleave is the
// regression. We pre-populate Scrollback with N marker 'A' bytes,
// then run a synthetic PTY reader goroutine that pumps 'B' bytes
// into Scrollback + Attached (the same shape as tty.go's real
// PTY reader). In parallel we call handleSessionAttach and read
// frames from the client side of a net.Pipe.
//
// Invariant: in the bytes the client receives, every 'B' must come
// AFTER every 'A'. Pre-fix this fails reliably; post-fix it holds.
func TestSessionAttachOrdering_PTYReaderDoesNotInterleave(t *testing.T) {
	// Build a session directly. No real PTY — we drive the Scrollback
	// from the synthetic PTY reader below.
	sess := &Session{
		ID:        "test-ordering",
		Argv:      []string{"sh"},
		TTY:       true,
		Scrollback: newRingBuffer(65536),
		CreatedAt: time.Now(),
	}

	const historicalSize = 1024
	historical := bytes.Repeat([]byte{'A'}, historicalSize)
	sess.Scrollback.Write(historical)

	registry.Lock()
	registry.sessions[sess.ID] = sess
	registry.Unlock()
	defer removeSession(sess.ID)

	clientConn, serverConn := net.Pipe()
	// Don't defer clientConn.Close() at test scope — close it from
	// the read goroutine the moment its deadline hits, so the PTY
	// simulator's WriteFrame to serverConn fails fast instead of
	// blocking forever (net.Pipe writes block until the other side
	// reads or closes).

	// Synthetic PTY reader: mirrors the production PTY-master-reader
	// loop in tty.go — under sess.mu, append to Scrollback and write
	// to Attached if non-nil. Runs concurrently with the attach.
	stopPTY := make(chan struct{})
	ptyDone := make(chan struct{})
	go func() {
		defer close(ptyDone)
		buf := []byte{'B'}
		for {
			select {
			case <-stopPTY:
				return
			default:
			}
			sess.mu.Lock()
			sess.Scrollback.Write(buf)
			if sess.Attached != nil {
				_ = proto.WriteFrame(sess.Attached, proto.STDOUT, buf)
			}
			sess.mu.Unlock()
			// Yield to give the attach goroutine a chance to run between
			// PTY-reader iterations. Without this the PTY reader's tight
			// loop dominates one core and the attach starves.
			time.Sleep(time.Microsecond)
		}
	}()

	// Run handleSessionAttach on the server side.
	attachDone := make(chan struct{})
	go func() {
		defer close(attachDone)
		handleSessionAttach(serverConn, sess.ID, false)
	}()

	// Read frames from the client side for a bounded period. Capture
	// only STDOUT data (the bytes that would render to the user's
	// terminal). SESSION_INFO and other control frames are ignored.
	var (
		stdout    bytes.Buffer
		readErr   error
		readMu    sync.Mutex
		readDone  = make(chan struct{})
	)
	go func() {
		defer close(readDone)
		defer clientConn.Close()
		clientConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		for {
			msgType, payload, err := proto.ReadFrame(clientConn)
			if err != nil {
				readMu.Lock()
				readErr = err
				readMu.Unlock()
				return
			}
			if msgType == proto.STDOUT {
				readMu.Lock()
				stdout.Write(payload)
				readMu.Unlock()
			}
		}
	}()

	<-readDone
	close(stopPTY)
	<-ptyDone
	serverConn.Close()
	<-attachDone
	_ = readErr // expected: deadline exceeded once we stop reading

	content := stdout.Bytes()

	// Sanity: we must have seen the full historical scrollback at
	// least once. (Ring buffer never lost it; client got the snapshot.)
	if !bytes.Contains(content, historical) {
		t.Fatalf("client never received historical scrollback (%d 'A' bytes)\nGot: %q",
			historicalSize, content)
	}

	// Core invariant: every 'B' must come AFTER every 'A'.
	// I.e., the last 'A' must come before the first 'B' (if any).
	lastA := bytes.LastIndexByte(content, 'A')
	firstB := bytes.IndexByte(content, 'B')
	if firstB >= 0 && firstB < lastA {
		// Render a small excerpt around the boundary for the failure msg.
		start := firstB - 20
		if start < 0 {
			start = 0
		}
		end := lastA + 20
		if end > len(content) {
			end = len(content)
		}
		t.Fatalf("ordering race: live PTY 'B' bytes interleaved before historical 'A's\n"+
			"  firstB=%d lastA=%d\n"+
			"  excerpt around boundary: %q",
			firstB, lastA, content[start:end])
	}
}

// TestSessionAttachOrdering_DetachedSessionExitedReplay verifies the
// exit path still delivers scrollback in the correct order even when
// the session has already exited. The fix moved the exit-frame branch
// inside the lock; check we still produce a clean SESSION_INFO →
// STDOUT(scrollback) → EXIT sequence.
func TestSessionAttachOrdering_DetachedSessionExitedReplay(t *testing.T) {
	sess := &Session{
		ID:        "test-exited",
		Argv:      []string{"echo", "done"},
		TTY:       true,
		Scrollback: newRingBuffer(65536),
		CreatedAt: time.Now(),
	}
	exitCode := 0
	sess.ExitCode = &exitCode
	sess.Scrollback.Write([]byte("done\n"))

	registry.Lock()
	registry.sessions[sess.ID] = sess
	registry.Unlock()
	// Note: handleSessionAttach itself removes the session on exit path;
	// no deferred removeSession needed.

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()

	go handleSessionAttach(serverConn, sess.ID, false)

	clientConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))

	// Frame 1: SESSION_INFO
	mt, _, err := proto.ReadFrame(clientConn)
	if err != nil {
		t.Fatalf("read frame 1: %v", err)
	}
	if mt != proto.SESSION_INFO {
		t.Fatalf("frame 1: expected SESSION_INFO, got 0x%02x", mt)
	}

	// Frame 2: STDOUT (scrollback)
	mt, payload, err := proto.ReadFrame(clientConn)
	if err != nil {
		t.Fatalf("read frame 2: %v", err)
	}
	if mt != proto.STDOUT {
		t.Fatalf("frame 2: expected STDOUT, got 0x%02x", mt)
	}
	if string(payload) != "done\n" {
		t.Fatalf("frame 2: expected scrollback %q, got %q", "done\n", payload)
	}

	// Frame 3: EXIT
	mt, _, err = proto.ReadFrame(clientConn)
	if err != nil {
		t.Fatalf("read frame 3: %v", err)
	}
	if mt != proto.EXIT {
		t.Fatalf("frame 3: expected EXIT, got 0x%02x", mt)
	}
}
