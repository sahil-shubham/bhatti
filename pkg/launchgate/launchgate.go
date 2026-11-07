// Package launchgate holds a helper process at its first instruction until the
// daemon has put it where it belongs.
//
// The daemon places a helper (bhatti-vmm, bhatti-netd) in its own cgroup by
// asking systemd to move the pid, and systemd's reply does not mean the pid has
// moved: a helper that started working at once would charge its first
// allocations, guest memory above all, to the daemon's cgroup, where they stay
// even after the move. So the daemon starts the helper with the read end of a
// pipe as an extra file and a leading `--gate-fd N` argument; the helper's
// first act is Wait, which reads one byte. The daemon writes it once the
// helper is in its scope and recorded there.
//
// The gate fails closed. Anything but the byte — the daemon closing the pipe
// on an error, the daemon dying (the kernel closes its end), a different byte,
// no byte in time — makes Wait return an error, and the helper exits before
// it has done any work.
package launchgate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// Flag is the argument that names the gate's file descriptor. Daemon and
// helpers ship in one bundle, so a helper that doesn't know it is a broken
// install and fails on the unknown flag.
const Flag = "--gate-fd"

// Timeout is how long a helper waits for the byte. The daemon bounds a
// placement well inside it, so only a daemon that is alive but wedged gets
// here; a dead one has already closed the pipe.
const Timeout = 30 * time.Second

// open is the one byte that opens the gate; anything else keeps it shut.
const open byte = 'G'

var (
	// ErrClosed: the daemon closed its end without opening the gate.
	ErrClosed = errors.New("launch gate closed without opening: the daemon did not place this helper")
	// ErrBadByte: something other than the opening byte arrived.
	ErrBadByte = errors.New("launch gate: unexpected byte")
	// ErrTimeout: nothing arrived in time.
	ErrTimeout = errors.New("launch gate: timed out waiting for the daemon")
)

// Wait blocks until the daemon opens the gate on fd. It returns nil only when
// it read the opening byte; every other outcome is an error and the caller
// must exit without doing its work. The descriptor is closed once it has been
// read, so the helper never carries the pipe into what it goes on to do.
func Wait(fd int, timeout time.Duration) error {
	f := os.NewFile(uintptr(fd), "launch-gate")
	if f == nil {
		return fmt.Errorf("launch gate: invalid file descriptor %d", fd)
	}
	type result struct {
		b   byte
		err error
	}
	// The inherited descriptor is a blocking one, which has no read deadline.
	// A read that outlives the timeout is abandoned: the caller exits.
	done := make(chan result, 1)
	go func() {
		var b [1]byte
		_, err := io.ReadFull(f, b[:])
		f.Close()
		done <- result{b[0], err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-done:
		switch {
		case errors.Is(r.err, io.EOF):
			return ErrClosed
		case r.err != nil:
			return fmt.Errorf("launch gate: %w", r.err)
		case r.b != open:
			return fmt.Errorf("%w %#x", ErrBadByte, r.b)
		}
		return nil
	case <-timer.C:
		return fmt.Errorf("%w (%s)", ErrTimeout, timeout)
	}
}

// SplitArgs takes a leading `--gate-fd N` off a helper's arguments (argv
// without the program name). fd is -1 when the helper was started without a
// gate. It serves the helper reading its own arguments and the daemon
// recognising a running helper by its argv.
func SplitArgs(args []string) (fd int, rest []string, err error) {
	if len(args) == 0 || args[0] != Flag {
		return -1, args, nil
	}
	if len(args) < 2 {
		return -1, nil, fmt.Errorf("%s needs a file descriptor", Flag)
	}
	n, perr := strconv.Atoi(args[1])
	if perr != nil || n < 3 {
		return -1, nil, fmt.Errorf("%s %q: want a descriptor number of 3 or more", Flag, args[1])
	}
	return n, args[2:], nil
}

// Gate is the daemon's end of the launch gate.
type Gate struct {
	r, w *os.File
}

// New makes a closed gate.
func New() (*Gate, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("launch gate: %w", err)
	}
	return &Gate{r: r, w: w}, nil
}

// Start starts cmd with the gate's read end as its last extra file and
// `--gate-fd N` ahead of its arguments, then lets go of the daemon's copy of
// that end. Until Open, the helper it starts can do nothing; Close (or the
// daemon's death) ends it. On error the gate is still the caller's to Close.
func (g *Gate) Start(cmd *exec.Cmd) error {
	fd := 3 + len(cmd.ExtraFiles) // os/exec numbers extra files from 3
	cmd.ExtraFiles = append(cmd.ExtraFiles, g.r)
	if len(cmd.Args) == 0 {
		cmd.Args = []string{cmd.Path}
	}
	args := make([]string, 0, len(cmd.Args)+2)
	args = append(args, cmd.Args[0], Flag, strconv.Itoa(fd))
	cmd.Args = append(args, cmd.Args[1:]...)
	err := cmd.Start()
	g.r.Close() // the child has its own; ours would only hide the daemon's death from it
	return err
}

// Open lets the helper go, and closes the gate. An error means the helper is
// already gone.
func (g *Gate) Open() error {
	_, err := g.w.Write([]byte{open})
	g.Close()
	if err != nil {
		return fmt.Errorf("open launch gate: %w", err)
	}
	return nil
}

// Close shuts the gate for good: a helper still waiting reads EOF and exits.
// Safe to call after Open and more than once.
func (g *Gate) Close() {
	g.w.Close()
	g.r.Close()
}
