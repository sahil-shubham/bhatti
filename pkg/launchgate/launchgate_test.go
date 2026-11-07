package launchgate

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestMain lets this test binary stand in for a gated helper and for the
// daemon that starts it: the gate is a contract between two processes, so the
// tests that matter run it between two.
func TestMain(m *testing.M) {
	switch os.Getenv("LAUNCHGATE_ROLE") {
	case "helper":
		helperMain()
	case "daemon":
		daemonMain()
	}
	os.Exit(m.Run())
}

// helperMain is a helper that reads its gate first, as bhatti-vmm and
// bhatti-netd do, and only then does "work": writes <marker>. A helper whose
// gate fails records why in <marker>.failed and exits 3.
//
//	helper <marker> <timeout>
func helperMain() {
	fd, rest, err := SplitArgs(os.Args[1:])
	if err != nil || fd < 0 || len(rest) != 2 {
		fmt.Fprintln(os.Stderr, "helper: usage: --gate-fd N <marker> <timeout>", err)
		os.Exit(4)
	}
	marker, timeout := rest[0], rest[1]
	d, _ := time.ParseDuration(timeout)
	if err := Wait(fd, d); err != nil {
		_ = os.WriteFile(marker+".failed", []byte(err.Error()), 0o600)
		os.Exit(3)
	}
	_ = os.WriteFile(marker, []byte("worked"), 0o600)
	os.Exit(0)
}

// daemonMain starts a helper behind a gate, prints its pid and never opens the
// gate: the test kills this process to stand for a daemon that dies mid-launch.
func daemonMain() {
	g, err := New()
	if err != nil {
		os.Exit(2)
	}
	cmd := exec.Command(os.Args[0], os.Getenv("LAUNCHGATE_MARKER"), "1m")
	cmd.Env = append(os.Environ(), "LAUNCHGATE_ROLE=helper")
	if err := g.Start(cmd); err != nil {
		os.Exit(2)
	}
	fmt.Println(cmd.Process.Pid)
	// A gate nobody references is closed by its files' finalizers, which
	// would stand for a daemon that gave up, not one that is still deciding.
	for {
		time.Sleep(time.Hour)
		runtime.KeepAlive(g)
	}
}

// startHelper starts the stand-in helper behind a new gate.
func startHelper(t *testing.T, marker, timeout string) (*Gate, *exec.Cmd, <-chan error) {
	t.Helper()
	g, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	cmd := exec.Command(os.Args[0], marker, timeout)
	cmd.Env = append(os.Environ(), "LAUNCHGATE_ROLE=helper")
	if err := g.Start(cmd); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return g, cmd, done
}

func exitCode(t *testing.T, done <-chan error) int {
	t.Helper()
	select {
	case err := <-done:
		var exit *exec.ExitError
		switch {
		case err == nil:
			return 0
		case errors.As(err, &exit):
			return exit.ExitCode()
		default:
			t.Fatalf("wait: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("helper still running")
	}
	return -1
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestHelperWorksOnlyAfterTheGateOpens: the helper's work happens after, and
// only after, the daemon opens the gate.
func TestHelperWorksOnlyAfterTheGateOpens(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "work")
	g, _, done := startHelper(t, marker, "1m")

	time.Sleep(200 * time.Millisecond) // long enough for a helper that ignored the gate to finish
	if exists(marker) {
		t.Fatal("helper did work while the gate was shut")
	}
	select {
	case err := <-done:
		t.Fatalf("helper exited with the gate shut: %v", err)
	default:
	}

	if err := g.Open(); err != nil {
		t.Fatal(err)
	}
	if code := exitCode(t, done); code != 0 {
		t.Fatalf("helper exit %d after the gate opened", code)
	}
	if !exists(marker) {
		t.Fatal("helper never worked after the gate opened")
	}
}

// TestHelperExitsBeforeWorkWhenTheGateFails: every way the gate can fail ends
// the helper, non-zero, with its work undone.
func TestHelperExitsBeforeWorkWhenTheGateFails(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout string
		fail    func(g *Gate)
		reason  error
	}{
		{"daemon closes the gate", "1m", func(g *Gate) { g.Close() }, ErrClosed},
		{"wrong byte", "1m", func(g *Gate) { g.w.Write([]byte("X")) }, ErrBadByte},
		{"timeout", "150ms", func(*Gate) {}, ErrTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "work")
			g, _, done := startHelper(t, marker, tc.timeout)
			tc.fail(g)
			if code := exitCode(t, done); code != 3 {
				t.Fatalf("exit %d, want 3 (the gate failed)", code)
			}
			if exists(marker) {
				t.Fatal("helper worked although its gate failed")
			}
			why, _ := os.ReadFile(marker + ".failed")
			if want := tc.reason.Error(); !strings.Contains(string(why), want) {
				t.Fatalf("helper reported %q, want it to say %q", why, want)
			}
		})
	}
}

