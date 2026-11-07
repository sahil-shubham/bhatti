package main

import (
	"bytes"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/launchgate"
)

// gatedNetd is a real netd process started behind a launch gate.
type gatedNetd struct {
	cmd     *exec.Cmd
	sock    string
	stderr  bytes.Buffer
	done    chan struct{} // closed once the process has ended
	waitErr error         // cmd.Wait's result; valid after done is closed
}

// startNetd starts bin listening on a fresh socket. start launches the
// process: it wires a gate (Gate.Start) or a hand-made pipe, then runs cmd.Start.
func startNetd(t *testing.T, bin string, start func(cmd *exec.Cmd) error) *gatedNetd {
	t.Helper()
	// Unix socket paths top out near 100 bytes on macOS, which t.TempDir() exceeds.
	dir, err := os.MkdirTemp("/tmp", "bgate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	n := &gatedNetd{sock: filepath.Join(dir, "net.sock"), done: make(chan struct{})}
	n.cmd = exec.Command(bin, "--net-uds", n.sock)
	n.cmd.Stderr = &n.stderr
	if err := start(n.cmd); err != nil {
		t.Fatalf("start netd: %v", err)
	}
	go func() {
		n.waitErr = n.cmd.Wait()
		close(n.done)
	}()
	t.Cleanup(func() {
		n.cmd.Process.Kill()
		<-n.done
	})
	return n
}

// wait blocks until the process has ended and returns how it ended.
func (n *gatedNetd) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-n.done:
		return n.waitErr
	case <-time.After(10 * time.Second):
		t.Fatal("netd did not exit")
		return nil
	}
}

func (n *gatedNetd) running() bool {
	select {
	case <-n.done:
		return false
	default:
		return true
	}
}

func (n *gatedNetd) socketExists() bool {
	_, err := os.Stat(n.sock)
	return err == nil
}

// requireRefused asserts netd ended with a failure and never bound its socket.
func (n *gatedNetd) requireRefused(t *testing.T, wantStderr string) {
	t.Helper()
	err := n.wait(t)
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() == 0 {
		t.Fatalf("netd exit = %v, want a non-zero exit", err)
	}
	if n.socketExists() {
		t.Fatalf("netd bound %s although the gate never opened", n.sock)
	}
	if got := n.stderr.String(); !strings.Contains(got, wantStderr) {
		t.Fatalf("stderr = %q, want it to mention %q", got, wantStderr)
	}
}

// TestNetdLaunchGate runs the real netd binary: the daemon waits for netd's
// socket to appear, so the socket must not exist before the gate opens, and a
// gate that never opens must end netd without it having bound anything.
func TestNetdLaunchGate(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain to build netd")
	}
	bin := filepath.Join(t.TempDir(), "bhatti-netd")
	// Built the way `make netd` builds it; on Linux confine needs a cgo-free binary.
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build bhatti-netd: %v\n%s", err, out)
	}

	t.Run("closed without the byte", func(t *testing.T) {
		g, err := launchgate.New()
		if err != nil {
			t.Fatal(err)
		}
		defer g.Close()
		n := startNetd(t, bin, g.Start)
		g.Close() // the daemon failing to place netd
		n.requireRefused(t, "launch gate")
	})

	t.Run("wrong byte", func(t *testing.T) {
		// The opening byte is private to launchgate, so a wrong one needs a pipe of our own.
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		defer w.Close()
		n := startNetd(t, bin, func(cmd *exec.Cmd) error {
			cmd.Args = append([]string{cmd.Args[0], launchgate.Flag, "3"}, cmd.Args[1:]...)
			cmd.ExtraFiles = []*os.File{r}
			return cmd.Start()
		})
		if _, err := w.Write([]byte{'X'}); err != nil {
			t.Fatal(err)
		}
		n.requireRefused(t, "unexpected byte")
	})

	t.Run("opening byte", func(t *testing.T) {
		g, err := launchgate.New()
		if err != nil {
			t.Fatal(err)
		}
		defer g.Close()
		n := startNetd(t, bin, g.Start)

		// Were netd not held at the gate it would have bound long before this.
		time.Sleep(500 * time.Millisecond)
		if n.socketExists() {
			t.Fatal("netd bound its socket before the gate opened")
		}
		if !n.running() {
			err := n.wait(t)
			t.Fatalf("netd exited while the gate was shut: %v\nstderr: %s", err, n.stderr.String())
		}

		if err := g.Open(); err != nil {
			t.Fatalf("open gate: %v", err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			c, err := net.Dial("unix", n.sock)
			if err == nil {
				c.Close()
				break
			}
			if !n.running() {
				werr := n.wait(t)
				t.Fatalf("netd exited after the gate opened: %v\nstderr: %s", werr, n.stderr.String())
			}
			if time.Now().After(deadline) {
				t.Fatalf("netd never listened on %s after the gate opened: %v", n.sock, err)
			}
			time.Sleep(20 * time.Millisecond)
		}

		if err := n.cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		if err := n.wait(t); err == nil {
			t.Fatal("netd exited cleanly after a kill")
		}
	})
}
