//go:build linux

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sahil-shubham/bhatti/pkg/agent/proto"
	"golang.org/x/sys/unix"
)

// Run the blanket-wait4 mode in its own process so it cannot steal the
// unrelated test suite's exec.Cmd waits. PR_SET_CHILD_SUBREAPER gives this
// process PID 1's responsibility for children orphaned by shell commands.
func TestChildReaperLifecycle(t *testing.T) {
	if os.Getenv("LOHAR_REAPER_TEST") == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestChildReaperLifecycle$", "-test.v")
		cmd.Env = append(os.Environ(), "LOHAR_REAPER_TEST=1")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("subreaper tests: %v\n%s", err, output)
		}
		t.Logf("subreaper tests:\n%s", output)
		return
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatalf("become child subreaper: %v", err)
	}
	childReaper.reapAll = true
	startChildReaper()

	t.Run("orphans and clean units", func(t *testing.T) {
		// This shell exits without waiting for its background child. The
		// subreaper adopts it and must remove its zombie after it exits.
		out, err := combinedOutputTracked(exec.Command("/bin/sh", "-c", "sleep 0.01 & echo $!"))
		if err != nil {
			t.Fatal(err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
		if err != nil {
			t.Fatalf("orphan PID %q: %v", out, err)
		}
		dir, reg := testReaperRegistry(t)
		if err := os.WriteFile(filepath.Join(dir, "clean.service"), []byte("[Service]\nType=simple\nExecStart=/bin/true\nRestart=no\n"), 0644); err != nil {
			t.Fatal(err)
		}
		u, err := reg.Resolve("clean")
		if err != nil {
			t.Fatal(err)
		}
		// Both synchronous service paths previously used Cmd.Run, which
		// could also lose its exit status to the PID-1 blanket reaper.
		for _, svc := range []struct{ name, typ string }{
			{"oneshot", "oneshot"},
			{"forking", "forking"},
		} {
			unit := fmt.Sprintf("[Service]\nType=%s\nExecStart=/bin/true\n", svc.typ)
			if err := os.WriteFile(filepath.Join(dir, svc.name+".service"), []byte(unit), 0644); err != nil {
				t.Fatal(err)
			}
		}
		oneshot, err := reg.Resolve("oneshot")
		if err != nil {
			t.Fatal(err)
		}
		forking, err := reg.Resolve("forking")
		if err != nil {
			t.Fatal(err)
		}
		churn := make(chan error, 1)
		go func() {
			for range 80 {
				if err := runTracked(exec.Command("/bin/sh", "-c", "sleep 0.01 &")); err != nil {
					churn <- err
					return
				}
			}
			churn <- nil
		}()
		for i := range 80 {
			if err := startDaemon(u, "/bin/true", u.Sections); err != nil {
				t.Fatal(err)
			}
			reg.WaitForWatchers()
			if u.IsFailed() || u.LastExitCode() != 0 {
				t.Fatalf("clean exit %d marked failed (exit %d)", i, u.LastExitCode())
			}
			if i < 8 {
				if err := svcStart(oneshot); err != nil {
					t.Fatalf("oneshot %d: %v", i, err)
				}
				if err := svcStart(forking); err != nil {
					t.Fatalf("forking %d: %v", i, err)
				}
			}
		}
		if err := <-churn; err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); os.IsNotExist(err) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("orphan %d was not reaped", pid)
	})

	for _, tc := range []struct {
		name, policy, command string
		lastCode              int
		failed                bool
	}{
		{"on-abnormal SIGKILL", "on-abnormal", "kill -KILL $$", 137, true},
		{"on-success exit 0", "on-success", "exit 0", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, reg := testReaperRegistry(t)
			count := filepath.Join(dir, "attempts")
			unit := fmt.Sprintf("[Service]\nType=simple\nExecStart=/bin/sh -c 'echo attempt >> %s; %s'\nRestart=%s\nRestartSec=1ms\nStartLimitBurst=1\nStartLimitIntervalSec=10\n", count, tc.command, tc.policy)
			if err := os.WriteFile(filepath.Join(dir, "restart.service"), []byte(unit), 0644); err != nil {
				t.Fatal(err)
			}
			u, err := reg.Resolve("restart")
			if err != nil {
				t.Fatal(err)
			}
			if err := svcStart(u); err != nil {
				t.Fatal(err)
			}
			reg.WaitForWatchers()
			attempts, err := os.ReadFile(count)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Count(string(attempts), "attempt\n"); got != 2 {
				t.Errorf("%s started %d times, want 2", tc.policy, got)
			}
			if u.IsFailed() != tc.failed || u.LastExitCode() != tc.lastCode {
				t.Errorf("%s: failed=%v exit=%d, want failed=%v exit=%d", tc.policy, u.IsFailed(), u.LastExitCode(), tc.failed, tc.lastCode)
			}
		})
	}

	if os.Geteuid() != 0 && os.Geteuid() != 1000 {
		t.Skip("exec handlers run processes with uid 1000")
	}
	for _, tc := range []struct {
		name, shell string
		want        int32
	}{
		{"success", "exit 0", 0},
		{"failure", "exit 3", 3},
		{"signal", "kill -TERM $$", 143},
	} {
		for _, handler := range []struct {
			name string
			fn   func(net.Conn, proto.ExecRequest)
		}{
			{"exec", handlePipedExec},
			{"piped session", handlePipedSession},
			{"PTY session", handleTTYSession},
		} {
			t.Run(handler.name+" "+tc.name, func(t *testing.T) {
				host, guest := net.Pipe()
				defer host.Close()
				defer guest.Close()
				host.SetDeadline(time.Now().Add(4 * time.Second))
				ended := make(chan struct{})
				go func() {
					handler.fn(guest, proto.ExecRequest{Argv: []string{"/bin/sh", "-c", tc.shell}})
					close(ended)
				}()
				for {
					kind, payload, err := proto.ReadFrame(host)
					if err != nil {
						t.Fatalf("host frame: %v", err)
					}
					if kind == proto.ERROR {
						t.Fatalf("exec error: %s", payload)
					}
					if kind == proto.EXIT {
						code, ok := proto.ParseExitCode(payload)
						if !ok || code != tc.want {
							t.Errorf("EXIT frame code=%d (valid=%v), want %d", code, ok, tc.want)
						}
						host.Close()
						select {
						case <-ended:
						case <-time.After(time.Second):
							t.Fatal("exec handler did not return after disconnect")
						}
						return
					}
				}
			})
		}
	}
}

func testReaperRegistry(t *testing.T) (string, *Registry) {
	t.Helper()
	dir := t.TempDir()
	reg := NewRegistry(Config{
		ServiceDirs: []string{dir},
		PidDir:      filepath.Join(dir, "pids"),
		LogDir:      filepath.Join(dir, "logs"),
		CgroupRoot:  filepath.Join(dir, "cgroup"),
	})
	t.Cleanup(reg.WaitForWatchers)
	return dir, reg
}