// TestHelperExitsWhenTheDaemonDies: a daemon killed between starting a helper
// and opening its gate takes the helper's pass with it: its end of the pipe
// closes with it, and the helper exits instead of running unplaced.
func TestHelperExitsWhenTheDaemonDies(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "work")
	daemon := exec.Command(os.Args[0])
	daemon.Env = append(os.Environ(), "LAUNCHGATE_ROLE=daemon", "LAUNCHGATE_MARKER="+marker)
	out, err := daemon.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = daemon.Process.Kill(); _ = daemon.Wait() }()
	var pid int
	if _, err := fmt.Fscan(out, &pid); err != nil {
		t.Fatalf("daemon did not report its helper: %v", err)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)

	time.Sleep(100 * time.Millisecond)
	if exists(marker) || exists(marker+".failed") {
		t.Fatal("helper acted while its daemon was alive and the gate shut")
	}
	_ = daemon.Process.Kill()
	_, _ = daemon.Process.Wait()

	deadline := time.Now().Add(10 * time.Second)
	for !exists(marker + ".failed") {
		if time.Now().After(deadline) {
			t.Fatal("helper still waiting after its daemon died")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if exists(marker) {
		t.Fatal("helper worked although its daemon died before opening the gate")
	}
}

// TestWait covers the helper's read without a second process.
func TestWait(t *testing.T) {
	for _, tc := range []struct {
		name    string
		write   string
		close   bool
		timeout time.Duration
		want    error
	}{
		{"opening byte", "G", false, time.Second, nil},
		{"closed", "", true, time.Second, ErrClosed},
		{"other byte", "g", false, time.Second, ErrBadByte},
		{"nothing", "", false, 50 * time.Millisecond, ErrTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer w.Close()
			fd, err := syscall.Dup(int(r.Fd())) // Wait owns the descriptor it is given
			if err != nil {
				t.Fatal(err)
			}
			r.Close()
			if tc.write != "" {
				w.WriteString(tc.write)
			}
			if tc.close {
				w.Close()
			}
			if err := Wait(fd, tc.timeout); !errors.Is(err, tc.want) {
				t.Fatalf("Wait = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestWaitClosesTheDescriptor: once the helper has its byte, the pipe isn't
// left open in it.
func TestWaitClosesTheDescriptor(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	fd, err := syscall.Dup(int(r.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	w.WriteString("G")
	if err := Wait(fd, time.Second); err != nil {
		t.Fatal(err)
	}
	// The read goroutine closes it just after the send Wait returned on.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); errors.Is(err, syscall.EBADF) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("descriptor %d still open after Wait", fd)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSplitArgs(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		fd      int
		rest    []string
		wantErr bool
	}{
		{[]string{"spec.json"}, -1, []string{"spec.json"}, false},
		{nil, -1, nil, false},
		{[]string{"--gate-fd", "3", "spec.json"}, 3, []string{"spec.json"}, false},
		{[]string{"--gate-fd", "7", "--net-uds", "/x"}, 7, []string{"--net-uds", "/x"}, false},
		{[]string{"--gate-fd"}, -1, nil, true},
		{[]string{"--gate-fd", "x", "spec.json"}, -1, nil, true},
		{[]string{"--gate-fd", "2", "spec.json"}, -1, nil, true},                                      // stdio is never the gate
		{[]string{"spec.json", "--gate-fd", "3"}, -1, []string{"spec.json", "--gate-fd", "3"}, false}, // only a leading flag counts
	} {
		fd, rest, err := SplitArgs(tc.args)
		if (err != nil) != tc.wantErr || fd != tc.fd || (err == nil && !slices.Equal(rest, tc.rest)) {
			t.Errorf("SplitArgs(%q) = %d, %q, %v; want %d, %q, error=%v", tc.args, fd, rest, err, tc.fd, tc.rest, tc.wantErr)
		}
	}
}

// TestStartNamesTheDescriptor: the helper is told which descriptor holds the
// gate, and extra files already on the command keep theirs.
func TestStartNamesTheDescriptor(t *testing.T) {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	for _, tc := range []struct {
		extra int
		want  string
	}{{0, "3"}, {1, "4"}} {
		g, err := New()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("true", "spec.json")
		for range tc.extra {
			cmd.ExtraFiles = append(cmd.ExtraFiles, devnull)
		}
		if err := g.Start(cmd); err != nil {
			t.Fatal(err)
		}
		want := []string{"true", Flag, tc.want, "spec.json"}
		if !slices.Equal(cmd.Args, want) || len(cmd.ExtraFiles) != tc.extra+1 {
			t.Errorf("%d extra files: args %q, %d files; want %q, %d files", tc.extra, cmd.Args, len(cmd.ExtraFiles), want, tc.extra+1)
		}
		_ = cmd.Wait()
		g.Close()
		if fd, rest, err := SplitArgs(cmd.Args[1:]); err != nil || fd != 3+tc.extra || !slices.Equal(rest, []string{"spec.json"}) {
			t.Errorf("SplitArgs of what Start wrote: %d %q %v", fd, rest, err)
		}
	}
}

// TestOpenAfterTheHelperDied: a helper that is already gone is reported, not
// papered over.
func TestOpenAfterTheHelperDied(t *testing.T) {
	g, err := New()
	if err != nil {
		t.Fatal(err)
	}
	g.r.Close() // no reader left: what the daemon sees once the helper has exited
	if err := g.Open(); err == nil || !errors.Is(err, syscall.EPIPE) {
		t.Fatalf("Open with no reader = %v, want EPIPE", err)
	}
}
