//go:build linux

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// In a child process (Landlock can't be undone): after landlockReadOnly("/etc")
// it can still read under /etc but can't write anywhere, so a file it could
// create a moment earlier is now refused. The child enforces the ruleset on its
// one locked thread: the test binary links cgo (go test -race needs it), where
// AllThreadsSyscall is unavailable. netd's every-thread enforcement is covered
// by TestKrucibleNetdConfined against the real cgo-free binary.
func TestLandlockReadOnly(t *testing.T) {
	if dir := os.Getenv("NETD_LANDLOCK_CHILD"); dir != "" {
		runtime.LockOSThread() // the checks below must run on the confined thread
		// confine sets no_new_privs before Landlock; restrict_self requires it
		// for an unprivileged caller.
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			t.Fatalf("no_new_privs: %v", err)
		}
		thisThread := func(fd uintptr) syscall.Errno {
			_, _, errno := syscall.RawSyscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0)
			return errno
		}
		if err := landlockReadOnly("/etc", thisThread); err != nil {
			if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) {
				os.Exit(3)
			}
			t.Fatalf("landlockReadOnly: %v", err)
		}
		if _, err := os.ReadFile("/etc/hostname"); err != nil {
			t.Fatalf("read under /etc after confine: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "after"), []byte("x"), 0o600); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("write after confine: err=%v, want permission denied", err)
		}
		return
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "before"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run", "^TestLandlockReadOnly$", "-test.v")
	cmd.Env = append(os.Environ(), "NETD_LANDLOCK_CHILD="+dir)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 3 {
		t.Skip("kernel without Landlock")
	}
	if err != nil {
		t.Fatalf("confined child: %v\n%s", err, out)
	}
}
