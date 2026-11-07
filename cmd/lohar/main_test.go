//go:build linux

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// requireUnrestrictedCgroup skips the calling test unless we have direct
// write access to /sys/fs/cgroup — i.e. we're PID 1 on a real host, the
// agni-01 smoke environment, or a privileged developer machine. Returns
// without skipping in those cases; t.Skip's the test in container/K8s
// pod environments where cgroup-namespace isolation makes our
// cgroup.procs writes either silently fail or land in the wrong
// subtree.
//
// Detection: /proc/self/cgroup in cgroup v2 has one line "0::<path>".
// An unrestricted env has path "/" (we're the root cgroup, or PID 1
// inside a VM that owns the cgroup hierarchy). A nested env has
// something like "/kubepods.slice/...cri-containerd-....scope" — the
// ARC runner inside a K8s pod is the common case.
//
// This is a stricter check than "is root" or "has cgroup v2 mounted"
// (the existing t.Skip pattern in cgroup_test.go) and catches the
// case where root + cgroup v2 are both present but the namespace
// boundary prevents moves to /sys/fs/cgroup/system.slice/.
func requireUnrestrictedCgroup(t *testing.T) {
	t.Helper()
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		t.Skipf("cannot read /proc/self/cgroup: %v", err)
	}
	line := strings.TrimSpace(string(data))
	// cgroup v2 single line: "0::<path>"
	parts := strings.SplitN(line, "::", 2)
	if len(parts) != 2 {
		t.Skipf("unexpected /proc/self/cgroup format: %q", line)
	}
	if parts[1] != "/" {
		t.Skipf("requires unrestricted cgroup v2 (we're nested at %q — container or K8s pod). "+
			"This test runs on real hosts only; verify on agni-01.", parts[1])
	}
}

var guestExecProbe struct {
	once sync.Once
	err  error
}

// requireGuestExec skips the calling test unless this process can start a
// child as uid/gid 1000, which is how lohar runs every exec, session and init
// command. That takes CAP_SETUID and CAP_SETGID, so in practice root: CI runs
// the suite once unprivileged and again under sudo, where these tests run.
// Probed rather than inferred from the uid, so a root stripped of those
// capabilities skips too.
func requireGuestExec(t *testing.T) {
	t.Helper()
	guestExecProbe.once.Do(func() {
		cmd := exec.Command("/bin/true")
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000}}
		guestExecProbe.err = runTracked(cmd)
	})
	if guestExecProbe.err != nil {
		t.Skipf("cannot start a child as uid/gid 1000 (needs root): %v", guestExecProbe.err)
	}
}

// processGone reports whether pid is dead. A zombie counts as dead: where
// nothing reaps orphans (a container without an init, say) a killed process
// stays one indefinitely, and kill -0 still succeeds on it.
func processGone(pid int) bool {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return os.IsNotExist(err)
	}
	// "pid (comm) S ...": comm may hold spaces and parens, so the state
	// follows the last ')'.
	i := bytes.LastIndexByte(stat, ')')
	return i >= 0 && i+2 < len(stat) && (stat[i+2] == 'Z' || stat[i+2] == 'X')
}

func waitProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !processGone(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d still alive", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestMain wires the spawn-helper indirection so existing startDaemon
// tests (TestRestartOnFailure, TestStopSuppressesRestart, the cgroup
// integration tests, etc.) work in a test binary.
//
// The problem this solves: startDaemon spawns a subprocess via
// `/proc/self/exe spawn ...`. In production /proc/self/exe is lohar's
// binary and main() dispatches argv[1] to runSpawn. In a test binary,
// /proc/self/exe is the test binary, whose main is testing.M.Run \u2014
// the argv-verb dispatch in cmd/lohar/main.go is never reached, and
// the forked child re-runs the whole test suite, recursively forking
// itself until the test timeout fires.
//
// Two responsibilities here:
//
//  1. Subprocess dispatch. When this test binary is exec'd by a parent
//     test process with LOHAR_SPAWN_HELPER=1 in the env, route directly
//     to runSpawn before any test runs. This mirrors the argv[1]=spawn
//     dispatch in production's main().
//
//  2. Parent configuration. In the parent test process (no env var),
//     point startDaemon at the test binary itself (os.Args[0]) and tell
//     it to set LOHAR_SPAWN_HELPER=1 on the subprocess env, so the
//     subprocess hits branch (1).
//
// The "--helper-args" sentinel separates go test's own flags from the
// args we want runSpawn to see. Without it, "--cgroup" and the daemon
// argv would collide with the testing package's flag parser.
func TestMain(m *testing.M) {
	if os.Getenv("LOHAR_SPAWN_HELPER") == "1" {
		// Subprocess role. Find the "--helper-args" sentinel and pass
		// everything after it to runSpawn. runSpawn either Execs into
		// the daemon (PID preserved, never returns) or calls os.Exit
		// on failure; the os.Exit(99) below is a defensive marker for
		// "spawn unexpectedly returned" \u2014 should never fire.
		// Match production's main() which calls runSpawn(os.Args[2:]),
		// skipping both argv[0] (the binary path) and argv[1] ("spawn").
		// Our test invocation looks like:
		//   <testbin> --helper-args spawn --cgroup <path> -- <argv...>
		// so we strip up to and including "--helper-args", then strip
		// "spawn" if present.
		args := os.Args[1:]
		for i, a := range args {
			if a == "--helper-args" {
				args = args[i+1:]
				break
			}
		}
		if len(args) > 0 && args[0] == "spawn" {
			args = args[1:]
		}
		runSpawn(args)
		os.Exit(99)
	}

	// Parent role: tell startDaemon to invoke us-as-helper.
	spawnHelperPath = os.Args[0]
	spawnHelperPrefix = []string{"--helper-args"}
	spawnHelperEnv = []string{"LOHAR_SPAWN_HELPER=1"}

	os.Exit(m.Run())
}
